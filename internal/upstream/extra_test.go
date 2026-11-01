package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "SSRF 拒绝", err: ErrBlocked, want: false},
		{name: "重定向超限", err: ErrTooManyRedirects, want: false},
		{name: "调用方取消", err: context.Canceled, want: false},
		{name: "总超时", err: context.DeadlineExceeded, want: false},
		{name: "响应头超时", err: errHeaderTimeout, want: true},
		{name: "连接重置", err: errors.New("connection reset by peer"), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryable(tc.err); got != tc.want {
				t.Fatalf("retryable(%v) = %v, 期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestSameHostname(t *testing.T) {
	a := mustURL(t, "https://Example.COM/a")
	b := mustURL(t, "http://example.com/b?x=1")
	c := mustURL(t, "https://other.com/a")
	if !sameHostname(a, b) {
		t.Fatal("同主机名（大小写/协议/端口不同）应判定为相同")
	}
	if sameHostname(a, c) {
		t.Fatal("不同主机名应判定为不同")
	}
	if sameHostname(nil, a) || sameHostname(a, nil) {
		t.Fatal("nil URL 应判定为不同")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析 URL 失败: %v", err)
	}
	return u
}

func TestValidMethod(t *testing.T) {
	valid := []string{"GET", "POST", "HEAD", "OPTIONS", "PATCH", "get", "X-Custom"}
	for _, method := range valid {
		if !validMethod(method) {
			t.Fatalf("validMethod(%q) = false, 期望 true", method)
		}
	}
	invalid := []string{"", "BAD METHOD", "GET\r\n", strings.Repeat("A", 33), "获 取", "GET\x00"}
	for _, method := range invalid {
		if validMethod(method) {
			t.Fatalf("validMethod(%q) = true, 期望 false", method)
		}
	}
}

func TestValidateTargetNil(t *testing.T) {
	if err := validateTarget(nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("validateTarget(nil) = %v, 期望 ErrBlocked", err)
	}
}

func TestResolveAndValidateEmptyResolution(t *testing.T) {
	lookup := newTestLookup(map[string][]netip.Addr{"empty.test": {}})
	u, err := parseTarget("http://empty.test/x")
	if err != nil {
		t.Fatalf("parseTarget: %v", err)
	}
	if _, err := resolveAndValidate(context.Background(), lookup.lookup, u, false, false); err == nil {
		t.Fatal("空解析结果应返回错误")
	}
}

func TestRedactURLTruncatesLongURL(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", 400)
	got := redactURL(long)
	if len(got) > 244 { // 240 字节 + UTF-8 省略号
		t.Fatalf("redactURL 长度 = %d, 期望截断到 240 附近", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("redactURL 截断后应带省略号: %q", got)
	}
}

func TestDefaultLookupLocalhost(t *testing.T) {
	addrs, err := defaultLookup(context.Background(), "localhost")
	if err != nil {
		t.Skipf("当前系统无法解析 localhost: %v", err)
	}
	if len(addrs) == 0 {
		t.Fatal("localhost 解析结果为空")
	}
	for _, addr := range addrs {
		if !addr.IsLoopback() {
			t.Fatalf("localhost 解析出非回环地址: %s", addr)
		}
	}
	if _, err := defaultLookup(context.Background(), "localhost."); err != nil {
		t.Fatalf("带尾点的 localhost 解析失败: %v", err)
	}
	if _, err := defaultLookup(context.Background(), "."); err == nil {
		t.Fatal("空主机名应返回错误")
	}
}

func TestNewWithZeroConfig(t *testing.T) {
	d, err := New(Config{})
	if err != nil {
		t.Fatalf("New(Config{}) 返回错误: %v", err)
	}
	c, ok := d.(*Client)
	if !ok {
		t.Fatalf("New 返回类型 = %T", d)
	}
	if c.logger == nil {
		t.Fatal("Logger 默认值未设置")
	}
}

func TestBackoffDefaultBase(t *testing.T) {
	c := mustNewClient(t, Config{})
	c.retryBase = 0
	for attempt := 1; attempt <= 8; attempt++ {
		if delay := c.backoff(attempt); delay <= 0 {
			t.Fatalf("attempt=%d delay=%v, 期望正数", attempt, delay)
		}
	}
}

func TestDrainAndCloseNil(t *testing.T) {
	drainAndClose(nil) // 不应 panic
}

func TestDialContextFallbackWithoutPlan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	u := mustURL(t, srv.URL)
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("拆分地址失败: %v", err)
	}

	lookup := newTestLookup(map[string][]netip.Addr{
		"fallback.test": {netip.MustParseAddr("127.0.0.1")},
	})
	c := mustNewClient(t, Config{AllowPrivate: true})
	useLookup(c, lookup)

	// 无固定计划时回退到现场解析（对应标准跟随模式换主机的场景）。
	conn, err := c.dialContext(context.Background(), "tcp", "fallback.test:"+port)
	if err != nil {
		t.Fatalf("dialContext 返回错误: %v", err)
	}
	_ = conn.Close()

	// 未知域名应返回解析错误。
	if _, err := c.dialContext(context.Background(), "tcp", "unknown.test:80"); err == nil {
		t.Fatal("未知域名应返回错误")
	}
	// 空计划（无效地址）应返回连接错误。
	if _, err := c.dialContext(context.Background(), "tcp", "fallback.test:1"); err == nil {
		t.Fatal("连不上的端口应返回错误")
	}
}

func TestDialAddrsFallsThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	// 第一个地址连不上（黑洞，靠 ConnectTimeout 快速失败），验证会继续尝试下一个。
	lookup := newTestLookup(map[string][]netip.Addr{
		"multi.test": {netip.MustParseAddr("127.0.0.99"), netip.MustParseAddr("127.0.0.1")},
	})
	c := mustNewClient(t, Config{AllowPrivate: true, ConnectTimeout: 150 * time.Millisecond})
	useLookup(c, lookup)

	text, err := c.GetText(context.Background(), "http://multi.test:"+serverPort(t, srv)+"/x", Options{})
	if err != nil {
		t.Fatalf("GetText 返回错误: %v", err)
	}
	if text != "ok" {
		t.Fatalf("text = %q", text)
	}
}

func TestCancelDuringRetryBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{AllowPrivate: true})
	c.retryBase = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, err := c.GetText(ctx, srv.URL, Options{Retries: 3})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, 期望 errors.Is(context.Canceled)", err)
	}
}

func TestRedirectInvalidLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://exa%zzmple.com/x")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{AllowPrivate: true})
	// 标准库会先于我们解析 Location，这里只要求错误可控、不 panic。
	_, err := c.GetText(context.Background(), srv.URL, Options{SafeRedirects: true})
	if err == nil {
		t.Fatal("非法 Location 应返回错误")
	}
}
