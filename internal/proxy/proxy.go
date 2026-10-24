package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"cilicili/internal/m3u8"
	"cilicili/internal/upstream"
)

const (
	proxyPathPrefix = "/api/proxy"
	livePathPrefix  = "/api/live/stream"

	// 播放列表里的地址必须指回本站代理（查询串形式；路径形式会被 EdgeOne
	// 等网关的 URL 归一化破坏，只在兼容旧地址时读取）。
	proxyURLBase = "/api/proxy?url="
	liveURLBase  = "/api/live/stream?url="

	defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"

	copyBufferSize   = 32 << 10
	maxPlaylistBytes = 2 << 20 // m3u8 文本上限 2 MiB

	defaultHeaderTimeout = 15 * time.Second
	// proxyTotalTimeout 是普通代理（含 body 读取）的安全网：防止上游发完头
	// 就不再发 body 时占住连接。直播走长连接，不设该超时。
	proxyTotalTimeout = 60 * time.Second

	imageCacheControl   = "public, max-age=2592000" // 图片 30 天
	defaultCacheControl = "public, max-age=3600"    // 分片/key/其余 1 小时
	liveCacheControl    = "no-store, no-transform"

	doubanReferer = "https://movie.douban.com/"
	// doubanImageMirrorHost 是豆瓣图片的镜像（60s API 提供）。豆瓣 WAF 会对
	// Go 的 TLS 指纹返回 200 + JS 挑战页，导致封面空白；镜像可作为兜底。
	doubanImageMirrorHost = "doubanio.viki.moe"

	// imageFailTTL 是图片失败结果的短缓存：源站图床挂掉时不要每次翻页都
	// 卡 3–6 秒超时，后续请求直接快速失败，前端立刻显示占位图。
	imageFailTTL = 10 * time.Minute
)

// imageFailCache 是每个代理实例独立的图片失败短缓存（按完整 URL 记录）。
type imageFailCache struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func newImageFailCache() *imageFailCache {
	return &imageFailCache{until: make(map[string]time.Time)}
}

func (c *imageFailCache) recently(rawURL string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.until[rawURL]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(c.until, rawURL)
		return false
	}
	return true
}

func (c *imageFailCache) mark(rawURL string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.until) > 4096 {
		now := time.Now()
		for k, v := range c.until {
			if now.After(v) {
				delete(c.until, k)
			}
		}
	}
	c.until[rawURL] = time.Now().Add(imageFailTTL)
}

// looksLikeImage 按路径后缀判断是否图片请求（用于失败短缓存）。
func looksLikeImage(u *url.URL) bool {
	if u == nil {
		return false
	}
	path := strings.ToLower(u.Path)
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif", ".bmp"} {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

var (
	// doubanRefererHosts 是需要伪装 Referer 的豆瓣域（精确后缀匹配）。
	doubanRefererHosts  = []string{"douban.com", "doubanio.com"}
	errPlaylistTooLarge = errors.New("playlist too large")
)

// proxyHandler 同时服务点播代理与直播流代理，两者只在超时、重试、
// 内网放行与缓存策略上不同。
type proxyHandler struct {
	opts      Options
	live      bool
	imageFail *imageFailCache
}

// New 构造点播/图片流式代理。
func New(opts Options) http.Handler {
	return &proxyHandler{opts: opts, imageFail: newImageFailCache()}
}

// NewLive 构造直播长连接代理：只限制响应头等待时间，body 不限时。
func NewLive(opts Options) http.Handler {
	return &proxyHandler{opts: opts, live: true, imageFail: newImageFailCache()}
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.writeError(w, http.StatusMethodNotAllowed, "仅支持 GET/HEAD")
		return
	}

	raw := h.targetURL(r)
	if raw == "" {
		h.writeError(w, http.StatusBadRequest, "缺少 url 参数")
		return
	}
	target, err := parseTarget(raw)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "无效的 URL")
		return
	}
	if !h.authorized(r, target) {
		h.writeError(w, http.StatusUnauthorized, "未登录")
		return
	}
	h.forward(w, r, target)
}

