package upstream

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testLookup 是可控的假解析器，并记录调用次数以断言“每个请求只解析一次”。
type testLookup struct {
	mu    sync.Mutex
	calls int
	hosts map[string][]netip.Addr
	err   error
}

func newTestLookup(hosts map[string][]netip.Addr) *testLookup {
	normalized := make(map[string][]netip.Addr, len(hosts))
	for host, addrs := range hosts {
		normalized[strings.ToLower(host)] = addrs
	}
	return &testLookup{hosts: normalized}
}

func (l *testLookup) lookup(_ context.Context, host string) ([]netip.Addr, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	addrs, ok := l.hosts[strings.ToLower(host)]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return append([]netip.Addr(nil), addrs...), nil
}

func (l *testLookup) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func (l *testLookup) setError(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.err = err
}

// mustNewClient 构造测试客户端：静默日志、缩短重试退避。
func mustNewClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New() 返回错误: %v", err)
	}
	c, ok := d.(*Client)
	if !ok {
		t.Fatalf("New() 返回类型 = %T, 期望 *Client", d)
	}
	c.retryBase = time.Millisecond
	return c
}

func useLookup(c *Client, l *testLookup) {
	c.lookup = l.lookup
}

func serverPort(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("解析 httptest URL 失败: %v", err)
	}
	return u.Port()
}

func mustReadBody(t *testing.T, resp *Response) string {
	t.Helper()
	defer resp.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	return string(body)
}

func waitChan[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		var zero T
		t.Fatal("等待通道超时")
		return zero
	}
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
