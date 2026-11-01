// Package upstream 封装所有出网 HTTP 请求：SSRF 防护、DNS 固定、超时、
// 安全重定向、重试与连接池。任何由用户输入驱动的服务端请求都必须走这里。
package upstream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// ErrBlocked 表示目标地址未通过 SSRF 校验。
var ErrBlocked = errors.New("目标地址不在允许范围内")

// ErrTooLarge 表示响应体超过 MaxBytes。
var ErrTooLarge = errors.New("上游响应体超过大小限制")

// StatusError 表示上游返回了非 2xx 状态码。
type StatusError struct {
	StatusCode int
	URL        string
}

func (e *StatusError) Error() string {
	return "上游返回 HTTP " + itoa(e.StatusCode)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Options 是单次请求的行为参数。零值表示使用 Client 的默认配置。
type Options struct {
	// Method 默认 GET。
	Method string
	// Headers 会覆盖默认 UA/Accept。
	Headers http.Header
	// Timeout 是包含 body 读取在内的总超时；0 表示由调用方 context 控制。
	Timeout time.Duration
	// HeaderTimeout 只限制“拿到响应头”的时间；用于直播等长连接。
	HeaderTimeout time.Duration
	// AllowPrivate 放行内网地址（仅直播自建源场景，由部署者显式开启）。
	AllowPrivate bool
	// AllowBenchmarkNet 放行 198.18.0.0/15 基准测试段（仅 VOD，元数据仍拒绝）。
	AllowBenchmarkNet bool
	// SafeRedirects 为 true 时手动逐跳跟随并逐跳做 SSRF 校验。
	// 注意零值是 false（标准库自动跟随，不再逐跳校验）；所有调用点必须显式传 true。
	SafeRedirects bool
	// MaxBytes 限制非流式读取的 body 大小；0 表示默认 16 MiB，<0 表示不限制。
	MaxBytes int64
	// Retries 是额外的重试次数（仅幂等方法/幂等场景使用）。
	Retries int
}

// Response 是上游响应。调用方负责 Close。
type Response struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
	FinalURL   string
}

// Close 关闭响应体。
func (r *Response) Close() error {
	if r == nil || r.Body == nil {
		return nil
	}
	return r.Body.Close()
}

// ReadAll 读取最多 max 字节的 body。max<=0 时使用默认上限。
func (r *Response) ReadAll(max int64) ([]byte, error) {
	if max <= 0 {
		max = 16 << 20
	}
	return io.ReadAll(io.LimitReader(r.Body, max))
}

// Config 是 Client 的构造配置。
type Config struct {
	UserAgent             string
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	IdleConnTimeout       time.Duration
	ConnectTimeout        time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	AllowPrivate          bool
	AllowBenchmarkNet     bool
	Logger                *slog.Logger
}

// Doer 是出网客户端接口，便于测试注入。
type Doer interface {
	Do(ctx context.Context, rawURL string, opts Options) (*Response, error)
	GetJSON(ctx context.Context, rawURL string, opts Options) ([]byte, error)
	GetText(ctx context.Context, rawURL string, opts Options) (string, error)
}
