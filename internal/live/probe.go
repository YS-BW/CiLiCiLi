package live

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"cilicili/internal/m3u8"
	"cilicili/internal/model"
	"cilicili/internal/upstream"
)

const (
	// defaultProbeConcurrency 是批量测活的并发 worker 数（LibreTV 同款 16）。
	defaultProbeConcurrency = 16
	// probeHostLimit 限制同一 host 的在途探测数：同一运营商的频道常共用 host，
	// 不限流会以 16×N 的并发冲击弱源，越测越红。
	probeHostLimit = 3
	// defaultProbeBudget 是单个频道三级探测的总预算（含各级请求）。
	defaultProbeBudget = 10 * time.Second
	// probeLevelTimeout 是入口/variant 单级请求上限，避免某一级吃光总预算。
	probeLevelTimeout = 5 * time.Second
	// probeSegmentTimeout 是分片级等待上限（分片只需响应头，但允许源忽略 Range 整段返回）。
	probeSegmentTimeout = 4 * time.Second
	// probeThroughputBytes 是分片吞吐采样字节数（Range: bytes=0-131071）。
	probeThroughputBytes = 128 << 10
	// probeThroughputBudget 是吞吐采样的时间上限。
	probeThroughputBudget = 2500 * time.Millisecond
	// probeThroughputMinRemaining 是采样所需最低剩余预算，不足时跳过采样避免拖长尾。
	probeThroughputMinRemaining = 1500 * time.Millisecond
	// probeManifestMaxBytes 是二级探测读取的播放列表上限。
	probeManifestMaxBytes = 2 << 20

	// probeCacheOKTTL / probeCacheFailTTL：成功结果稳定，失败可能是瞬断。
	probeCacheOKTTL   = 30 * time.Minute
	probeCacheFailTTL = 2 * time.Minute
	// probeCacheMaxEntries 需覆盖典型全量频道数（几千），否则一轮测活就挤爆缓存。
	probeCacheMaxEntries = 20_000
)

