package upstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewDefaults(t *testing.T) {
	c := mustNewClient(t, Config{})
	if c.userAgent != defaultUserAgent {
		t.Fatalf("userAgent = %q, 期望默认 UA", c.userAgent)
	}
	if c.allowPrivate {
		t.Fatal("默认不应放行内网")
	}
	if c.transport.MaxIdleConns != defaultMaxIdleConns ||
		c.transport.MaxIdleConnsPerHost != defaultMaxIdleConnsPerHost ||
		c.transport.IdleConnTimeout != defaultIdleConnTimeout {
		t.Fatalf("连接池默认值不对: %+v", c.transport)
	}
	if c.dialer.Timeout != defaultConnectTimeout {
		t.Fatalf("ConnectTimeout = %v, 期望 %v", c.dialer.Timeout, defaultConnectTimeout)
	}
	if c.transport.TLSHandshakeTimeout != defaultTLSHandshakeTimeout {
		t.Fatalf("TLSHandshakeTimeout = %v", c.transport.TLSHandshakeTimeout)
	}
	if c.transport.ResponseHeaderTimeout != 0 {
		t.Fatalf("ResponseHeaderTimeout 默认应为 0（交给 Options.HeaderTimeout），实际 %v", c.transport.ResponseHeaderTimeout)
	}
	if c.transport.Proxy != nil {
		t.Fatal("Transport.Proxy 必须为空，避免环境代理绕过固定 IP")
	}
	if !c.transport.ForceAttemptHTTP2 {
		t.Fatal("应启用 HTTP/2")
	}
	if c.transport.DisableCompression {
		t.Fatal("应允许 Transport 自动解压 gzip")
	}
}

func TestNewConfigOverrides(t *testing.T) {
	c := mustNewClient(t, Config{
		UserAgent:             "custom-ua",
		MaxIdleConns:          7,
		MaxIdleConnsPerHost:   3,
		IdleConnTimeout:       11 * time.Second,
		ConnectTimeout:        2 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 4 * time.Second,
		AllowPrivate:          true,
	})
	if c.userAgent != "custom-ua" || !c.allowPrivate {
		t.Fatalf("配置未生效: ua=%q allowPrivate=%v", c.userAgent, c.allowPrivate)
	}
	if c.transport.MaxIdleConns != 7 || c.transport.MaxIdleConnsPerHost != 3 ||
		c.transport.IdleConnTimeout != 11*time.Second ||
		c.transport.ResponseHeaderTimeout != 4*time.Second {
		t.Fatalf("Transport 配置未生效: %+v", c.transport)
	}
	if c.dialer.Timeout != 2*time.Second || c.transport.TLSHandshakeTimeout != 3*time.Second {
		t.Fatalf("拨号超时未生效: %v / %v", c.dialer.Timeout, c.transport.TLSHandshakeTimeout)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "负 ConnectTimeout", cfg: Config{ConnectTimeout: -time.Second}},
		{name: "负 TLSHandshakeTimeout", cfg: Config{TLSHandshakeTimeout: -time.Second}},
		{name: "负 IdleConnTimeout", cfg: Config{IdleConnTimeout: -time.Second}},
		{name: "负 ResponseHeaderTimeout", cfg: Config{ResponseHeaderTimeout: -time.Second}},
		{name: "负 MaxIdleConns", cfg: Config{MaxIdleConns: -1}},
		{name: "负 MaxIdleConnsPerHost", cfg: Config{MaxIdleConnsPerHost: -1}},
		{name: "UA 带换行", cfg: Config{UserAgent: "a\r\nb"}},
		{name: "UA 过长", cfg: Config{UserAgent: strings.Repeat("a", 600)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatalf("New(%+v) 期望返回错误", tc.cfg)
			}
		})
	}
}

