package recommend

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cilicili/internal/cache"
	"cilicili/internal/upstream"
)

// fakeCall 记录一次出网调用，便于断言请求地址与参数。
type fakeCall struct {
	URL  string
	Opts upstream.Options
}

// fakeDoer 是可编排的 upstream.Doer：由 fn 决定响应，测试全程不联网。
type fakeDoer struct {
	mu    sync.Mutex
	calls []fakeCall
	fn    func(ctx context.Context, rawURL string, opts upstream.Options) (*upstream.Response, error)
}

func (f *fakeDoer) Do(ctx context.Context, rawURL string, opts upstream.Options) (*upstream.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{URL: rawURL, Opts: opts})
	fn := f.fn
	f.mu.Unlock()
	return fn(ctx, rawURL, opts)
}

// setFn 替换行为，供测试中途模拟上游故障。
func (f *fakeDoer) setFn(fn func(ctx context.Context, rawURL string, opts upstream.Options) (*upstream.Response, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fn = fn
}

func (f *fakeDoer) GetJSON(ctx context.Context, rawURL string, opts upstream.Options) ([]byte, error) {
	resp, err := f.Do(ctx, rawURL, opts)
	if err != nil {
		return nil, err
	}
	defer resp.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &upstream.StatusError{StatusCode: resp.StatusCode, URL: resp.FinalURL}
	}
	return body, nil
}

func (f *fakeDoer) GetText(ctx context.Context, rawURL string, opts upstream.Options) (string, error) {
	body, err := f.GetJSON(ctx, rawURL, opts)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// callCount 返回出网调用次数。
func (f *fakeDoer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// lastCall 返回最近一次出网调用。
func (f *fakeDoer) lastCall(t *testing.T) fakeCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("没有任何出网调用")
	}
	return f.calls[len(f.calls)-1]
}

// allCalls 返回全部出网调用（副本）。
func (f *fakeDoer) allCalls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

// doerContent 返回固定 body 的 fakeDoer。
func doerContent(body []byte) *fakeDoer {
	return &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return &upstream.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			FinalURL:   rawURL,
		}, nil
	}}
}

// doerError 返回固定错误的 fakeDoer。
func doerError(err error) *fakeDoer {
	return &fakeDoer{fn: func(context.Context, string, upstream.Options) (*upstream.Response, error) {
		return nil, err
	}}
}

// doerStatus 返回固定状态码（空 body）的 fakeDoer。
func doerStatus(status int) *fakeDoer {
	return &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return &upstream.Response{
			StatusCode: status,
			Header:     http.Header{},
			Body:       io.NopCloser(bytes.NewReader(nil)),
			FinalURL:   rawURL,
		}, nil
	}}
}

// fakeCache 是 cache.Store 的内存实现，额外记录 Set 的 TTL 并支持手工「变陈旧」。
type fakeCache struct {
	mu      sync.Mutex
	entries map[string]*fakeCacheEntry
	gets    int
	sets    int
}

type fakeCacheEntry struct {
	value    []byte
	fresh    bool
	ttl      time.Duration
	staleTTL time.Duration
}

func newFakeCache() *fakeCache {
	return &fakeCache{entries: make(map[string]*fakeCacheEntry)}
}

func (c *fakeCache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	entry, ok := c.entries[key]
	if !ok || !entry.fresh {
		return nil, false
	}
	return append([]byte(nil), entry.value...), true
}

func (c *fakeCache) GetStale(key string) ([]byte, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false, false
	}
	return append([]byte(nil), entry.value...), entry.fresh, true
}

func (c *fakeCache) Set(key string, value []byte, ttl, staleTTL time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets++
	c.entries[key] = &fakeCacheEntry{
		value:    append([]byte(nil), value...),
		fresh:    true,
		ttl:      ttl,
		staleTTL: staleTTL,
	}
}

func (c *fakeCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

func (c *fakeCache) Stats() cache.Stats { return cache.Stats{} }

func (c *fakeCache) Close() error { return nil }

// expireAll 把所有条目标记为已过新鲜期（仍可用 GetStale 读到）。
func (c *fakeCache) expireAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		entry.fresh = false
	}
}

// windowOf 返回某 key 的缓存窗口。
func (c *fakeCache) windowOf(t *testing.T, key string) (time.Duration, time.Duration) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		t.Fatalf("缓存里没有 key %q", key)
	}
	return entry.ttl, entry.staleTTL
}

// setCount 返回 Set 次数。
func (c *fakeCache) setCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sets
}

// readFixture 读取 testdata 下的 fixture。
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取 fixture %s 失败: %v", name, err)
	}
	return data
}

// newTestFetcher 用固定 client/cache 构造 Fetcher（日志只打 Debug 以上，避免测试噪音）。
func newTestFetcher(client upstream.Doer, store cache.Store) *Fetcher {
	return New(Options{
		Client: client,
		Cache:  store,
		Logger: slog.New(slog.DiscardHandler),
	})
}
