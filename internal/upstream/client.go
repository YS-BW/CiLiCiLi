package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// defaultUserAgent 与 config 包保持一致的浏览器 UA，避免采集站按 UA 拒绝。
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"
	// defaultMaxBytes 是 GetJSON/GetText 的默认 body 上限（16 MiB）。
	defaultMaxBytes = 16 << 20
	// maxSafeRedirects 是安全模式下手动跟随的最大跳数。
	maxSafeRedirects = 5
	// maxFollowRedirects 是标准库跟随（Options.SafeRedirects=false）的跳数上限。
	maxFollowRedirects = 10

	defaultMaxIdleConns        = 100
	defaultMaxIdleConnsPerHost = 16
	defaultIdleConnTimeout     = 90 * time.Second
	defaultConnectTimeout      = 5 * time.Second
	defaultTLSHandshakeTimeout = 5 * time.Second
	defaultRetryBase           = 100 * time.Millisecond
	maxRetryDelay              = 2 * time.Second
)

var (
	// ErrTooManyRedirects 表示安全模式下的重定向跳数超过上限。
	ErrTooManyRedirects = errors.New("重定向次数超过上限")
	// errHeaderTimeout 是 HeaderTimeout 触发时的取消原因（仅内部使用）。
	errHeaderTimeout = errors.New("等待上游响应头超时")
)

// sensitiveHeaders 在跨主机重定向时丢弃，避免把凭据转发给第三方。
var sensitiveHeaders = map[string]bool{
	"Authorization":       true,
	"Cookie":              true,
	"Proxy-Authorization": true,
}

// Client 是 Doer 的实现。所有请求都经过 SSRF 校验，DNS 只解析一次并把结果
// 固定到本次请求的连接上；重定向默认逐跳复检。
type Client struct {
	logger       *slog.Logger
	userAgent    string
	allowPrivate bool

	lookup lookupFunc
	dialer *net.Dialer

	transport    *http.Transport
	safeClient   *http.Client // 手动逐跳跟随（CheckRedirect 立即停止）
	followClient *http.Client // 标准库跟随（Options.SafeRedirects=false）

	retryBase time.Duration // 指数退避基数；测试可调小
}

var _ Doer = (*Client)(nil)

// New 构造出网客户端。Config 的零值表示使用推荐默认值。
func New(cfg Config) (Doer, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()

	c := &Client{
		logger:       cfg.Logger,
		userAgent:    cfg.UserAgent,
		allowPrivate: cfg.AllowPrivate,
		lookup:       defaultLookup,
		dialer:       &net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second},
		retryBase:    defaultRetryBase,
	}
	// Proxy 显式置空：走环境代理会让连接实际由代理建立，固定 IP 形同虚设。
	c.transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           c.dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   cfg.TLSHandshakeTimeout,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    false, // 由 Transport 自动协商并解压 gzip
	}
	c.safeClient = &http.Client{
		Transport: c.transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	// 标准跟随模式：保留标准库行为，只把跳数上限换成可判断的错误。
	c.followClient = &http.Client{
		Transport: c.transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxFollowRedirects {
				return ErrTooManyRedirects
			}
			return nil
		},
	}
	return c, nil
}

func (cfg Config) validate() error {
	for _, item := range []struct {
		name  string
		value time.Duration
	}{
		{"IdleConnTimeout", cfg.IdleConnTimeout},
		{"ConnectTimeout", cfg.ConnectTimeout},
		{"TLSHandshakeTimeout", cfg.TLSHandshakeTimeout},
		{"ResponseHeaderTimeout", cfg.ResponseHeaderTimeout},
	} {
		if item.value < 0 {
			return fmt.Errorf("upstream: Config.%s 不能为负数", item.name)
		}
	}
	if cfg.MaxIdleConns < 0 || cfg.MaxIdleConnsPerHost < 0 {
		return errors.New("upstream: Config.MaxIdleConns/MaxIdleConnsPerHost 不能为负数")
	}
	if strings.ContainsAny(cfg.UserAgent, "\r\n") || len(cfg.UserAgent) > 512 {
		return errors.New("upstream: Config.UserAgent 非法")
	}
	return nil
}

