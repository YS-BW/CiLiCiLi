package recommend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"cilicili/internal/cache"
	"cilicili/internal/upstream"
)

const (
	// defaultUserAgent 是默认请求 UA。豆瓣的搜索引擎接口要求像浏览器的 UA，
	// Bangumi / 60s API 只要求「可识别的 UA」（浏览器 UA 同样可用），因此三者共用一个默认值。
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"
	// defaultTimeout 是单次上游请求的默认超时（与配置层 RecommendTimeout 的默认 10s 对齐）。
	defaultTimeout = 10 * time.Second
	// maxRecommendBody 是推荐接口的响应体上限：豆瓣/Bangumi/60s 正常都在百 KB 量级。
	maxRecommendBody = 4 << 20
	// defaultSixtyAPIBase 是 60s API 的默认社区实例；部署者可用 SixtyAPIBase 指向自建实例。
	defaultSixtyAPIBase = "https://60s.crystelf.top"
	// officialSixtyAPIBase 是 60s 官方公共实例：仅在主实例限流/宕机时回退。
	// 官方实例每日额度有限、数据中心出口 IP 常被限流，不能当主用。
	officialSixtyAPIBase = "https://60s.viki.moe"
	// staleMultiplier 决定上游持续失败时陈旧数据最多还能兜底多久（5 倍新鲜期）。
	staleMultiplier = 5
)

// cacheWindow 是一类推荐数据的缓存窗口。
type cacheWindow struct {
	fresh time.Duration
	stale time.Duration // 新鲜期之后仍可兜底的额外时长
}

// window 由新鲜期推导出完整缓存窗口。
func window(fresh time.Duration) cacheWindow {
	return cacheWindow{fresh: fresh, stale: staleMultiplier * fresh}
}

// Fetcher 拉取首页推荐数据（豆瓣 / Bangumi / 影视榜单）。
type Fetcher struct {
	client    upstream.Doer
	cache     cache.Store
	log       *slog.Logger
	ua        string
	sixtyBase string
	timeout   time.Duration
}

// New 创建推荐数据拉取器。Client 为 nil 时各方法返回错误而不是 panic，
// 便于未配置出网客户端的环境优雅降级。
func New(opts Options) *Fetcher {
	f := &Fetcher{
		client:    opts.Client,
		cache:     opts.Cache,
		log:       opts.Logger,
		ua:        strings.TrimSpace(opts.UA),
		sixtyBase: strings.TrimSpace(opts.SixtyAPIBase),
		timeout:   opts.Timeout,
	}
	if f.log == nil {
		f.log = slog.Default()
	}
	if f.ua == "" {
		f.ua = defaultUserAgent
	}
	if f.timeout <= 0 {
		f.timeout = defaultTimeout
	}
	if f.sixtyBase == "" {
		f.sixtyBase = defaultSixtyAPIBase
	}
	f.sixtyBase = strings.TrimRight(f.sixtyBase, "/")
	return f
}

// ready 检查构造参数，返回可直接对外展示的错误。
func (f *Fetcher) ready() error {
	if f == nil || f.client == nil {
		return errors.New("推荐数据不可用：未配置上游客户端")
	}
	return nil
}

// requestOptions 是推荐接口统一的请求参数：显式逐跳校验重定向、限制 body、总超时。
func (f *Fetcher) requestOptions(headers http.Header) upstream.Options {
	return upstream.Options{
		Headers:       headers,
		Timeout:       f.timeout,
		SafeRedirects: true,
		MaxBytes:      maxRecommendBody,
	}
}

// doubanHeaders 返回豆瓣要求的请求头：浏览器 UA + 站内 Referer。
func (f *Fetcher) doubanHeaders() http.Header {
	h := http.Header{}
	h.Set("User-Agent", f.ua)
	h.Set("Referer", doubanReferer)
	h.Set("Accept", "application/json")
	return h
}

// jsonHeaders 返回 Bangumi / 60s 使用的请求头。
func (f *Fetcher) jsonHeaders() http.Header {
	h := http.Header{}
	h.Set("User-Agent", f.ua)
	h.Set("Accept", "application/json")
	return h
}

