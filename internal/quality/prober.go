package quality

// 本文件实现画质探测的编排层：默认参数、结果缓存、singleflight 合并与
// ProbeMany 的并发/限流/串行 emit。解析细节见 playlist.go / codec.go，
// 单条探测流程见 episode.go。

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
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"cilicili/internal/cache"
	"cilicili/internal/cms"
	"cilicili/internal/model"
	"cilicili/internal/upstream"
)

const (
	// defaultTimeout 是单条探测总死线。一次探测要串行走 3~5 跳
	// （列表接口 → m3u8 → init/分片 → 可能还有 AES key），按时限收紧。
	defaultTimeout = 8 * time.Second
	// requestTimeout 是单跳请求超时；总预算仍由 context 死线兜住。
	requestTimeout = 6 * time.Second
	// segmentBudget 是分片/解密阶段的独立预算：走到这里说明 m3u8 已经拿到、
	// 源本身是通的，这一阶段失败只算「没探出画质」，不能升级成探测失败。
	segmentBudget = 4 * time.Second

	defaultConcurrency     = 3
	defaultHostConcurrency = 3

	// successTTL 用于成功与「测不出」两种有效结论；failureTTL 只用于失败。
	successTTL = 30 * 24 * time.Hour
	failureTTL = 2 * time.Minute

	// 读取上限：master playlist 通常 < 1KB，媒体列表可能几百 KB，只读够解析的头部。
	m3u8MaxBytes       = 64 << 10
	segmentPrefixBytes = 96 << 10
	initPrefixBytes    = 64 << 10
	keyMaxBytes        = 32
	detailMaxBytes     = 16 << 20
	detailPageMaxBytes = 8 << 20
)

// 稳定的错误文案。不包含完整 URL / 内网地址：上游错误原文可能带这些信息，
// 统一映射成固定文案，前端按文案做短标签（超时/防盗链/无资源）。
const (
	msgNotConfigured = "画质探测器未配置"
	msgInvalidItem   = "无效的源或影片 ID"
	msgNoEpisodes    = "未找到播放资源"
	msgTimeout       = "画质探测超时"
	msgCanceled      = "探测已取消"
	msgBlocked       = "目标地址不允许访问"
	msgTooLarge      = "上游响应体过大"
	msgRequestFailed = "上游请求失败"
	msgProbeFailed   = "画质探测失败"
	msgHTTPStatus    = "上游返回 HTTP %d"
)

// cachedResult 是落缓存的精简结果；「测不出」是 height=0 且 error 为空。
type cachedResult struct {
	Height int    `json:"h,omitempty"`
	Error  string `json:"e,omitempty"`
}

// Prober 是画质探测器。
type Prober struct {
	client upstream.Doer
	cache  cache.Store
	log    *slog.Logger
	ua     string

	timeout         time.Duration
	segmentBudget   time.Duration
	concurrency     int
	hostConcurrency int

	sf singleflight.Group

	mu    sync.Mutex
	hosts map[string]chan struct{}
}

// New 创建画质探测器；零值字段使用默认值（8s、3 并发、同 host 3 并发）。
func New(opts Options) *Prober {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if strings.TrimSpace(opts.UA) == "" {
		opts.UA = cms.DefaultUserAgent
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultConcurrency
	}
	if opts.HostConcurrency <= 0 {
		opts.HostConcurrency = defaultHostConcurrency
	}
	budget := segmentBudget
	if opts.Timeout < budget {
		budget = opts.Timeout
	}
	return &Prober{
		client:          opts.Client,
		cache:           opts.Cache,
		log:             opts.Logger,
		ua:              strings.TrimSpace(opts.UA),
		timeout:         opts.Timeout,
		segmentBudget:   budget,
		concurrency:     opts.Concurrency,
		hostConcurrency: opts.HostConcurrency,
		hosts:           make(map[string]chan struct{}),
	}
}

// Probe 探测单条「源 + 影片」的画质。height=0 且 error 为空表示「测不出」，
// 与请求失败（error 非空）是两种不同结论。同一 key 的并发请求会被合并。
func (p *Prober) Probe(ctx context.Context, item model.QualityProbeItem) model.QualityProbeResult {
	if ctx == nil {
		ctx = context.Background()
	}
	item.ID = strings.TrimSpace(item.ID)
	base := model.QualityProbeResult{SourceKey: item.Source.Key, VodID: item.ID}
	if p.client == nil {
		base.Error = msgNotConfigured
		return base
	}
	if !validProbeItem(item) {
		base.Error = msgInvalidItem
		return base
	}

	key := cacheKey(item)
	if res, ok := p.loadCached(key, base); ok {
		return res
	}

	runCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	// 网络工作在 singleflight 里用一个不受调用方取消影响的 context 跑：
	// 调用方断开时本函数立即返回，但合并到的那次探测不影响其他等待者。
	// sfBase 是 base 的只读副本：闭包与调用方可能并发执行，不能再共享可写变量。
	sfBase := base
	ch := p.sf.DoChan(key, func() (any, error) {
		if res, ok := p.loadCached(key, sfBase); ok {
			return res, nil
		}
		inner, cancelInner := context.WithTimeout(context.WithoutCancel(ctx), p.timeout)
		defer cancelInner()
		return p.probeOne(inner, item), nil
	})

	select {
	case v := <-ch:
		if res, ok := v.Val.(model.QualityProbeResult); ok {
			res.SourceKey, res.VodID = base.SourceKey, base.VodID
			return res
		}
		base.Error = msgProbeFailed
		return base
	case <-runCtx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			base.Error = msgCanceled
		} else {
			base.Error = msgTimeout
		}
		return base
	}
}