func TestDoGetHeadersAndBody(t *testing.T) {
	type captured struct {
		method string
		ua     string
		accept string
		host   string
		path   string
		query  string
	}
	ch := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- captured{
			method: r.Method, ua: r.Header.Get("User-Agent"), accept: r.Header.Get("Accept"),
			host: r.Host, path: r.URL.Path, query: r.URL.RawQuery,
		}
		w.Header().Set("X-Test", "1")
		_, _ = io.WriteString(w, "hello")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{AllowPrivate: true, UserAgent: "cfg-ua"})
	resp, err := c.Do(context.Background(), srv.URL+"/api?wd=x", Options{})
	if err != nil {
		t.Fatalf("Do 返回错误: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, 期望 200", resp.StatusCode)
	}
	if resp.FinalURL != srv.URL+"/api?wd=x" {
		t.Fatalf("FinalURL = %q", resp.FinalURL)
	}
	if resp.Header.Get("X-Test") != "1" {
		t.Fatalf("响应头未透出: %v", resp.Header)
	}
	if body := mustReadBody(t, resp); body != "hello" {
		t.Fatalf("body = %q, 期望 hello", body)
	}
	got := waitChan(t, ch)
	if got.method != http.MethodGet || got.ua != "cfg-ua" || got.accept != "*/*" {
		t.Fatalf("默认请求头/方法不对: %+v", got)
	}
	if got.path != "/api" || got.query != "wd=x" {
		t.Fatalf("请求 URL 不对: %+v", got)
	}
}

func TestHeadersOverride(t *testing.T) {
	type captured struct{ ua, accept, referer string }
	ch := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- captured{
			ua: r.Header.Get("User-Agent"), accept: r.Header.Get("Accept"),
			referer: r.Header.Get("Referer"),
		}
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{AllowPrivate: true})
	headers := http.Header{}
	headers.Set("User-Agent", "opt-ua")
	headers.Set("Accept", "application/json")
	headers.Set("Referer", "https://example.com/")
	if _, err := c.GetText(context.Background(), srv.URL, Options{Headers: headers}); err != nil {
		t.Fatalf("GetText 返回错误: %v", err)
	}
	got := waitChan(t, ch)
	if got.ua != "opt-ua" || got.accept != "application/json" || got.referer != "https://example.com/" {
		t.Fatalf("Options.Headers 未覆盖默认头: %+v", got)
	}
}

func TestGetJSONAndGetText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})
	ctx := context.Background()

	body, err := c.GetJSON(ctx, srv.URL+"/json", Options{})
	if err != nil {
		t.Fatalf("GetJSON 返回错误: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}

	text, err := c.GetText(ctx, srv.URL+"/json", Options{})
	if err != nil {
		t.Fatalf("GetText 返回错误: %v", err)
	}
	if text != `{"ok":true}` {
		t.Fatalf("text = %q", text)
	}

	// Do 对 404 必须返回响应且 error == nil（代理需要透传状态码）。
	resp, err := c.Do(ctx, srv.URL+"/fail", Options{})
	if err != nil {
		t.Fatalf("Do 对 404 返回错误: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("StatusCode = %d", resp.StatusCode)
	}
	if body := mustReadBody(t, resp); body != `{"error":"not found"}` {
		t.Fatalf("404 body = %q", body)
	}

	// GetJSON/GetText 才做 2xx 校验。
	_, err = c.GetJSON(ctx, srv.URL+"/fail", Options{})
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("GetJSON 404 err = %v, 期望 *StatusError", err)
	}
	if statusErr.StatusCode != http.StatusNotFound {
		t.Fatalf("StatusError.StatusCode = %d", statusErr.StatusCode)
	}
	if statusErr.URL == "" || !strings.Contains(statusErr.URL, "/fail") {
		t.Fatalf("StatusError.URL = %q", statusErr.URL)
	}
	if _, err := c.GetText(ctx, srv.URL+"/fail", Options{}); !errors.As(err, &statusErr) {
		t.Fatalf("GetText 404 err = %v, 期望 *StatusError", err)
	}
}

func TestDoReturnsAllStatusCodes(t *testing.T) {
	statuses := []int{200, 204, 206, 301, 302, 304, 400, 403, 404, 500, 503}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := 0
		_, _ = fmt.Sscanf(r.URL.Query().Get("code"), "%d", &code)
		w.WriteHeader(code)
		if code != http.StatusNoContent && code != http.StatusNotModified {
			_, _ = io.WriteString(w, "body")
		}
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})
	for _, code := range statuses {
		t.Run(fmt.Sprintf("%d", code), func(t *testing.T) {
			resp, err := c.Do(context.Background(), fmt.Sprintf("%s/?code=%d", srv.URL, code), Options{})
			if err != nil {
				t.Fatalf("Do(%d) 返回错误: %v", code, err)
			}
			if resp.StatusCode != code {
				t.Fatalf("StatusCode = %d, 期望 %d", resp.StatusCode, code)
			}
			resp.Close()
		})
	}
}