// targetURL 取代理目标：优先查询串 ?url=，其次兼容路径形式
// /api/proxy/<encoded> 与 /api/live/stream/<encoded>。
func (h *proxyHandler) targetURL(r *http.Request) string {
	if v := strings.TrimSpace(r.URL.Query().Get("url")); v != "" {
		return v
	}

	prefix := proxyPathPrefix
	if h.live {
		prefix = livePathPrefix
	}
	path := r.URL.EscapedPath()
	if rest, ok := strings.CutPrefix(path, prefix); ok {
		path = rest
	}
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return ""
	}
	// 路径形式下目标被整体编码过一次；网关若已把 %2F 解码成 /，这一步是幂等的。
	if decoded, err := url.PathUnescape(path); err == nil {
		path = decoded
	}
	return strings.TrimSpace(path)
}

// parseTarget 做语法校验（协议 + 主机）。SSRF 校验由 upstream.Doer 负责。
func parseTarget(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("empty target")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("unsupported scheme")
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	return u, nil
}

// authorized 判定本次请求是否放行。
// Auth 为 nil 或未配置 PASSWORD 时全放行（本地开发）；已配置时普通代理要求
// 登录，匿名仅放行 AnonymousImageHosts 里的图片域；直播始终要求登录。
// 匿名白名单按「主机名等于后缀或为其子域」精确匹配，不能用裸 HasSuffix
// （否则 evil-doubanio.com 会命中 doubanio.com，形成鉴权绕过）。
func (h *proxyHandler) authorized(r *http.Request, target *url.URL) bool {
	m := h.opts.Auth
	if m == nil || !m.Configured() {
		return true
	}
	if m.FromRequest(r) {
		return true
	}
	if h.live {
		return false
	}
	return hostMatches(target.Hostname(), h.opts.AnonymousImageHosts)
}

func (h *proxyHandler) forward(w http.ResponseWriter, r *http.Request, target *url.URL) {
	if h.opts.Client == nil {
		h.writeError(w, http.StatusBadGateway, "代理请求失败")
		return
	}

	// 客户端断开（关页/切台）时取消上游请求，防止连接泄漏。
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	isImage := !h.live && r.Method == http.MethodGet && looksLikeImage(target)
	if isImage && h.imageFail.recently(target.String()) {
		h.writeError(w, http.StatusBadGateway, "封面不可用")
		return
	}

	upstreamOpts := h.upstreamOptions(r, target)
	resp, err := h.opts.Client.Do(ctx, target.String(), upstreamOpts)
	if err != nil {
		if r.Context().Err() != nil {
			return // 客户端已断开，不再写响应
		}
		if errors.Is(err, upstream.ErrBlocked) {
			h.writeError(w, http.StatusForbidden, "目标地址不允许访问")
			return
		}
		if isImage {
			h.imageFail.mark(target.String())
		}
		h.debug("proxy 上游请求失败", "method", r.Method, "err", err)
		h.writeError(w, http.StatusBadGateway, "代理请求失败")
		return
	}
	if resp == nil {
		if isImage {
			h.imageFail.mark(target.String())
		}
		h.writeError(w, http.StatusBadGateway, "代理请求失败")
		return
	}

	// 豆瓣图床对部分请求返回 200 + 反爬挑战页（text/html）。此时换镜像再取一次，
	// 否则浏览器拿到 HTML 无法解码，表现为封面空白。
	if !h.live && isDoubanImageTarget(target) && !isImageContentType(resp.Header.Get("Content-Type")) {
		if mirror := doubanImageMirror(target); mirror != nil {
			_ = resp.Close()
			if mresp, merr := h.opts.Client.Do(ctx, mirror.String(), h.upstreamOptions(r, mirror)); merr == nil && mresp != nil {
				h.debug("豆瓣图片走镜像兜底", "url", target.String(), "mirror", mirror.String())
				resp = mresp
			} else {
				h.debug("豆瓣图片镜像兜底失败", "url", target.String(), "err", merr)
				if isImage {
					h.imageFail.mark(target.String())
				}
				h.writeError(w, http.StatusBadGateway, "封面获取失败")
				return
			}
		}
	}
	defer func() { _ = resp.Close() }()

	finalURL := resp.FinalURL
	if finalURL == "" {
		finalURL = target.String()
	}
	contentType := resp.Header.Get("Content-Type")
	// 目标 URL 或重定向后的最终 URL 任一有 m3u8 特征都按播放列表处理：
	// 采集站常见「无后缀入口 302 到 .m3u8」与「.m3u8 返回 text/plain」。
	if m3u8.IsPlaylist(contentType, target.String()) || m3u8.IsPlaylist(contentType, finalURL) {
		h.servePlaylist(w, r, resp, finalURL)
		return
	}
	if isImage && !isImageContentType(contentType) {
		// 图床返回 HTML/空体：缓存失败，避免反复等待。
		h.imageFail.mark(target.String())
	}
	h.serveStream(w, r, resp)
}