func (cfg Config) withDefaults() Config {
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	if cfg.MaxIdleConns == 0 {
		cfg.MaxIdleConns = defaultMaxIdleConns
	}
	if cfg.MaxIdleConnsPerHost == 0 {
		cfg.MaxIdleConnsPerHost = defaultMaxIdleConnsPerHost
	}
	if cfg.IdleConnTimeout == 0 {
		cfg.IdleConnTimeout = defaultIdleConnTimeout
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = defaultConnectTimeout
	}
	if cfg.TLSHandshakeTimeout == 0 {
		cfg.TLSHandshakeTimeout = defaultTLSHandshakeTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return cfg
}

// Do 发起请求并返回响应。任何 HTTP 状态码都返回 (*Response, nil)，body 交给调用方
// 流式读取；只有传输层错误、SSRF 拒绝、超时、重定向失败才返回 error。
// opts.SafeRedirects=true 时手动逐跳跟随并复检；否则交给标准库跟随（最多 10 跳）。
func (c *Client) Do(ctx context.Context, rawURL string, opts Options) (*Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	method := strings.ToUpper(strings.TrimSpace(opts.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !validMethod(method) {
		return nil, fmt.Errorf("upstream: 非法 HTTP 方法 %q", method)
	}
	attempts := opts.Retries + 1
	if attempts < 1 {
		attempts = 1
	}

	// 总超时（含 body）只创建一次，重试共享同一份预算。
	reqCtx := ctx
	var cancelTotal context.CancelFunc = func() {}
	if opts.Timeout > 0 {
		reqCtx, cancelTotal = context.WithTimeout(ctx, opts.Timeout)
	}

	start := time.Now()
	for attempt := 1; attempt <= attempts; attempt++ {
		httpResp, finalURL, cancelHdr, err := c.attempt(reqCtx, method, rawURL, opts)
		if err != nil {
			c.logDebug(reqCtx, "上游请求失败",
				"method", method, "url", redactURL(rawURL), "attempt", attempt,
				"ms", time.Since(start).Milliseconds(), "err", safeErrText(err))
			if attempt >= attempts || !retryable(err) || reqCtx.Err() != nil {
				cancelTotal()
				return nil, err
			}
			if werr := sleepCtx(reqCtx, c.backoff(attempt)); werr != nil {
				cancelTotal()
				return nil, fmt.Errorf("重试等待被取消: %w", werr)
			}
			continue
		}
		// 5xx 可能是上游临时故障；重试耗尽后仍按约定原样返回响应。
		if httpResp.StatusCode >= http.StatusInternalServerError && attempt < attempts {
			drainAndClose(httpResp.Body)
			c.logDebug(reqCtx, "上游 5xx，准备重试",
				"method", method, "url", redactURL(finalURL), "attempt", attempt,
				"status", httpResp.StatusCode, "ms", time.Since(start).Milliseconds())
			if werr := sleepCtx(reqCtx, c.backoff(attempt)); werr != nil {
				cancelTotal()
				return nil, fmt.Errorf("重试等待被取消: %w", werr)
			}
			continue
		}
		c.logDebug(reqCtx, "上游请求完成",
			"method", method, "url", redactURL(finalURL), "attempt", attempt,
			"status", httpResp.StatusCode, "ms", time.Since(start).Milliseconds())

		body := &cancelingBody{rc: httpResp.Body}
		body.cancel = func() {
			cancelHdr()
			cancelTotal()
		}
		return &Response{
			StatusCode: httpResp.StatusCode,
			Header:     httpResp.Header,
			Body:       body,
			FinalURL:   finalURL,
		}, nil
	}
	cancelTotal()
	return nil, errors.New("上游请求失败")
}

// attempt 是单次尝试：处理 HeaderTimeout，并完成整个重定向链。
// 成功时返回的 closeBody 用于在调用方关闭响应体时释放 HeaderTimeout 的上下文。
func (c *Client) attempt(ctx context.Context, method, rawURL string, opts Options) (*http.Response, string, func(), error) {
	noop := func() {}

	var (
		reqCtx   = ctx
		cancelH  context.CancelCauseFunc
		timer    *time.Timer
		timerEnd chan struct{}
	)
	// HeaderTimeout 只限制“拿到响应头”为止：计时器到点就取消请求，
	// 拿到响应头后立即停止，body 读取不受影响。
	if opts.HeaderTimeout > 0 {
		reqCtx, cancelH = context.WithCancelCause(ctx)
		timerEnd = make(chan struct{})
		timer = time.AfterFunc(opts.HeaderTimeout, func() {
			cancelH(errHeaderTimeout)
			close(timerEnd)
		})
	}

	httpResp, finalURL, err := c.fetch(reqCtx, method, rawURL, opts)

	headerTimedOut := false
	if timer != nil {
		if !timer.Stop() {
			<-timerEnd // 回调已启动，等它设置好取消原因
		}
		headerTimedOut = errors.Is(context.Cause(reqCtx), errHeaderTimeout)
	}
	if headerTimedOut {
		if httpResp != nil {
			drainAndClose(httpResp.Body)
		}
		if cancelH != nil {
			cancelH(nil)
		}
		return nil, "", noop,
			fmt.Errorf("%w（%s 内未收到响应头）: %w", errHeaderTimeout, opts.HeaderTimeout, context.DeadlineExceeded)
	}
	if err != nil {
		if cancelH != nil {
			cancelH(nil)
		}
		return nil, "", noop, err
	}
	closeBody := noop
	if cancelH != nil {
		closeBody = func() { cancelH(nil) }
	}
	return httpResp, finalURL, closeBody, nil
}

// fetch 根据 Options.SafeRedirects 选择重定向策略。
func (c *Client) fetch(ctx context.Context, method, rawURL string, opts Options) (*http.Response, string, error) {
	if opts.SafeRedirects {
		return c.fetchSafe(ctx, method, rawURL, opts)
	}
	return c.fetchFollow(ctx, method, rawURL, opts)
}

// fetchSafe 手动逐跳跟随重定向：每跳都重新做协议与 SSRF 校验并固定新的 IP。
func (c *Client) fetchSafe(ctx context.Context, method, rawURL string, opts Options) (*http.Response, string, error) {
	current, err := parseTarget(rawURL)
	if err != nil {
		return nil, "", err
	}
	allowPrivate := c.allowPrivate || opts.AllowPrivate
	curMethod := method
	dropSensitive := false
	redirects := 0
	for {
		resp, err := c.doRequest(ctx, c.safeClient, curMethod, current, opts, allowPrivate, dropSensitive)
		if err != nil {
			return nil, "", err
		}
		if !isRedirectStatus(resp.StatusCode) {
			return resp, current.String(), nil
		}
		location := strings.TrimSpace(resp.Header.Get("Location"))
		if location == "" {
			return resp, current.String(), nil
		}
		if redirects >= maxSafeRedirects {
			drainAndClose(resp.Body)
			return nil, "", fmt.Errorf("%w（最多 %d 跳）", ErrTooManyRedirects, maxSafeRedirects)
		}
		redirects++

		next, parseErr := current.Parse(location)
		drainAndClose(resp.Body)
		if parseErr != nil {
			return nil, "", fmt.Errorf("%w: 无法解析重定向目标", ErrBlocked)
		}
		// 301/302/303 把非 GET/HEAD 请求降级为 GET（与本项目实际只发 GET 一致）。
		if resp.StatusCode == http.StatusSeeOther ||
			((resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound) &&
				curMethod != http.MethodGet && curMethod != http.MethodHead) {
			curMethod = http.MethodGet
		}
		if !sameHostname(current, next) {
			dropSensitive = true
		}
		current = next
	}
}

// fetchFollow 交给标准库跟随重定向（最多 10 跳），不再逐跳做 SSRF 校验。
// 仅在调用方明确设置 Options.SafeRedirects=false 时使用。
func (c *Client) fetchFollow(ctx context.Context, method, rawURL string, opts Options) (*http.Response, string, error) {
	target, err := parseTarget(rawURL)
	if err != nil {
		return nil, "", err
	}
	allowPrivate := c.allowPrivate || opts.AllowPrivate
	resp, err := c.doRequest(ctx, c.followClient, method, target, opts, allowPrivate, false)
	if err != nil {
		return nil, "", err
	}
	finalURL := target.String()
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	return resp, finalURL, nil
}

// doRequest 完成单跳请求：校验并固定 DNS 结果，然后交给 http.Client。
func (c *Client) doRequest(
	ctx context.Context,
	httpClient *http.Client,
	method string,
	target *url.URL,
	opts Options,
	allowPrivate bool,
	dropSensitive bool,
) (*http.Response, error) {
	addrs, err := resolveAndValidate(ctx, c.lookup, target, allowPrivate)
	if err != nil {
		return nil, err
	}
	plan := &dialPlan{host: target.Hostname(), addrs: addrs}
	req, err := http.NewRequestWithContext(withDialPlan(ctx, plan), method, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("构造上游请求失败: %w", err)
	}
	c.setHeaders(req, opts, dropSensitive)
	return httpClient.Do(req)
}

// GetJSON 读取 2xx 响应体（不解析 JSON），超过 MaxBytes 返回 ErrTooLarge。
// MaxBytes=0 用默认 16 MiB，<0 表示不限制。
func (c *Client) GetJSON(ctx context.Context, rawURL string, opts Options) ([]byte, error) {
	resp, err := c.Do(ctx, rawURL, opts)
	if err != nil {
		return nil, err
	}
	defer resp.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{StatusCode: resp.StatusCode, URL: resp.FinalURL}
	}
	return readLimited(resp.Body, opts.MaxBytes)
}

// GetText 与 GetJSON 相同，只是返回字符串（用于 m3u8/HTML 等文本）。
func (c *Client) GetText(ctx context.Context, rawURL string, opts Options) (string, error) {
	body, err := c.GetJSON(ctx, rawURL, opts)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	if max < 0 {
		return io.ReadAll(r)
	}
	if max == 0 {
		max = defaultMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("%w: 上限 %d 字节", ErrTooLarge, max)
	}
	return body, nil
}

// setHeaders 先写默认头，再用调用方提供的头覆盖；跨主机重定向时丢弃敏感头。
func (c *Client) setHeaders(req *http.Request, opts Options, dropSensitive bool) {
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "*/*")
	for key, values := range opts.Headers {
		canonical := http.CanonicalHeaderKey(key)
		if dropSensitive && sensitiveHeaders[canonical] {
			continue
		}
		if len(values) == 0 {
			continue
		}
		req.Header[canonical] = append([]string(nil), values...)
	}
}

// cancelingBody 在 Close 时释放与本次请求绑定的上下文（总超时、HeaderTimeout），
// 避免调用方读完 body 后仍残留计时器。
type cancelingBody struct {
	rc     io.ReadCloser
	once   sync.Once
	cancel func()
}

func (b *cancelingBody) Read(p []byte) (int, error) { return b.rc.Read(p) }

func (b *cancelingBody) Close() error {
	b.once.Do(func() {
		if b.cancel != nil {
			b.cancel()
		}
	})
	return b.rc.Close()
}

// validMethod 按 RFC 7230 的 token 规则校验方法名，避免把非法输入交给 Transport 反复重试。
func validMethod(method string) bool {
	if method == "" || len(method) > 32 {
		return false
	}
	for i := 0; i < len(method); i++ {
		ch := method[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", rune(ch)):
		default:
			return false
		}
	}
	return true
}

func isRedirectStatus(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

func sameHostname(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Hostname(), b.Hostname())
}

// retryable 判断错误是否值得重试：连接错误、传输层超时可以重试；
// SSRF 拒绝、重定向超限、调用方取消、总超时已到、HeaderTimeout 不重试。
func retryable(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrBlocked), errors.Is(err, ErrTooManyRedirects):
		return false
	case errors.Is(err, errHeaderTimeout):
		return true // 响应头超时属于上游慢，可重试；预算仍由总超时兜底
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	}
	return true
}

