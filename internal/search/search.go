// Package search 实现多源并发聚合搜索、流式事件与两级缓存。
package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"cilicili/internal/cache"
	"cilicili/internal/cms"
	"cilicili/internal/filter"
	"cilicili/internal/model"
	"cilicili/internal/source"
	"cilicili/internal/upstream"
)

// Config 是搜索器的运行参数。
type Config struct {
	MaxPages      int
	SourceTimeout time.Duration
	PageTimeout   time.Duration
	SoftDeadline  time.Duration
	HardDeadline  time.Duration
	CacheFresh    time.Duration
	CacheStale    time.Duration
	SourceFresh   time.Duration
	SourceStale   time.Duration
	FailCacheTTL  time.Duration
	UserAgent     string
	FastSources   int
}

// Searcher 聚合搜索器。
type Searcher struct {
	client upstream.Doer
	cache  cache.Store
	health source.HealthStore
	log    *slog.Logger
	cfg    Config
	sf     singleflight.Group
}

// New 创建搜索器。
func New(client upstream.Doer, store cache.Store, health source.HealthStore, log *slog.Logger, cfg Config) *Searcher {
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = 5
	}
	if cfg.SourceTimeout <= 0 {
		cfg.SourceTimeout = 6 * time.Second
	}
	if cfg.PageTimeout <= 0 {
		cfg.PageTimeout = 5 * time.Second
	}
	if cfg.SoftDeadline <= 0 {
		cfg.SoftDeadline = 1200 * time.Millisecond
	}
	if cfg.HardDeadline <= 0 {
		cfg.HardDeadline = 6 * time.Second
	}
	if cfg.CacheFresh <= 0 {
		cfg.CacheFresh = 5 * time.Minute
	}
	if cfg.CacheStale <= 0 {
		cfg.CacheStale = time.Hour
	}
	if cfg.SourceFresh <= 0 {
		cfg.SourceFresh = 30 * time.Minute
	}
	if cfg.SourceStale <= 0 {
		cfg.SourceStale = 6 * time.Hour
	}
	if cfg.FailCacheTTL <= 0 {
		cfg.FailCacheTTL = time.Minute
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"
	}
	if cfg.FastSources <= 0 {
		cfg.FastSources = 8
	}
	return &Searcher{client: client, cache: store, health: health, log: log, cfg: cfg}
}

// Search 是非流式搜索：命中缓存立即返回；未命中则等待硬截止。
func (s *Searcher) Search(ctx context.Context, wd string, sources []model.SourceConfig, filterAdult bool) (model.SearchResponse, error) {
	wd = strings.TrimSpace(wd)
	if err := validateInput(wd, sources); err != nil {
		return model.SearchResponse{}, err
	}
	key := searchCacheKey(wd, sources, filterAdult)

	if raw, ok := s.cache.Get(key); ok {
		if resp, err := decodeResponse(raw); err == nil {
			return resp, nil
		}
	}
	if raw, fresh, found := s.cache.GetStale(key); found {
		if !fresh {
			s.refreshAsync(wd, sources, filterAdult, key)
		}
		if resp, err := decodeResponse(raw); err == nil {
			return resp, nil
		}
	}

	base := context.WithoutCancel(ctx)
	v, err, _ := s.sf.Do(key, func() (any, error) {
		// 双重检查：并发等待者可能在别人写入后才醒来。
		if raw, ok := s.cache.Get(key); ok {
			if resp, err := decodeResponse(raw); err == nil {
				return resp, nil
			}
		}
		runCtx, cancel := context.WithTimeout(base, s.cfg.HardDeadline)
		defer cancel()
		outcomes := s.runAll(runCtx, sources, wd)
		resp := s.aggregate(outcomes, wd, filterAdult)
		s.storeSearch(key, resp)
		return resp, nil
	})
	if err != nil {
		return model.SearchResponse{}, err
	}
	resp, _ := v.(model.SearchResponse)
	return resp, nil
}