func TestMaxBytes(t *testing.T) {
	payload := strings.Repeat("a", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})
	ctx := context.Background()

	if _, err := c.GetText(ctx, srv.URL, Options{MaxBytes: 10}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("MaxBytes=10 err = %v, 期望 ErrTooLarge", err)
	}
	if text, err := c.GetText(ctx, srv.URL, Options{MaxBytes: 100}); err != nil || text != payload {
		t.Fatalf("MaxBytes=100 text=%q err=%v", text, err)
	}
	if text, err := c.GetText(ctx, srv.URL, Options{MaxBytes: 101}); err != nil || text != payload {
		t.Fatalf("MaxBytes=101 text=%q err=%v", text, err)
	}
	if text, err := c.GetText(ctx, srv.URL, Options{MaxBytes: -1}); err != nil || text != payload {
		t.Fatalf("MaxBytes=-1 text=%q err=%v", text, err)
	}
	if _, err := c.GetJSON(ctx, srv.URL, Options{MaxBytes: 1}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("GetJSON MaxBytes=1 err = %v, 期望 ErrTooLarge", err)
	}
}

func TestMaxBytesDefaultLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("默认上限测试需要传输 16 MiB+ 数据")
	}
	// 比默认上限（16 MiB）多 1 字节。
	payload := bytes.Repeat([]byte("a"), defaultMaxBytes+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})
	if _, err := c.GetText(context.Background(), srv.URL, Options{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("默认上限 err = %v, 期望 ErrTooLarge", err)
	}
}

func TestTotalTimeoutIncludesBody(t *testing.T) {
	// 头立刻返回，但 body 迟迟不结束：Options.Timeout 必须覆盖 body 读取。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})

	start := time.Now()
	_, err := c.GetText(context.Background(), srv.URL, Options{Timeout: 80 * time.Millisecond})
	if err == nil {
		t.Fatal("期望超时错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, 期望 errors.Is(context.DeadlineExceeded)", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("超时耗时 %v, 期望远小于 1s", elapsed)
	}
}

func TestHeaderTimeoutOnlyCoversHeaders(t *testing.T) {
	t.Run("响应头按时返回则 body 慢也没关系", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(120 * time.Millisecond)
			_, _ = io.WriteString(w, "slow-body")
		}))
		defer srv.Close()
		c := mustNewClient(t, Config{AllowPrivate: true})
		text, err := c.GetText(context.Background(), srv.URL, Options{HeaderTimeout: 80 * time.Millisecond})
		if err != nil {
			t.Fatalf("GetText 返回错误: %v", err)
		}
		if text != "slow-body" {
			t.Fatalf("text = %q", text)
		}
	})

	t.Run("响应头迟迟不来则超时", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, "late")
		}))
		defer srv.Close()
		c := mustNewClient(t, Config{AllowPrivate: true})
		start := time.Now()
		_, err := c.GetText(context.Background(), srv.URL, Options{HeaderTimeout: 60 * time.Millisecond})
		if err == nil {
			t.Fatal("期望超时错误")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, 期望 errors.Is(context.DeadlineExceeded)", err)
		}
		if !strings.Contains(err.Error(), "响应头") {
			t.Fatalf("err = %v, 期望提示响应头超时", err)
		}
		if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
			t.Fatalf("超时耗时 %v, 期望远小于 800ms", elapsed)
		}
	})
}

func TestContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.GetText(ctx, srv.URL, Options{})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	err := waitChan(t, done)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, 期望 errors.Is(context.Canceled)", err)
	}
}

func TestRetryOn5xx(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := atomic.AddInt32(&count, 1); n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})

	text, err := c.GetText(context.Background(), srv.URL, Options{Retries: 2})
	if err != nil {
		t.Fatalf("GetText 返回错误: %v", err)
	}
	if text != "ok" {
		t.Fatalf("text = %q", text)
	}
	if got := atomic.LoadInt32(&count); got != 3 {
		t.Fatalf("请求次数 = %d, 期望 3", got)
	}
}

func TestRetryExhausted5xxReturnsResponse(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "bad")
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})
	ctx := context.Background()

	resp, err := c.Do(ctx, srv.URL, Options{Retries: 1})
	if err != nil {
		t.Fatalf("Do 返回错误: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("StatusCode = %d", resp.StatusCode)
	}
	if body := mustReadBody(t, resp); body != "bad" {
		t.Fatalf("body = %q", body)
	}
	if got := atomic.LoadInt32(&count); got != 2 {
		t.Fatalf("请求次数 = %d, 期望 2", got)
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})

	_, err := c.GetJSON(context.Background(), srv.URL, Options{Retries: 3})
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound {
		t.Fatalf("err = %v, 期望 404 *StatusError", err)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("请求次数 = %d, 期望 1（4xx 不重试）", got)
	}
}

