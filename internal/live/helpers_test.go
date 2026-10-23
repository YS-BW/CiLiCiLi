package live

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"cilicili/internal/cache"
	"cilicili/internal/upstream"
)

// fakeDoer 是可编程的出网客户端替身：记录每次调用并交给 fn 决定响应。
type fakeDoer struct {
	mu    sync.Mutex
	calls []fakeCall
	fn    func(ctx context.Context, rawURL string, opts upstream.Options) (*upstream.Response, error)
}

type fakeCall struct {
	url    string
	header http.Header
}

func (f *fakeDoer) Do(ctx context.Context, rawURL string, opts upstream.Options) (*upstream.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{url: rawURL, header: opts.Headers.Clone()})
	fn := f.fn
	f.mu.Unlock()
	return fn(ctx, rawURL, opts)
}

func (f *fakeDoer) GetJSON(context.Context, string, upstream.Options) ([]byte, error) {
	return nil, io.ErrUnexpectedEOF
}

func (f *fakeDoer) GetText(context.Context, string, upstream.Options) (string, error) {
	return "", io.ErrUnexpectedEOF
}

func (f *fakeDoer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeDoer) callsFor(rawURL string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeCall, 0, len(f.calls))
	for _, call := range f.calls {
		if call.url == rawURL {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeDoer) urls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		out = append(out, call.url)
	}
	return out
}

// testResponse 构造上游响应。
func testResponse(status int, contentType, body, finalURL string) *upstream.Response {
	header := http.Header{}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return &upstream.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		FinalURL:   finalURL,
	}
}

// testResponseBody 构造带自定义 body 的上游响应（用于慢速/流式读取测试）。
func testResponseBody(status int, contentType string, body io.ReadCloser, finalURL string) *upstream.Response {
	header := http.Header{}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return &upstream.Response{StatusCode: status, Header: header, Body: body, FinalURL: finalURL}
}

func gzipBytes(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(body)); err != nil {
		t.Fatalf("gzip 写入失败: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip 关闭失败: %v", err)
	}
	return buf.Bytes()
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestService 构造带内存缓存的直播服务（测试用）。
func newTestService(t *testing.T, client upstream.Doer) (*Service, cache.Store) {
	t.Helper()
	store, err := cache.New(cache.Config{Logger: testLogger(), MaxMemoryEntries: 1000})
	if err != nil {
		t.Fatalf("创建测试缓存失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc := New(Options{Client: client, Cache: store, Logger: testLogger()})
	return svc, store
}

// fixedClock 返回可手动推进的时间源。
type fixedClock struct {
	mu sync.Mutex
	at time.Time
}

func newFixedClock(at time.Time) *fixedClock { return &fixedClock{at: at} }

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// slowReader 每块数据延迟一小段，用于吞吐采样与超时测试。
type slowReader struct {
	data  []byte
	chunk int
	delay time.Duration
	pos   int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	n := r.chunk
	if n > len(p) {
		n = len(p)
	}
	if r.pos+n > len(r.data) {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

func (r *slowReader) Close() error { return nil }

// blockingBody 读取时一直阻塞到 Close（模拟不响应超时的上游）。
type blockingBody struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingBody() *blockingBody { return &blockingBody{closed: make(chan struct{})} }

func (b *blockingBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *blockingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}