// Stream 是流式搜索：逐源回调 + 软截止 partial_done + 最终 done。
// emit 由调用方串行调用（本函数保证不并发调用 emit）。
func (s *Searcher) Stream(
	ctx context.Context,
	wd string,
	sources []model.SourceConfig,
	filterAdult bool,
	emit func(any) error,
) (model.SearchResponse, error) {
	wd = strings.TrimSpace(wd)
	if err := validateInput(wd, sources); err != nil {
		return model.SearchResponse{}, err
	}
	key := searchCacheKey(wd, sources, filterAdult)

	if raw, ok := s.cache.Get(key); ok {
		if resp, err := decodeResponse(raw); err == nil {
			if err := emit(model.DoneEvent{Type: "done", List: resp.List, Failures: resp.Failures}); err != nil {
				return resp, err
			}
			return resp, nil
		}
	}
	if raw, fresh, found := s.cache.GetStale(key); found {
		if resp, err := decodeResponse(raw); err == nil {
			if !fresh {
				s.refreshAsync(wd, sources, filterAdult, key)
			}
			if err := emit(model.DoneEvent{Type: "done", List: resp.List, Failures: resp.Failures}); err != nil {
				return resp, err
			}
			return resp, nil
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, s.cfg.HardDeadline)
	defer cancel()

	outcomes := make([]model.SourceSearchOutcome, len(sources))
	ch := make(chan sourceResult, len(sources))
	var wg sync.WaitGroup
	for i, src := range sources {
		wg.Add(1)
		go func(i int, src model.SourceConfig) {
			defer wg.Done()
			ch <- sourceResult{index: i, outcome: s.searchSource(runCtx, src, wd)}
		}(i, src)
	}
	go func() { wg.Wait(); close(ch) }()

	soft := time.NewTimer(s.cfg.SoftDeadline)
	defer soft.Stop()
	softSent := false
	remaining := len(sources)

	var emitErr error
	for remaining > 0 {
		select {
		case r, ok := <-ch:
			if !ok {
				remaining = 0
				break
			}
			outcomes[r.index] = r.outcome
			remaining--
			if emitErr == nil {
				emitErr = emit(model.SourceEvent{Type: "source", SourceSearchOutcome: r.outcome})
				if emitErr != nil {
					cancel()
				}
			}
		case <-soft.C:
			if !softSent {
				softSent = true
				if emitErr == nil {
					part := s.aggregate(outcomes, wd, filterAdult)
					emitErr = emit(model.PartialDoneEvent{
						Type: "partial_done", List: part.List, Failures: part.Failures, Partial: true,
					})
				}
			}
		case <-runCtx.Done():
			// 硬截止：取消在途请求，给 goroutine 一点时间把结果写回（带缓冲，不会阻塞）。
			remaining = 0
		}
	}
	cancel()
	wg.Wait()

	// wg.Wait 之后 channel 一定已关闭；这里收集硬截止前写回的结果。
	for r := range ch {
		outcomes[r.index] = r.outcome
	}

	resp := s.aggregate(outcomes, wd, filterAdult)
	// 只有完整跑完（没有因硬截止丢结果）才写最终缓存，避免把残缺结果缓存 5 分钟。
	if !hasUnsettled(outcomes, sources) {
		s.storeSearch(key, resp)
	}
	if emitErr == nil {
		emitErr = emit(model.DoneEvent{Type: "done", List: resp.List, Failures: resp.Failures})
	}
	return resp, emitErr
}

type sourceResult struct {
	index   int
	outcome model.SourceSearchOutcome
}

func (s *Searcher) runAll(ctx context.Context, sources []model.SourceConfig, wd string) []model.SourceSearchOutcome {
	outcomes := make([]model.SourceSearchOutcome, len(sources))
	var wg sync.WaitGroup
	for i, src := range sources {
		wg.Add(1)
		go func(i int, src model.SourceConfig) {
			defer wg.Done()
			outcomes[i] = s.searchSource(ctx, src, wd)
		}(i, src)
	}
	wg.Wait()
	return outcomes
}

func (s *Searcher) searchSource(ctx context.Context, src model.SourceConfig, wd string) model.SourceSearchOutcome {
	start := time.Now()
	out := model.SourceSearchOutcome{SourceKey: src.Key, List: []model.SearchResultItem{}}

	if s.health != nil && s.health.IsOpen(src.Key, time.Now()) {
		out.Error = "源已被服务端熔断，暂时跳过"
		return out
	}

	srcKey := sourceCacheKey(src, wd)
	if raw, ok := s.cache.Get(srcKey); ok {
		if cached, err := decodeOutcome(raw); err == nil {
			cached.MS = time.Since(start).Milliseconds()
			return cached
		}
	}

	srcCtx, cancel := context.WithTimeout(ctx, s.cfg.SourceTimeout)
	defer cancel()

	first, pageCount, err := s.fetchPage(srcCtx, src, wd, 1)
	if err != nil {
		return s.fail(src, wd, &out, err, start)
	}
	if pageCount < 1 {
		pageCount = 1
	}
	items := first
	if pageCount > 1 {
		pages := pageCount - 1
		if pages > s.cfg.MaxPages-1 {
			pages = s.cfg.MaxPages - 1
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for p := 2; p <= pages+1; p++ {
			wg.Add(1)
			go func(page int) {
				defer wg.Done()
				list, _, err := s.fetchPage(srcCtx, src, wd, page)
				if err != nil {
					return
				}
				mu.Lock()
				items = append(items, list...)
				mu.Unlock()
			}(p)
		}
		wg.Wait()
	}

	out.OK = true
	out.List = items
	out.MS = time.Since(start).Milliseconds()
	if s.health != nil {
		s.health.RecordSuccess(src.Key, out.MS, time.Now())
	}
	raw, _ := json.Marshal(out)
	s.cache.Set(srcKey, raw, s.cfg.SourceFresh, s.cfg.SourceStale)
	return out
}

func (s *Searcher) fail(src model.SourceConfig, wd string, out *model.SourceSearchOutcome, err error, start time.Time) model.SourceSearchOutcome {
	timedOut := isTimeout(err)
	out.OK = false
	out.TimedOut = timedOut
	out.Error = sanitizeError(err, timedOut)
	if s.log != nil {
		s.log.Debug("source search failed", "source", src.Key, "err", err)
	}
	out.MS = time.Since(start).Milliseconds()
	if s.health != nil {
		s.health.RecordFailure(src.Key, timedOut, time.Now())
	}
	if raw, mErr := json.Marshal(out); mErr == nil {
		s.cache.Set(sourceCacheKey(src, wd), raw, s.cfg.FailCacheTTL, s.cfg.FailCacheTTL)
	}
	return *out
}

// fetchPage 取某一页。返回值 pageCount 只在第 1 页有意义。
func (s *Searcher) fetchPage(ctx context.Context, src model.SourceConfig, wd string, page int) ([]model.SearchResultItem, int, error) {
	api := strings.TrimRight(src.URL, "/")
	sep := "?"
	if strings.Contains(api, "?") {
		sep = "&"
	}
	api += sep + "ac=videolist&wd=" + url.QueryEscape(wd) + "&pg=" + strconv.Itoa(page)

	res, err := s.client.Do(ctx, api, upstream.Options{
		Headers:       cms.RequestHeaders(s.cfg.UserAgent),
		Timeout:       s.cfg.PageTimeout,
		MaxBytes:      16 << 20,
		Retries:       1,
		SafeRedirects: true,
	})
	if err != nil {
		return nil, 0, err
	}
	defer res.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, 0, &upstream.StatusError{StatusCode: res.StatusCode, URL: api}
	}
	data, err := res.ReadAll(16 << 20)
	if err != nil {
		return nil, 0, err
	}
	return cms.ParseSearchList(data, src)
}

func (s *Searcher) aggregate(outcomes []model.SourceSearchOutcome, wd string, filterAdult bool) model.SearchResponse {
	seen := make(map[string]struct{})
	list := make([]model.SearchResultItem, 0, len(outcomes)*8)
	failures := make([]model.SearchFailure, 0)
	for _, o := range outcomes {
		if !o.OK {
			if o.SourceKey != "" {
				failures = append(failures, model.SearchFailure{
					SourceKey: o.SourceKey,
					Error:     firstNonEmpty(o.Error, "请求失败"),
					TimedOut:  o.TimedOut,
				})
			}
			continue
		}
		for _, item := range o.List {
			key := item.SourceKey + "_" + item.VodID
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			list = append(list, item)
		}
	}
	list = filter.Adult(list, filterAdult)
	list = filter.Relevant(list, wd)
	filter.Sort(list, wd)
	if list == nil {
		list = []model.SearchResultItem{}
	}
	return model.SearchResponse{List: list, Failures: failures}
}

func (s *Searcher) storeSearch(key string, resp model.SearchResponse) {
	raw, err := json.Marshal(resp)
	if err != nil {
		return
	}
	ttl := s.cfg.CacheFresh
	if len(resp.Failures) > 0 {
		// 有失败/超时的结果只缓存很短时间，给源站恢复留机会。
		ttl = s.cfg.FailCacheTTL
	}
	s.cache.Set(key, raw, ttl, s.cfg.CacheStale)
}

func (s *Searcher) refreshAsync(wd string, sources []model.SourceConfig, filterAdult bool, key string) {
	go func() {
		_, _, _ = s.sf.Do("refresh:"+key, func() (any, error) {
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.HardDeadline)
			defer cancel()
			outcomes := s.runAll(ctx, sources, wd)
			s.storeSearch(key, s.aggregate(outcomes, wd, filterAdult))
			return nil, nil
		})
	}()
}

