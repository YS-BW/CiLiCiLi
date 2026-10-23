package subscribe

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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

// doerText 返回固定文本的 fakeDoer。
func doerText(body string) *fakeDoer {
	return &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return &upstream.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
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

// readFixture 读取 testdata 下的 fixture。
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取 fixture %s 失败: %v", name, err)
	}
	return data
}