// ProbeMany 批量探测：全局并发 Options.Concurrency、同 host 并发
// Options.HostConcurrency，emit 串行调用；emit 返回错误时停止（其余结果丢弃）。
func (p *Prober) ProbeMany(ctx context.Context, items []model.QualityProbeItem, emit func(model.QualityProbeResult) error) {
	if len(items) == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan model.QualityProbeResult, len(items))
	sem := make(chan struct{}, p.concurrency)
	var wg sync.WaitGroup
	for _, item := range items {
		wg.Add(1)
		go func(item model.QualityProbeItem) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-runCtx.Done():
				return
			}
			defer func() { <-sem }()

			release, ok := p.acquireHost(runCtx, item.Source.URL)
			if !ok {
				return
			}
			defer release()
			results <- p.Probe(runCtx, item)
		}(item)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	if emit == nil {
		for range results {
		}
		return
	}
	for res := range results {
		if err := emit(res); err != nil {
			cancel()
			break
		}
	}
	// emit 失败后不再回调，但要等剩余 goroutine 结束（channel 带足缓冲，不会阻塞）。
	cancel()
	for range results {
	}
}

// acquireHost 取得同一 host 的并发名额。
// hosts 只增不减：条目数量受配置的源数量约束，不会随请求无限增长。
func (p *Prober) acquireHost(ctx context.Context, rawURL string) (func(), bool) {
	host := hostOf(rawURL)
	if host == "" {
		return func() {}, true
	}
	p.mu.Lock()
	ch := p.hosts[host]
	if ch == nil {
		ch = make(chan struct{}, p.hostConcurrency)
		p.hosts[host] = ch
	}
	p.mu.Unlock()

	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	case <-ctx.Done():
		return nil, false
	}
}

// probeOne 执行一次未缓存的探测并写缓存；客户端断开时由调用方提前返回，
// 但合并后的这次探测仍会跑完并落缓存。
func (p *Prober) probeOne(ctx context.Context, item model.QualityProbeItem) model.QualityProbeResult {
	start := time.Now()
	res := p.probeUncached(ctx, item)
	if res.Error == "" && ctx.Err() != nil {
		// 总死线已耗尽：不能把「超时」当成「测不出」按 30 天缓存。
		if errors.Is(ctx.Err(), context.Canceled) {
			res.Error = msgCanceled
		} else {
			res.Error = msgTimeout
		}
	}
	p.storeResult(item, res)
	p.log.Debug("画质探测完成",
		"source", item.Source.Key,
		"vod", item.ID,
		"height", res.Height,
		"error", res.Error,
		"ms", time.Since(start).Milliseconds())
	return res
}

func (p *Prober) loadCached(key string, base model.QualityProbeResult) (model.QualityProbeResult, bool) {
	if p.cache == nil {
		return base, false
	}
	raw, ok := p.cache.Get(key)
	if !ok {
		return base, false
	}
	var cached cachedResult
	if err := json.Unmarshal(raw, &cached); err != nil {
		return base, false
	}
	base.Height = cached.Height
	base.Error = cached.Error
	return base, true
}

func (p *Prober) storeResult(item model.QualityProbeItem, res model.QualityProbeResult) {
	if p.cache == nil {
		return
	}
	switch res.Error {
	case msgInvalidItem, msgNotConfigured, msgCanceled:
		return
	}
	ttl := successTTL
	if res.Error != "" {
		ttl = failureTTL
	}
	raw, err := json.Marshal(cachedResult{Height: res.Height, Error: res.Error})
	if err != nil {
		return
	}
	p.cache.Set(cacheKey(item), raw, ttl, ttl)
}

// classify 把上游错误映射成稳定文案；绝不透传原始错误（可能含内网 IP/完整 URL）。
func (p *Prober) classify(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, upstream.ErrBlocked) {
		return msgBlocked
	}
	if errors.Is(err, upstream.ErrTooLarge) {
		return msgTooLarge
	}
	var statusErr *upstream.StatusError
	if errors.As(err, &statusErr) {
		return fmt.Sprintf(msgHTTPStatus, statusErr.StatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return msgTimeout
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return msgCanceled
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return msgTimeout
	}
	return msgRequestFailed
}

func statusOf(err error) int {
	var statusErr *upstream.StatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode
	}
	return 0
}

func cacheKey(item model.QualityProbeItem) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s",
		strings.TrimRight(strings.TrimSpace(item.Source.URL), "/"),
		strings.TrimRight(strings.TrimSpace(item.Source.Detail), "/"),
		item.ID,
	)
	return "quality:" + hex.EncodeToString(h.Sum(nil))[:32]
}

func hostOf(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return ""
	}
	return strings.ToLower(parsed.Host)
}

func validProbeItem(item model.QualityProbeItem) bool {
	return validVideoID(item.ID) && isHTTPURL(strings.TrimSpace(item.Source.URL))
}

func validVideoID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

func isHTTPURL(raw string) bool {
	return strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
}