var (
	// errorPlaceholderRe 识别「令牌失效/账号异常」时源返回的 200 错误占位资源
	//（如 /status/error_account_pirated.mp4），只在发生重定向时判定。
	errorPlaceholderRe = regexp.MustCompile(`(?i)(^|/)(error|errors|status|denied|forbidden|expired|invalid|pirated|unauthorized)([_\-./]|$)`)
	// m3u8PathRe 匹配播放列表路径（.m3u / .m3u8）。
	m3u8PathRe = regexp.MustCompile(`(?i)\.m3u8?$`)
	// hlsMapURIPattern 取 EXT-X-MAP 的初始化段地址。
	hlsMapURIPattern = regexp.MustCompile(`URI="([^"]+)"`)
	// codecsPattern 取 master playlist 的 CODECS 属性。
	codecsPattern = regexp.MustCompile(`(?i)CODECS="([^"]+)"`)
	// probeIPv4Re / probeIPv6Re 用于从错误文案里抹掉 IP（要求错误文案不含内网 IP）。
	probeIPv4Re = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b`)
	probeIPv6Re = regexp.MustCompile(`\[[0-9A-Fa-f:]+\](?::\d+)?`)
)

// probeCacheEntry 是一条测活缓存。
type probeCacheEntry struct {
	result    model.LiveProbeResult
	expiresAt time.Time
}

// Probe 批量测活。每个 URL 的结果经 emit 串行回调；命中短缓存的结果立即回调。
//
// 并发模型：worker 数 = Options.ProbeConcurrency（默认 16），同一 host 在途 ≤3；
// 单频道总预算 10s；ctx 取消（客户端断开）后停止派发。emit 返回错误视为
// 消费端断开，立即停止并取消在途探测。
func (s *Service) Probe(ctx context.Context, urls []string, emit func(model.LiveProbeResult) error) {
	if emit == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	pending := make([]string, 0, len(urls))
	seen := make(map[string]bool, len(urls))
	for _, raw := range urls {
		target := strings.TrimSpace(raw)
		if !isHTTPURL(target) || seen[target] {
			continue
		}
		seen[target] = true
		if hit, ok := s.cachedProbe(target); ok {
			if err := emit(hit); err != nil {
				return
			}
			continue
		}
		pending = append(pending, target)
	}
	if len(pending) == 0 {
		return
	}
	if s.client == nil {
		for _, target := range pending {
			if err := emit(model.LiveProbeResult{URL: target, Error: ErrNoClient.Error()}); err != nil {
				return
			}
		}
		return
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		emitMu  sync.Mutex
		emitErr error
		wg      sync.WaitGroup
		cursor  atomic.Int64
	)
	send := func(res model.LiveProbeResult) {
		emitMu.Lock()
		if emitErr != nil {
			emitMu.Unlock()
			return
		}
		err := emit(res)
		if err != nil {
			emitErr = err
			emitMu.Unlock()
			cancel()
			return
		}
		emitMu.Unlock()
	}

	workers := s.probeConcurrency
	if workers <= 0 {
		workers = defaultProbeConcurrency
	}
	if workers > len(pending) {
		workers = len(pending)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if runCtx.Err() != nil {
					return
				}
				idx := int(cursor.Add(1)) - 1
				if idx >= len(pending) {
					return
				}
				res := s.probeOne(runCtx, pending[idx])
				// 客户端断开/emit 失败后不再写缓存与回调。
				if runCtx.Err() != nil {
					return
				}
				s.storeProbe(res)
				send(res)
			}
		}()
	}
	wg.Wait()
}

// probeOne 探测单个频道：m3u8 走 manifest → variant → 分片三级，直链只做一级。
func (s *Service) probeOne(ctx context.Context, rawURL string) model.LiveProbeResult {
	release, err := s.limiter.acquire(ctx, hostOf(rawURL))
	if err != nil {
		return model.LiveProbeResult{URL: rawURL, Error: "探测已取消"}
	}
	defer release()

	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, s.probeBudget)
	defer cancel()
	deadline, _ := probeCtx.Deadline()
	remain := func() time.Duration { return time.Until(deadline) }
	capTimeout := func(limit time.Duration) time.Duration {
		d := remain()
		if d <= 0 {
			return time.Millisecond
		}
		if d > limit {
			d = limit
		}
		return d
	}

	referer := refererOf(rawURL)
	entry, fetchErr := s.probeFetch(probeCtx, rawURL, capTimeout(probeLevelTimeout), referer, "")
	if fetchErr != nil {
		return s.probeFailure(rawURL, start, probeCtx, 0, fetchErr)
	}
	if !entry.ok {
		return model.LiveProbeResult{
			URL:    rawURL,
			Status: entry.status,
			MS:     elapsedMS(start),
			Error:  fmt.Sprintf("入口响应 %d", entry.status),
		}
	}
	// 令牌失效/账号异常时源常 302 到错误提示媒体：HTTP 层 200 但本质不可用。
	// 仍以 m3u8 结尾的重定向视为正常播放列表。
	if !sameTarget(entry.finalURL, rawURL) {
		if path := pathOf(entry.finalURL); !m3u8PathRe.MatchString(path) && errorPlaceholderRe.MatchString(path) {
			return model.LiveProbeResult{
				URL:    rawURL,
				Status: entry.status,
				MS:     elapsedMS(start),
				Error:  "源返回错误占位内容（令牌失效 / 账号异常）",
			}
		}
	}
	// 非 m3u8（FLV/TS 直链）：拒绝明显的网页/接口响应，其余仅标记「可达」。
	if !entry.isM3U8 {
		if isWebPageType(entry.contentType) {
			return model.LiveProbeResult{
				URL:    rawURL,
				Status: entry.status,
				MS:     elapsedMS(start),
				Error:  fmt.Sprintf("返回内容不是流媒体（%s）", mediaType(entry.contentType)),
			}
		}
		return model.LiveProbeResult{
			URL:    rawURL,
			OK:     true,
			Status: entry.status,
			MS:     elapsedMS(start),
			Level:  "head",
		}
	}

	// m3u8 必须含 #EXTM3U 头，否则是伪装的 HTML/错误页。
	entryText := m3uBody(entry.text)
	if !strings.HasPrefix(entryText, "#EXTM3U") {
		return model.LiveProbeResult{
			URL:    rawURL,
			Status: entry.status,
			MS:     elapsedMS(start),
			Error:  "返回内容不是有效的 m3u8",
		}
	}

	mediaURL, mediaText := entry.finalURL, entryText
	codec := extractCodecs(entryText)
	variantURL, isMaster := firstVariantURL(entryText, entry.finalURL)
	if isMaster {
		if variantURL == "" {
			return model.LiveProbeResult{
				URL:   rawURL,
				MS:    elapsedMS(start),
				Error: "playlist 中没有可用的流地址",
			}
		}
		variant, vErr := s.probeFetch(probeCtx, variantURL, capTimeout(probeLevelTimeout), referer, "")
		if vErr != nil {
			res := s.probeFailure(rawURL, start, probeCtx, 0, vErr)
			res.Codec = codec
			return res
		}
		variantText := m3uBody(variant.text)
		if !variant.ok || !strings.HasPrefix(variantText, "#EXTM3U") {
			status := variant.status
			if status == 0 {
				status = http.StatusBadGateway
			}
			return model.LiveProbeResult{
				URL:    rawURL,
				Status: status,
				MS:     elapsedMS(start),
				Codec:  codec,
				Error:  fmt.Sprintf("子播放列表响应 %d", status),
			}
		}
		mediaURL, mediaText = variant.finalURL, variantText
		if codec == "" {
			codec = extractCodecs(variantText)
		}
	}

	segmentURL := lastSegmentURL(mediaText, mediaURL)
	if segmentURL == "" {
		// 空 media playlist（源刚启动/无分片）视为 manifest 级通过。
		return model.LiveProbeResult{
			URL:    rawURL,
			OK:     true,
			Status: entry.status,
			MS:     elapsedMS(start),
			Level:  "manifest",
			Codec:  codec,
		}
	}

	segmentStart := time.Now()
	segment, segErr := s.probeFetch(probeCtx, segmentURL, capTimeout(probeSegmentTimeout), referer, "bytes=0-1")
	segmentMS := elapsedMS(segmentStart)
	if segErr != nil {
		res := s.probeFailure(rawURL, start, probeCtx, 0, segErr)
		res.MS = segmentMS // ms 只统计分片往返，三级累计对用户没有参考意义
		res.Level = "segment"
		res.Codec = codec
		return res
	}
	if !segment.ok {
		return model.LiveProbeResult{
			URL:    rawURL,
			Status: segment.status,
			MS:     segmentMS,
			Level:  "segment",
			Codec:  codec,
			Error:  fmt.Sprintf("分片响应 %d", segment.status),
		}
	}

	kbps := 0
	if remain() > probeThroughputMinRemaining {
		kbps = s.measureThroughput(probeCtx, segmentURL, referer)
	}
	return model.LiveProbeResult{
		URL:    rawURL,
		OK:     true,
		Status: segment.status,
		MS:     segmentMS,
		Level:  "segment",
		Codec:  codec,
		Kbps:   kbps,
	}
}

// probeResponse 是单级探测拿到的最小信息集。
type probeResponse struct {
	ok          bool
	status      int
	text        string // 仅 m3u8 读取
	finalURL    string
	contentType string
	isM3U8      bool
}

// probeFetch 请求一个资源。m3u8 读取文本供解析，其余只取响应头即断开 body；
// rangeValue 非空时用于分片级探测（只读响应头，不下载媒体数据）。
func (s *Service) probeFetch(ctx context.Context, rawURL string, timeout time.Duration, referer, rangeValue string) (*probeResponse, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	headers := http.Header{}
	headers.Set("User-Agent", s.ua)
	headers.Set("Accept", "*/*")
	if referer != "" {
		headers.Set("Referer", referer)
	}
	if rangeValue != "" {
		headers.Set("Range", rangeValue)
	}
	res, err := s.client.Do(reqCtx, rawURL, upstream.Options{
		Headers:       headers,
		AllowPrivate:  s.allowPrivate,
		SafeRedirects: true,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()

	out := &probeResponse{
		ok:          res.StatusCode >= 200 && res.StatusCode <= 299,
		status:      res.StatusCode,
		finalURL:    res.FinalURL,
		contentType: res.Header.Get("Content-Type"),
	}
	if out.finalURL == "" {
		out.finalURL = rawURL
	}
	out.isM3U8 = m3u8.IsPlaylist(out.contentType, rawURL)
	if out.isM3U8 && out.ok {
		body, readErr := io.ReadAll(io.LimitReader(res.Body, probeManifestMaxBytes))
		if readErr != nil {
			return nil, readErr
		}
		out.text = string(body)
	}
	return out, nil
}

// probeFailure 把失败归一为结果：预算耗尽标记 TimedOut，错误文案抹掉 IP。
func (s *Service) probeFailure(rawURL string, start time.Time, probeCtx context.Context, status int, err error) model.LiveProbeResult {
	res := model.LiveProbeResult{
		URL:    rawURL,
		Status: status,
		MS:     elapsedMS(start),
	}
	budgetExhausted := false
	if ctxErr := probeCtx.Err(); ctxErr != nil {
		if !errors.Is(ctxErr, context.DeadlineExceeded) {
			// 客户端断开 / emit 失败：结果不会再被回调，文案仅用于内部。
			res.Error = "探测已取消"
			return res
		}
		budgetExhausted = true
	} else if deadline, ok := probeCtx.Deadline(); ok && !time.Now().Before(deadline) {
		// 单级请求与总预算同时到点，谁先报错不确定；以「预算已耗尽」为准。
		budgetExhausted = true
	}
	if budgetExhausted {
		res.TimedOut = true
		res.Error = fmt.Sprintf("探测超时（%dms）", s.probeBudget.Milliseconds())
		return res
	}
	res.Error = sanitizeProbeError(err)
	return res
}

// measureThroughput 分片吞吐采样：Range 拉 128KB（或 2.5s 截止）估算 kbps。
// 与可达性检查是两次独立请求：前者只取响应头，这里才真正读少量数据。
func (s *Service) measureThroughput(ctx context.Context, rawURL, referer string) int {
	if s.client == nil {
		return 0
	}
	budget := probeThroughputBudget
	if remaining := remainingOf(ctx); remaining > 0 && remaining < budget {
		budget = remaining
	}
	if budget <= 0 {
		return 0
	}
	reqCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	headers := http.Header{}
	headers.Set("User-Agent", s.ua)
	headers.Set("Accept", "*/*")
	headers.Set("Range", fmt.Sprintf("bytes=0-%d", probeThroughputBytes-1))
	if referer != "" {
		headers.Set("Referer", referer)
	}
	res, err := s.client.Do(reqCtx, rawURL, upstream.Options{
		Headers:       headers,
		AllowPrivate:  s.allowPrivate,
		SafeRedirects: true,
	})
	if err != nil {
		return 0
	}
	defer func() { _ = res.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return 0
	}

	start := time.Now()
	var total int64
	buf := make([]byte, 32<<10)
	for total < probeThroughputBytes {
		want := len(buf)
		if left := probeThroughputBytes - int(total); left < want {
			want = left
		}
		n, readErr := res.Body.Read(buf[:want])
		total += int64(n)
		if readErr != nil || n == 0 || reqCtx.Err() != nil {
			break
		}
	}
	seconds := time.Since(start).Seconds()
	if total <= 0 || seconds < 0.05 {
		return 0
	}
	return int(float64(total) * 8 / seconds / 1000)
}

// firstVariantURL 从 master playlist 取第一个 variant 地址。
// 返回 (url, true) 表示是 master playlist；url 为空说明 STREAM-INF 后没有可用地址。
func firstVariantURL(content, baseURL string) (string, bool) {
	if !strings.Contains(content, "#EXT-X-STREAM-INF") {
		return "", false
	}
	lines := strings.Split(content, "\n")
	for i, raw := range lines {
		if !strings.HasPrefix(strings.TrimSpace(raw), "#EXT-X-STREAM-INF") {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if next == "" || strings.HasPrefix(next, "#") {
				continue
			}
			return resolveReference(next, baseURL), true
		}
	}
	return "", true
}

// lastSegmentURL 选一个用于探测的分片：优先初始化段（EXT-X-MAP），
// 否则取窗口内最后一个（最新）分片——直播滑动窗口里首个分片最接近过期。
func lastSegmentURL(content, baseURL string) string {
	var last string
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MAP") {
			if m := hlsMapURIPattern.FindStringSubmatch(line); m != nil {
				if uri := resolveReference(m[1], baseURL); uri != "" {
					return uri
				}
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if uri := resolveReference(line, baseURL); uri != "" {
			last = uri
		}
	}
	return last
}

// extractCodecs 提取 master playlist 的 CODECS 属性（用于提示 H.265 等浏览器
// 无法解码的源）。
func extractCodecs(content string) string {
	m := codecsPattern.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// resolveReference 把相对地址按 baseURL 解析为绝对 http(s) 地址。
func resolveReference(ref, baseURL string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	parsed, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if base, baseErr := url.Parse(baseURL); baseErr == nil {
		parsed = base.ResolveReference(parsed)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	if parsed.Host == "" {
		return ""
	}
	return parsed.String()
}

// m3uBody 去掉 BOM 与前导空白，用于 #EXTM3U 前缀校验。
func m3uBody(text string) string {
	text = strings.TrimPrefix(text, "\uFEFF")
	return strings.TrimLeftFunc(text, unicode.IsSpace)
}

// isWebPageType 判断 content-type 是否明显是网页/接口响应（200 的错误页不能当流）。
func isWebPageType(contentType string) bool {
	ct := strings.ToLower(contentType)
	for _, marker := range []string{
		"text/html", "application/xhtml", "application/json", "text/xml", "application/xml",
	} {
		if strings.Contains(ct, marker) {
			return true
		}
	}
	return false
}

// mediaType 只保留 content-type 的 MIME 主类型（错误文案用）。
func mediaType(contentType string) string {
	if idx := strings.IndexByte(contentType, ';'); idx >= 0 {
		contentType = contentType[:idx]
	}
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		return "未知类型"
	}
	return contentType
}

// sanitizeProbeError 把上游错误转成可回给客户端的安全文案（不含 IP/URL）。
func sanitizeProbeError(err error) string {
	if err == nil {
		return "探测失败"
	}
	return safeErrorText(err)
}

func elapsedMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}

func remainingOf(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		return time.Until(deadline)
	}
	return 24 * time.Hour
}

// hostOf 取 URL 主机名（小写、不含端口）作为限流键。
func hostOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// refererOf 用源站 origin 作 Referer：部分 IPTV 源校验 Referer 才放行列表/分片。
func refererOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host + "/"
}

// pathOf 取 URL 路径（用于错误占位资源识别）。
func pathOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Path
}

// sameTarget 判断两个 URL 是否指向同一资源（避免把等价写法误判为重定向）。
func sameTarget(a, b string) bool {
	ua, aErr := url.Parse(a)
	ub, bErr := url.Parse(b)
	if aErr != nil || bErr != nil {
		return a == b
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) &&
		strings.EqualFold(ua.Host, ub.Host) &&
		ua.EscapedPath() == ub.EscapedPath() &&
		ua.RawQuery == ub.RawQuery
}

// hostLimiter 按 host 限制在途请求数（同一源站并发 ≤limit）。
type hostLimiter struct {
	limit int
	mu    sync.Mutex
	sems  map[string]chan struct{}
}

func newHostLimiter(limit int) *hostLimiter {
	if limit <= 0 {
		limit = probeHostLimit
	}
	return &hostLimiter{limit: limit, sems: make(map[string]chan struct{})}
}

// acquire 获取一个 host 槽位；返回的 release 必须调用。
func (l *hostLimiter) acquire(ctx context.Context, host string) (func(), error) {
	l.mu.Lock()
	sem, ok := l.sems[host]
	if !ok {
		sem = make(chan struct{}, l.limit)
		l.sems[host] = sem
	}
	l.mu.Unlock()

	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// cachedProbe 读取测活短缓存。
func (s *Service) cachedProbe(rawURL string) (model.LiveProbeResult, bool) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	entry, ok := s.probeEntries[rawURL]
	if !ok {
		return model.LiveProbeResult{}, false
	}
	if s.now().After(entry.expiresAt) {
		delete(s.probeEntries, rawURL)
		return model.LiveProbeResult{}, false
	}
	return entry.result, true
}

// storeProbe 写入测活短缓存（成功 30min / 失败 2min），容量满时先清过期再淘汰一条。
func (s *Service) storeProbe(res model.LiveProbeResult) {
	ttl := probeCacheFailTTL
	if res.OK {
		ttl = probeCacheOKTTL
	}
	now := s.now()

	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if len(s.probeEntries) >= probeCacheMaxEntries {
		for key, entry := range s.probeEntries {
			if now.After(entry.expiresAt) {
				delete(s.probeEntries, key)
			}
		}
		if len(s.probeEntries) >= probeCacheMaxEntries {
			for key := range s.probeEntries {
				delete(s.probeEntries, key)
				break
			}
		}
	}
	s.probeEntries[res.URL] = probeCacheEntry{result: res, expiresAt: now.Add(ttl)}
}