// Warm 在启动时后台探测一遍源：预热连接池、填充健康状态。
// 对失败的源会隔 1.5s 再确认一次（连续两次失败才触发熔断），避免瞬时抖动误伤。

// SelectFast 按服务端健康状态挑选最快的 N 个源。用于 "fast" 搜索模式：
// 默认只查最快的 8 个源，把全量源留给用户显式发起 "搜索全部源"。
// 排序依据：熔断等级 → EWMA 延迟 → 连续失败数 → 原始顺序（稳定）。
func (s *Searcher) SelectFast(sources []model.SourceConfig, n int) []model.SourceConfig {
	if n <= 0 || len(sources) <= n {
		return sources
	}
	type ranked struct {
		src   model.SourceConfig
		level int
		ewma  float64
		fails int
		order int
	}
	rankedList := make([]ranked, 0, len(sources))
	for i, src := range sources {
		r := ranked{src: src, order: i, ewma: 1 << 30}
		if s.health != nil {
			if h, ok := s.health.Get(src.Key); ok {
				r.level = h.Level
				r.fails = h.FailStreak
				if h.EWMAms > 0 {
					r.ewma = h.EWMAms
				}
			}
		}
		rankedList = append(rankedList, r)
	}
	sort.SliceStable(rankedList, func(i, j int) bool {
		a, b := rankedList[i], rankedList[j]
		if a.level != b.level {
			return a.level < b.level
		}
		if a.ewma != b.ewma {
			return a.ewma < b.ewma
		}
		if a.fails != b.fails {
			return a.fails < b.fails
		}
		return a.order < b.order
	})
	out := make([]model.SourceConfig, 0, n)
	for i := 0; i < n && i < len(rankedList); i++ {
		out = append(out, rankedList[i].src)
	}
	return out
}