func TestRetryOnConnectionError(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})

	_, err := c.GetText(context.Background(), srv.URL, Options{Retries: 1})
	if err == nil {
		t.Fatal("期望连接错误")
	}
	if got := atomic.LoadInt32(&count); got != 2 {
		t.Fatalf("请求次数 = %d, 期望 2（连接错误重试一次）", got)
	}
}

func TestDNSResultIsPinnedForConnection(t *testing.T) {
	type captured struct {
		host string
		path string
	}
	ch := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- captured{host: r.Host, path: r.URL.Path}
		_, _ = io.WriteString(w, "pinned")
	}))
	defer srv.Close()

	// 域名解析到 127.0.0.1：建连使用解析结果，Host/SNI 仍是原域名。
	lookup := newTestLookup(map[string][]netip.Addr{
		"pinned.test": {netip.MustParseAddr("127.0.0.1")},
	})
	c := mustNewClient(t, Config{AllowPrivate: true})
	useLookup(c, lookup)

	url := "http://pinned.test:" + serverPort(t, srv) + "/x"
	text, err := c.GetText(context.Background(), url, Options{})
	if err != nil {
		t.Fatalf("GetText 返回错误: %v", err)
	}
	if text != "pinned" {
		t.Fatalf("text = %q", text)
	}
	got := waitChan(t, ch)
	if expected := strings.TrimSuffix(strings.TrimPrefix(url, "http://"), "/x"); got.host != expected {
		t.Fatalf("Host = %q, 期望原域名 %q", got.host, expected)
	}
	if lookup.callCount() != 1 {
		t.Fatalf("DNS 解析次数 = %d, 期望 1（不得重复解析）", lookup.callCount())
	}
}

func TestDNSResolvesToPrivateIsBlocked(t *testing.T) {
	lookup := newTestLookup(map[string][]netip.Addr{
		"evil.test": {netip.MustParseAddr("10.1.2.3")},
	})
	c := mustNewClient(t, Config{})
	useLookup(c, lookup)

	_, err := c.GetText(context.Background(), "http://evil.test/x", Options{})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, 期望 ErrBlocked", err)
	}
}

func TestPartialResolutionToPrivateIsBlocked(t *testing.T) {
	lookup := newTestLookup(map[string][]netip.Addr{
		"mixed.test": {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")},
	})
	c := mustNewClient(t, Config{})
	useLookup(c, lookup)

	if _, err := c.GetText(context.Background(), "http://mixed.test/x", Options{}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, 期望 ErrBlocked", err)
	}
}

func TestLiteralAddressBlocked(t *testing.T) {
	c := mustNewClient(t, Config{})
	urls := []string{
		"http://127.0.0.1:1/",
		"http://169.254.169.254/latest/meta-data/",
		"https://[::1]:443/",
		"http://[::ffff:127.0.0.1]:80/",
		"http://[::ffff:10.0.0.1]:80/",
		"http://10.0.0.1/",
		"http://172.16.0.1/",
		"http://192.168.1.1/",
		"http://100.64.0.1/",
		"http://0.0.0.0/",
		"http://224.0.0.1/",
		"http://[fd00::1]:80/",
		"http://[fe80::1]:80/",
		"ftp://example.com/x",
		"file:///etc/passwd",
	}
	for _, raw := range urls {
		t.Run(raw, func(t *testing.T) {
			if _, err := c.GetText(context.Background(), raw, Options{}); !errors.Is(err, ErrBlocked) {
				t.Fatalf("GetText(%q) err = %v, 期望 ErrBlocked", raw, err)
			}
		})
	}
}

func TestAllowPrivateOptionsOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "local")
	}))
	defer srv.Close()

	// Client 默认拒绝内网，单次请求用 Options.AllowPrivate 放行（直播场景）。
	c := mustNewClient(t, Config{})
	text, err := c.GetText(context.Background(), srv.URL, Options{AllowPrivate: true})
	if err != nil {
		t.Fatalf("GetText 返回错误: %v", err)
	}
	if text != "local" {
		t.Fatalf("text = %q", text)
	}

	// 反向：Client 放行内网时不需要 Options。
	c2 := mustNewClient(t, Config{AllowPrivate: true})
	if _, err := c2.GetText(context.Background(), srv.URL, Options{}); err != nil {
		t.Fatalf("Config.AllowPrivate 未生效: %v", err)
	}
}