func (h *proxyHandler) upstreamOptions(r *http.Request, target *url.URL) upstream.Options {
	opts := upstream.Options{
		Method:        r.Method,
		Headers:       h.upstreamHeaders(r, target),
		HeaderTimeout: h.opts.HeaderTimeout,
		SafeRedirects: true,
		// 0 表示「非流式读取」用默认上限；Do 返回的 body 由调用方流式消费，不受限。
		MaxBytes: 0,
	}
	if opts.HeaderTimeout <= 0 {
		opts.HeaderTimeout = defaultHeaderTimeout
	}
	if h.live {
		// 直播：body 不限时、不重试；部署者可显式放行内网自建源。
		opts.AllowPrivate = h.opts.LiveAllowPrivate
		opts.Retries = 0
	} else {
		opts.Timeout = proxyTotalTimeout
		opts.Retries = 1
	}
	return opts
}

func (h *proxyHandler) upstreamHeaders(r *http.Request, target *url.URL) http.Header {
	ua := h.opts.UA
	if ua == "" {
		ua = defaultUA
	}
	hdr := make(http.Header, 8)
	hdr.Set("User-Agent", ua)
	hdr.Set("Accept", "*/*")
	// Range / 条件请求逐项透传（206、304 依赖它们）。
	for _, name := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if v := r.Header.Get(name); v != "" {
			hdr.Set(name, v)
		}
	}
	// 豆瓣图床有防盗链：带电影页 Referer 才能取到封面。
	if !h.live && h.opts.DoubanReferer && hostMatches(target.Hostname(), doubanRefererHosts) {
		hdr.Set("Referer", doubanReferer)
	}
	return hdr
}

// servePlaylist 读取 m3u8 文本并改写为本站代理地址。
func (h *proxyHandler) servePlaylist(w http.ResponseWriter, r *http.Request, resp *upstream.Response, finalURL string) {
	status := statusOrOK(resp.StatusCode)
	h.setCommonHeaders(w)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")

	if r.Method == http.MethodHead || status == http.StatusNotModified || status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}

	raw, err := readLimited(resp.Body, maxPlaylistBytes)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, "播放列表读取失败")
		return
	}
	proxyBase := proxyURLBase
	if h.live {
		proxyBase = liveURLBase
	}
	body := m3u8.Rewrite(string(raw), finalURL, proxyBase)

	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// serveStream 流式透传图片/分片/FLV 等二进制响应，不整体读入内存。
func (h *proxyHandler) serveStream(w http.ResponseWriter, r *http.Request, resp *upstream.Response) {
	status := statusOrOK(resp.StatusCode)

	if h.live && status >= http.StatusBadRequest {
		// 透传上游状态码便于前端区分「token 过期」与「源瞬断」，但不回传上游 body。
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Live-Upstream-Status", strconv.Itoa(status))
		h.writeError(w, status, "直播流上游返回 "+strconv.Itoa(status))
		return
	}

	h.copyStreamHeaders(w, resp)
	w.WriteHeader(status)

	if r.Method == http.MethodHead || status == http.StatusNotModified || status == http.StatusNoContent {
		return
	}
	copyStream(w, resp.Body)
}

