package subscribe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"cilicili/internal/model"
	"cilicili/internal/upstream"
)

const (
	// defaultTimeout 是订阅拉取的默认超时（与 LibreTV 的 8000ms 一致）。
	defaultTimeout = 8 * time.Second
	// maxSubscribeBytes 是订阅内容上限：共享 TVBOX 配置常见数百 KB ~ 数 MB。
	maxSubscribeBytes = 16 << 20
)

var (
	// ErrInvalidURL 表示订阅地址不合法；接口层据此返回 400。
	ErrInvalidURL = errors.New("无效的订阅地址")
	// ErrFetchFailed 表示订阅拉取失败；接口层据此返回 502。
	ErrFetchFailed = errors.New("订阅拉取失败")
)

// Fetcher 拉取远程订阅并解析。竞态安全：无可变状态，可并发复用。
type Fetcher struct {
	client  upstream.Doer
	log     *slog.Logger
	timeout time.Duration
}

// New 创建订阅拉取器。Client 为 nil 时 Fetch 返回错误而不是 panic。
func New(opts Options) *Fetcher {
	f := &Fetcher{
		client:  opts.Client,
		log:     opts.Logger,
		timeout: opts.Timeout,
	}
	if f.log == nil {
		f.log = slog.Default()
	}
	if f.timeout <= 0 {
		f.timeout = defaultTimeout
	}
	return f
}

// Fetch 拉取并解析远程订阅。
//
// rawURL 先归一化（trim + 去尾斜杠/结尾空查询串），之后必须是 http(s) 地址；
// SSRF 判定与逐跳重定向复检由 upstream.Doer 负责。解析失败与网络失败通过
// ErrInvalidContent / ErrFetchFailed 区分，接口层据此选 400 还是 502。
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*model.SourceListPayload, error) {
	if f == nil || f.client == nil {
		return nil, errors.New("订阅不可用：未配置上游客户端")
	}

	target := NormalizeSubscriptionURL(rawURL)
	if !validHTTPURL(target) {
		return nil, fmt.Errorf("%w：只支持 http(s) 地址", ErrInvalidURL)
	}

	text, err := f.client.GetText(ctx, target, upstream.Options{
		Headers:       http.Header{"Accept": []string{"application/json"}},
		Timeout:       f.timeout,
		SafeRedirects: true,
		MaxBytes:      maxSubscribeBytes,
	})
	if err != nil {
		f.log.Debug("订阅拉取失败", "host", hostnameOf(target), "err", redactText(err.Error()))
		return nil, fetchError(err)
	}

	payload, err := Parse([]byte(text))
	if err != nil {
		return nil, err
	}
	// 内容合法但一条都没导入（例如老格式里全是非法地址）：按内容问题报错，
	// 避免调用方把空订阅当成「同步成功」把现有源清空。
	if len(payload.Sources) == 0 && len(payload.LiveSources) == 0 {
		return nil, contentErrorf("订阅中没有可导入的源")
	}
	return payload, nil
}

// fetchError 把上游错误收敛成对外文案：只保留分类与状态码，不回显完整订阅地址。
func fetchError(err error) error {
	var status *upstream.StatusError
	switch {
	case errors.As(err, &status):
		return fmt.Errorf("%w：订阅地址返回 HTTP %d", ErrFetchFailed, status.StatusCode)
	case errors.Is(err, upstream.ErrBlocked):
		return fmt.Errorf("%w：目标地址不可用", ErrFetchFailed)
	case errors.Is(err, upstream.ErrTooLarge):
		return fmt.Errorf("%w：订阅内容超出大小限制", ErrFetchFailed)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w：请求超时", ErrFetchFailed)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%w：请求已取消", ErrFetchFailed)
	}
	return fmt.Errorf("%w：无法访问订阅地址", ErrFetchFailed)
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
