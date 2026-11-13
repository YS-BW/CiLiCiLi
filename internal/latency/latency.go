// Package latency 做轻量播放列表（m3u8）可达性测量：
// 记录首字节时间（TTFB）与读完播放列表的时间（完成时间）。
//
// 与 internal/quality 的区别：这里只请求一次播放列表，不解析码流、不下载分片，
// 用于详情页/一起看给每个片源快速标注延迟，避免拖慢首屏。
package latency

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"cilicili/internal/upstream"
)

const (
	// DefaultTimeout 是单条播放列表探测的总死线。
	DefaultTimeout = 4 * time.Second
	// DefaultCacheTTL 是成功结果的缓存时长；失败结果使用更短的 failureTTL。
	DefaultCacheTTL = 30 * time.Second
	failureTTL      = 10 * time.Second
	// maxBodyBytes 只读播放列表头部；m3u8 通常远小于该值。
	maxBodyBytes = 64 << 10
)

// Measurement 是一次播放列表探测结果。
type Measurement struct {
	OK       bool   `json:"ok"`
	TTFBMs   int64  `json:"ttfbMs"`
	MS       int64  `json:"ms"`
	Status   int    `json:"status,omitempty"`
	Error    string `json:"error,omitempty"`
	TimedOut bool   `json:"timedOut,omitempty"`
}

// Options 是探测器的构造参数。
type Options struct {
	Client   upstream.Doer
	UA       string
	Timeout  time.Duration
	CacheTTL time.Duration
	Logger   *slog.Logger
}

type cacheEntry struct {
	value   Measurement
	expires time.Time
}

// Prober 是并发安全的播放列表延迟测量器。
type Prober struct {
	opts Options

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// New 创建探测器；零值参数使用推荐默认值。
func New(opts Options) *Prober {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = DefaultCacheTTL
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Prober{opts: opts, cache: make(map[string]cacheEntry)}
}

// Measure 请求一次播放列表并返回测量结果。结果按 URL 做短缓存。
func (p *Prober) Measure(ctx context.Context, rawURL string) Measurement {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return Measurement{Error: "缺少播放地址"}
	}
	if p == nil || p.opts.Client == nil {
		return Measurement{Error: "延迟探测未配置"}
	}
	if !validURL(raw) {
		return Measurement{Error: "无效的播放地址"}
	}
	if m, ok := p.load(raw); ok {
		return m
	}

	m := p.measure(ctx, raw)
	p.store(raw, m)
	return m
}

func (p *Prober) measure(ctx context.Context, raw string) Measurement {
	if ctx == nil {
		ctx = context.Background()
	}
	start := time.Now()

	runCtx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()

	headers := make(http.Header, 3)
	if p.opts.UA != "" {
		headers.Set("User-Agent", p.opts.UA)
	}
	headers.Set("Accept", "*/*")

	res, err := p.opts.Client.Do(runCtx, raw, upstream.Options{
		Headers:       headers,
		Timeout:       p.opts.Timeout,
		HeaderTimeout: p.opts.Timeout,
		Retries:       0,
		SafeRedirects: true,
		MaxBytes:      0,
	})
	if err != nil {
		m := Measurement{MS: time.Since(start).Milliseconds()}
		if isTimeout(runCtx, err) {
			m.TimedOut = true
			m.Error = "探测超时"
		} else {
			m.Error = "请求失败"
		}
		return m
	}
	defer func() { _ = res.Close() }()

	m := Measurement{Status: res.StatusCode}
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		m.MS = time.Since(start).Milliseconds()
		m.Error = fmt.Sprintf("HTTP %d", res.StatusCode)
		return m
	}

	// 先读一次拿到首字节，再读剩余部分计算完成时间。
	buf := make([]byte, 32<<10)
	n, readErr := res.Body.Read(buf)
	ttfb := time.Since(start)
	m.TTFBMs = ttfb.Milliseconds()

	if readErr == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxBodyBytes-int64(n)))
	}
	m.MS = time.Since(start).Milliseconds()
	if m.MS < m.TTFBMs {
		m.MS = m.TTFBMs
	}

	if readErr != nil && !errors.Is(readErr, io.EOF) {
		if isTimeout(runCtx, readErr) {
			m.TimedOut = true
			m.Error = "探测超时"
		} else {
			m.Error = "读取失败"
		}
		return m
	}
	m.OK = true
	return m
}

func (p *Prober) load(raw string) (Measurement, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.cache[raw]
	if !ok {
		return Measurement{}, false
	}
	if time.Now().After(entry.expires) {
		delete(p.cache, raw)
		return Measurement{}, false
	}
	return entry.value, true
}

func (p *Prober) store(raw string, m Measurement) {
	ttl := p.opts.CacheTTL
	if !m.OK && ttl > failureTTL {
		ttl = failureTTL
	}
	if ttl <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// 简单上限，避免长期运行时缓存无限增长。
	if len(p.cache) > 2048 {
		now := time.Now()
		for k, v := range p.cache {
			if now.After(v.expires) {
				delete(p.cache, k)
			}
		}
	}
	p.cache[raw] = cacheEntry{value: m, expires: time.Now().Add(ttl)}
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func isTimeout(ctx context.Context, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline") || strings.Contains(msg, "超时")
}