func TestMetadataAlwaysBlocked(t *testing.T) {
	c := mustNewClient(t, Config{AllowPrivate: true})
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://169.254.0.23/latest/",
		"http://100.100.100.200/latest/",
		"http://[fd00:ec2::254]/latest/",
	} {
		if _, err := c.GetText(context.Background(), raw, Options{AllowPrivate: true}); !errors.Is(err, ErrBlocked) {
			t.Fatalf("GetText(%q) err = %v, 期望 ErrBlocked（元数据地址永远拒绝）", raw, err)
		}
	}
}

func TestGzipAutoDecompress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "缺少 Accept-Encoding")
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(`{"ok":true}`))
		_ = gz.Close()
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{AllowPrivate: true})
	resp, err := c.Do(context.Background(), srv.URL, Options{})
	if err != nil {
		t.Fatalf("Do 返回错误: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d", resp.StatusCode)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("自动解压后不应残留 Content-Encoding, 实际 %q", enc)
	}
	if body := mustReadBody(t, resp); body != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
}

func TestLogsDoNotLeakSensitiveQuery(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{AllowPrivate: true, Logger: logger})
	secretURL := srv.URL + "/api?token=super-secret-value&wd=abc"
	if _, err := c.GetText(context.Background(), secretURL, Options{}); err != nil {
		t.Fatalf("GetText 返回错误: %v", err)
	}
	logs := buf.String()
	if logs == "" {
		t.Fatal("期望产生 Debug 日志")
	}
	if strings.Contains(logs, "super-secret-value") {
		t.Fatalf("日志泄露了 query 值:\n%s", logs)
	}
	if !strings.Contains(logs, "token=<redacted>") {
		t.Fatalf("日志缺少脱敏占位:\n%s", logs)
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		raw      string
		mustNot  []string
		mustHave []string
	}{
		{
			raw:      "https://example.com/api?token=abc&wd=foo",
			mustNot:  []string{"abc", "foo"},
			mustHave: []string{"example.com", "/api", "token=<redacted>", "wd=<redacted>"},
		},
		{
			raw:      "https://user:password@example.com/p?x=1",
			mustNot:  []string{"password"},
			mustHave: []string{"example.com"},
		},
		{
			raw:      "https://example.com/plain",
			mustHave: []string{"https://example.com/plain"},
		},
		{
			raw:     "https://example.com/p?%%%",
			mustNot: []string{"%%%"},
		},
		{
			raw:     "://bad url",
			mustNot: []string{"bad url"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			got := redactURL(tc.raw)
			for _, s := range tc.mustNot {
				if strings.Contains(got, s) {
					t.Fatalf("redactURL(%q) = %q, 不应包含 %q", tc.raw, got, s)
				}
			}
			for _, s := range tc.mustHave {
				if !strings.Contains(got, s) {
					t.Fatalf("redactURL(%q) = %q, 应包含 %q", tc.raw, got, s)
				}
			}
		})
	}
}

func TestSafeErrTextRedactsURL(t *testing.T) {
	raw := "https://example.com/api?token=secret-value&wd=abc"
	err := &url.Error{Op: "Get", URL: raw, Err: errors.New("connection refused")}
	got := safeErrText(err)
	if strings.Contains(got, "secret-value") || strings.Contains(got, "abc") {
		t.Fatalf("safeErrText 泄露 query: %q", got)
	}
	if !strings.Contains(got, "token=<redacted>") || !strings.Contains(got, "connection refused") {
		t.Fatalf("safeErrText = %q, 期望含脱敏 URL 与原错误", got)
	}
	if safeErrText(nil) != "" {
		t.Fatalf("safeErrText(nil) = %q", safeErrText(nil))
	}
}

func TestBackoffBounds(t *testing.T) {
	c := mustNewClient(t, Config{})
	for attempt := 1; attempt <= 6; attempt++ {
		delay := c.backoff(attempt)
		if delay <= 0 {
			t.Fatalf("attempt=%d delay=%v, 期望正数", attempt, delay)
		}
		if delay > maxRetryDelay+maxRetryDelay/2 {
			t.Fatalf("attempt=%d delay=%v, 超过上限", attempt, delay)
		}
	}
}

func TestSleepCtxCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, 期望 context.Canceled", err)
	}
}

func TestDialPlanContext(t *testing.T) {
	plan := &dialPlan{host: "example.test", addrs: []netip.Addr{netip.MustParseAddr("8.8.8.8")}}
	ctx := withDialPlan(context.Background(), plan)
	got := dialPlanFrom(ctx)
	if got == nil || got.host != plan.host || len(got.addrs) != 1 {
		t.Fatalf("dialPlanFrom = %+v", got)
	}
	if dialPlanFrom(context.Background()) != nil {
		t.Fatal("空上下文不应有固定计划")
	}
}

func TestNilContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})
	//nolint:staticcheck // 故意传 nil context，验证不会 panic。
	if _, err := c.GetText(nil, srv.URL, Options{}); err != nil {
		t.Fatalf("GetText(nil ctx) 返回错误: %v", err)
	}
}

func TestInvalidMethodRejected(t *testing.T) {
	c := mustNewClient(t, Config{AllowPrivate: true})
	_, err := c.Do(context.Background(), "http://127.0.0.1:1/", Options{Method: "BAD METHOD"})
	if err == nil {
		t.Fatal("非法方法应返回错误")
	}
	if errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, 期望方法校验错误", err)
	}
}

func TestMethodIsNormalizedAndSent(t *testing.T) {
	ch := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- r.Method
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{AllowPrivate: true})
	if _, err := c.GetText(context.Background(), srv.URL, Options{Method: "post"}); err != nil {
		t.Fatalf("GetText 返回错误: %v", err)
	}
	if got := waitChan(t, ch); got != http.MethodPost {
		t.Fatalf("method = %q, 期望 POST", got)
	}
}

func TestDoDoesNotApplyMaxBytes(t *testing.T) {
	payload := strings.Repeat("b", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{AllowPrivate: true})
	// MaxBytes 只作用于 GetJSON/GetText；Do 的 body 交给调用方流式读取，不设上限。
	resp, err := c.Do(context.Background(), srv.URL, Options{MaxBytes: 5})
	if err != nil {
		t.Fatalf("Do 返回错误: %v", err)
	}
	if body := mustReadBody(t, resp); body != payload {
		t.Fatalf("Do body 长度 = %d, 期望 %d", len(body), len(payload))
	}
}

func TestRetryOnHeaderTimeout(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := atomic.AddInt32(&count, 1); n == 1 {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{AllowPrivate: true})
	text, err := c.GetText(context.Background(), srv.URL, Options{HeaderTimeout: 80 * time.Millisecond, Retries: 1})
	if err != nil {
		t.Fatalf("GetText 返回错误: %v", err)
	}
	if text != "ok" {
		t.Fatalf("text = %q", text)
	}
	if got := atomic.LoadInt32(&count); got != 2 {
		t.Fatalf("请求次数 = %d, 期望 2（响应头超时重试）", got)
	}
}

func TestConcurrentDo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{AllowPrivate: true})
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				if text, err := c.GetText(ctx, srv.URL, Options{}); err != nil || text != "ok" {
					t.Errorf("并发请求失败: text=%q err=%v", text, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestTLSUsesOriginalHostnameForSNI(t *testing.T) {
	sni := make(chan string, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tls-ok")
	}))
	srv.Config.ErrorLog = stdlog.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			select {
			case sni <- hello.ServerName:
			default:
			}
			return nil, errors.New("测试中故意不完成 TLS 握手")
		},
	}
	srv.StartTLS()
	defer srv.Close()

	lookup := newTestLookup(map[string][]netip.Addr{
		"secure.test": {netip.MustParseAddr("127.0.0.1")},
	})
	c := mustNewClient(t, Config{AllowPrivate: true})
	useLookup(c, lookup)

	url := "https://secure.test:" + serverPort(t, srv) + "/x"
	if _, err := c.GetText(context.Background(), url, Options{}); err == nil {
		t.Fatal("期望 TLS 握手失败（测试证书未受信）")
	}
	// 连接虽然拨到了固定 IP，TLS 的 SNI 必须仍是 URL 里的原始域名。
	if got := waitChan(t, sni); got != "secure.test" {
		t.Fatalf("SNI = %q, 期望 secure.test", got)
	}
}