// backoff 返回带抖动的指数退避时长，区间约为 [d/2, 1.5d)。
func (c *Client) backoff(attempt int) time.Duration {
	base := c.retryBase
	if base <= 0 {
		base = defaultRetryBase
	}
	delay := base << (attempt - 1)
	if delay <= 0 || delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	return delay/2 + time.Duration(rand.Int64N(int64(delay)))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4<<10))
	_ = body.Close()
}

func (c *Client) logDebug(ctx context.Context, msg string, args ...any) {
	if c.logger == nil || !c.logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	c.logger.DebugContext(ctx, msg, args...)
}

// redactURL 只保留协议/主机/路径和查询参数名，避免日志泄露 token 等敏感值。
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<非法 URL>"
	}
	u.User = nil
	u.Fragment = ""
	switch {
	case u.RawQuery == "":
	case len(u.Query()) == 0:
		u.RawQuery = "<redacted>"
	default:
		keys := make([]string, 0, len(u.Query()))
		for key := range u.Query() {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, key := range keys {
			parts[i] = key + "=<redacted>"
		}
		u.RawQuery = strings.Join(parts, "&")
	}
	s := u.String()
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

// safeErrText 保证日志里的错误串也不带完整 query。
func safeErrText(err error) string {
	if err == nil {
		return ""
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Op + " " + redactURL(urlErr.URL) + ": " + urlErr.Err.Error()
	}
	return err.Error()
}