func (s *Searcher) Warm(ctx context.Context, sources []model.SourceConfig) {
	if len(sources) == 0 {
		return
	}
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for _, src := range sources {
		wg.Add(1)
		go func(src model.SourceConfig) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if s.warmOne(ctx, src) {
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(1500 * time.Millisecond):
				s.warmOne(ctx, src)
			}
		}(src)
	}
	wg.Wait()
}

// warmOne 探测一个源，返回是否成功。结果只记入健康状态，不写搜索结果缓存。
func (s *Searcher) warmOne(ctx context.Context, src model.SourceConfig) bool {
	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, s.cfg.PageTimeout)
	defer cancel()
	if s.health != nil && s.health.IsOpen(src.Key, time.Now()) {
		return true
	}
	_, _, err := s.fetchPage(probeCtx, src, "庆余年", 1)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		if s.health != nil {
			s.health.RecordFailure(src.Key, isTimeout(err), time.Now())
		}
		if s.log != nil {
			s.log.Debug("warm source failed", "source", src.Key, "err", err)
		}
		return false
	}
	if s.health != nil {
		s.health.RecordSuccess(src.Key, ms, time.Now())
	}
	return true
}

func validateInput(wd string, sources []model.SourceConfig) error {
	if wd == "" {
		return errors.New("搜索关键词无效")
	}
	if len([]rune(wd)) > 100 {
		return errors.New("搜索关键词过长")
	}
	if len(sources) == 0 {
		return errors.New("请至少选择一个点播源")
	}
	if len(sources) > 50 {
		return errors.New("点播源数量过多")
	}
	return nil
}