func (h *proxyHandler) copyStreamHeaders(w http.ResponseWriter, resp *upstream.Response) {
	hdr := w.Header()
	// 只复制语义头；content-length/content-encoding 不透传：
	// Transport 已自动解压，长度对不上会让浏览器二次解压/截断。
	for _, name := range []string{"Content-Type", "Accept-Ranges", "Content-Range", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(name); v != "" {
			hdr.Set(name, v)
		}
	}
	h.setCommonHeaders(w)

	if h.live {
		if hdr.Get("Content-Type") == "" {
			hdr.Set("Content-Type", "video/mp2t")
		}
		hdr.Set("Cache-Control", liveCacheControl)
		hdr.Set("X-Accel-Buffering", "no")
		return
	}
	if strings.HasPrefix(strings.ToLower(hdr.Get("Content-Type")), "image/") {
		hdr.Set("Cache-Control", imageCacheControl)
		return
	}
	// 非图片/视频（典型是反爬挑战页）不缓存：避免一次失败被浏览器缓存住，
	// 换线路/镜像后仍显示旧内容。
	if strings.HasPrefix(strings.ToLower(hdr.Get("Content-Type")), "text/html") {
		hdr.Set("Cache-Control", "no-store")
		return
	}
	hdr.Set("Cache-Control", defaultCacheControl)
}

func (h *proxyHandler) setCommonHeaders(w http.ResponseWriter) {
	hdr := w.Header()
	// Web 端直连上游受限时，ACAO 让浏览器可读响应；nosniff 防止内容嗅探。
	hdr.Set("Access-Control-Allow-Origin", "*")
	hdr.Set("X-Content-Type-Options", "nosniff")
}

func (h *proxyHandler) writeError(w http.ResponseWriter, status int, msg string) {
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json; charset=utf-8")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (h *proxyHandler) debug(msg string, args ...any) {
	if h.opts.Logger != nil {
		h.opts.Logger.Debug(msg, args...)
	}
}

// copyStream 用 32 KiB 缓冲流式转发，并在每块后 flush，
// 保证 FLV/TS 这类长连接低延迟（nginx 侧还应关 buffering）。
func copyStream(w http.ResponseWriter, body io.Reader) {
	if body == nil {
		return
	}
	dst := io.Writer(w)
	if f, ok := w.(http.Flusher); ok {
		dst = flushingWriter{w: w, f: f}
	}
	_, _ = io.CopyBuffer(dst, body, make([]byte, copyBufferSize))
}

type flushingWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw flushingWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}

// readLimited 读取上限为 max 的 body；超限返回 errPlaylistTooLarge。
func readLimited(body io.Reader, max int64) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errPlaylistTooLarge
	}
	return data, nil
}

// isDoubanImageTarget 判断是否需要为豆瓣图片准备兜底。
func isDoubanImageTarget(u *url.URL) bool {
	if u == nil || !hostMatches(u.Hostname(), []string{"doubanio.com"}) {
		return false
	}
	return strings.HasPrefix(u.Path, "/view/photo/")
}

// doubanImageMirror 返回等价镜像地址；豆瓣图片路径在镜像上保持一致。
func doubanImageMirror(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	return &url.URL{
		Scheme:   "https",
		Host:     doubanImageMirrorHost,
		Path:     u.Path,
		RawQuery: u.RawQuery,
	}
}

func isImageContentType(ct string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "image/")
}

// hostMatches 按「等于后缀或为其子域」匹配主机名，大小写不敏感，
// 兼容配置里的 ".example.com" / "*.example.com" 写法。
func hostMatches(host string, suffixes []string) bool {
	host = normalizeHost(host)
	if host == "" {
		return false
	}
	for _, suffix := range suffixes {
		suffix = normalizeHost(suffix)
		if suffix == "" {
			continue
		}
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(host, "*.")
	return strings.Trim(host, ".")
}

// statusOrOK 把 Doer 返回的非法状态码归一化，避免 WriteHeader panic。
func statusOrOK(code int) int {
	if code < 100 || code > 599 {
		return http.StatusOK
	}
	return code
}