// cached 是三类推荐数据共用的「读缓存 → 拉取 → 回写 → 失败用陈旧数据兜底」流程。
//
// store 为 nil 表示不缓存；拉取失败时若仍有未过 stale 期的旧数据就照旧返回，
// 首页宁可展示过期榜单也不要整块空白。对外错误由 describeError 脱敏。
func cached[T any](
	store cache.Store,
	log *slog.Logger,
	key string,
	cw cacheWindow,
	what string,
	path string,
	fetch func() (T, error),
) (T, error) {
	if value, ok := cacheGet[T](store, key); ok {
		return value, nil
	}
	value, err := fetch()
	if err != nil {
		if stale, ok := cacheGetStale[T](store, key); ok {
			log.Debug(what+"拉取失败，沿用陈旧缓存", "path", path, "err", redactText(err.Error()))
			return stale, nil
		}
		return value, describeError(what, err)
	}
	cacheSet(log, store, key, value, cw)
	return value, nil
}

// cacheGet 读取并解码仍在新鲜期内的缓存；缓存缺失或数据损坏都按未命中处理。
func cacheGet[T any](store cache.Store, key string) (T, bool) {
	var zero T
	if store == nil {
		return zero, false
	}
	raw, ok := store.Get(key)
	if !ok {
		return zero, false
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return zero, false
	}
	return value, true
}

// cacheGetStale 读取可能已过新鲜期的缓存（兜底用）。
func cacheGetStale[T any](store cache.Store, key string) (T, bool) {
	var zero T
	if store == nil {
		return zero, false
	}
	raw, _, found := store.GetStale(key)
	if !found {
		return zero, false
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return zero, false
	}
	return value, true
}

// cacheSet 把结果序列化后写入缓存。缓存失败不影响本次返回。
func cacheSet[T any](log *slog.Logger, store cache.Store, key string, value T, cw cacheWindow) {
	if store == nil {
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		log.Debug("推荐数据序列化失败，跳过缓存", "key", key, "err", err)
		return
	}
	store.Set(key, raw, cw.fresh, cw.stale)
}

// badDataError 是「已经足够友好、无需再脱敏」的对外错误（数据为空/格式异常等）。
// describeError 会原样返回它的文案。
type badDataError struct{ msg string }

func (e *badDataError) Error() string { return e.msg }

// badData 构造一个可直接展示的数据异常错误。
func badData(format string, args ...any) error {
	return &badDataError{msg: fmt.Sprintf(format, args...)}
}

// describeError 把上游错误收敛成不含完整 URL / 内网地址的对外文案：
// 状态码、超时、SSRF 拒绝等分类保留，底层错误文本只进日志。
func describeError(what string, err error) error {
	var dataErr *badDataError
	if errors.As(err, &dataErr) {
		return dataErr
	}
	var status *upstream.StatusError
	switch {
	case errors.Is(err, upstream.ErrBlocked):
		return fmt.Errorf("%s失败：目标地址不可用", what)
	case errors.Is(err, upstream.ErrTooLarge):
		return fmt.Errorf("%s失败：上游响应超出大小限制", what)
	case errors.As(err, &status):
		return fmt.Errorf("%s失败：上游返回 HTTP %d", what, status.StatusCode)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s失败：请求超时", what)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s失败：请求已取消", what)
	}
	return fmt.Errorf("%s失败", what)
}

var (
	// urlLikePattern 匹配错误文本里的完整 URL（含 query），日志里需要打码。
	urlLikePattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://[^\s"']+`)
	// ipLikePattern 匹配 IPv4 字面量，避免日志泄露目标地址。
	ipLikePattern = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
)

// redactText 把错误文本里的 URL 与 IP 打码，仅用于日志。
func redactText(text string) string {
	text = urlLikePattern.ReplaceAllString(text, "<url>")
	return ipLikePattern.ReplaceAllString(text, "<ip>")
}

// truncateRunes 按字符数截断（避免把用户输入原样回显进错误/日志）。
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}