func searchCacheKey(wd string, sources []model.SourceConfig, filterAdult bool) string {
	urls := make([]string, 0, len(sources))
	for _, s := range sources {
		urls = append(urls, strings.TrimRight(s.URL, "/"))
	}
	sort.Strings(urls)
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%t\n%s", strings.ToLower(strings.TrimSpace(wd)), filterAdult, strings.Join(urls, "|"))
	return "search:" + hex.EncodeToString(h.Sum(nil))[:32]
}

func sourceCacheKey(src model.SourceConfig, wd string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s", strings.TrimRight(src.URL, "/"), src.Detail, strings.ToLower(strings.TrimSpace(wd)))
	return "src:" + hex.EncodeToString(h.Sum(nil))[:32]
}

func decodeResponse(raw []byte) (model.SearchResponse, error) {
	var resp model.SearchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return model.SearchResponse{}, err
	}
	if resp.List == nil {
		resp.List = []model.SearchResultItem{}
	}
	if resp.Failures == nil {
		resp.Failures = []model.SearchFailure{}
	}
	return resp, nil
}

func decodeOutcome(raw []byte) (model.SourceSearchOutcome, error) {
	var out model.SourceSearchOutcome
	if err := json.Unmarshal(raw, &out); err != nil {
		return model.SourceSearchOutcome{}, err
	}
	if out.List == nil {
		out.List = []model.SearchResultItem{}
	}
	return out, nil
}

func hasUnsettled(outcomes []model.SourceSearchOutcome, sources []model.SourceConfig) bool {
	for i := range sources {
		if outcomes[i].SourceKey == "" {
			return true
		}
	}
	return false
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") || strings.Contains(msg, "timed out") ||
		strings.Contains(msg, "deadline exceeded")
}

// sanitizeError 把底层网络错误折叠成稳定、无敏感信息的用户可见文案。
// 完整错误只写日志，避免把内网 IP / 完整 URL / 查询关键词回显给客户端。
func sanitizeError(err error, timedOut bool) string {
	if timedOut {
		return "请求超时"
	}
	var se *upstream.StatusError
	if errors.As(err, &se) {
		return fmt.Sprintf("HTTP %d", se.StatusCode)
	}
	if errors.Is(err, upstream.ErrBlocked) {
		return "目标地址不在允许范围内"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection reset"):
		return "连接被重置"
	case strings.Contains(msg, "connection refused"):
		return "连接被拒绝"
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "name resolution"):
		return "域名解析失败"
	case strings.Contains(msg, "tls"):
		return "TLS 握手失败"
	case strings.Contains(msg, "context canceled"):
		return "请求已取消"
	default:
		return "请求失败"
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
