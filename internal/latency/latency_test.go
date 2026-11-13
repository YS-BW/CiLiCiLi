package latency

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"cilicili/internal/upstream"
)

type fakeDoer struct {
	fn func(ctx context.Context, rawURL string, opts upstream.Options) (*upstream.Response, error)
}

func (f *fakeDoer) Do(ctx context.Context, rawURL string, opts upstream.Options) (*upstream.Response, error) {
	return f.fn(ctx, rawURL, opts)
}

func (f *fakeDoer) GetJSON(ctx context.Context, rawURL string, opts upstream.Options) ([]byte, error) {
	res, err := f.Do(ctx, rawURL, opts)
	if err != nil {
		return nil, err
	}
	defer res.Close()
	return res.ReadAll(0)
}

func (f *fakeDoer) GetText(ctx context.Context, rawURL string, opts upstream.Options) (string, error) {
	b, err := f.GetJSON(ctx, rawURL, opts)
	return string(b), err
}

func response(status int, body string) *upstream.Response {
	return &upstream.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/vnd.apple.mpegurl"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

func TestMeasureSuccess(t *testing.T) {
	doer := &fakeDoer{fn: func(context.Context, string, upstream.Options) (*upstream.Response, error) {
		return response(200, "#EXTM3U\n#EXTINF:1,\nseg.ts\n"), nil
	}}
	p := New(Options{Client: doer, Timeout: time.Second})

	m := p.Measure(context.Background(), "https://cdn.test/index.m3u8")
	if !m.OK || m.TimedOut {
		t.Fatalf("unexpected measurement: %+v", m)
	}
	if m.Status != 200 {
		t.Fatalf("status = %d", m.Status)
	}
	if m.MS < 0 || m.TTFBMs < 0 || m.MS < m.TTFBMs {
		t.Fatalf("bad timings: %+v", m)
	}
}

func TestMeasureHTTPFailure(t *testing.T) {
	doer := &fakeDoer{fn: func(context.Context, string, upstream.Options) (*upstream.Response, error) {
		return response(403, "forbidden"), nil
	}}
	p := New(Options{Client: doer, Timeout: time.Second})

	m := p.Measure(context.Background(), "https://cdn.test/index.m3u8")
	if m.OK || m.Error != "HTTP 403" {
		t.Fatalf("unexpected measurement: %+v", m)
	}
}

func TestMeasureTimeout(t *testing.T) {
	doer := &fakeDoer{fn: func(ctx context.Context, _ string, _ upstream.Options) (*upstream.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	p := New(Options{Client: doer, Timeout: 30 * time.Millisecond})

	m := p.Measure(context.Background(), "https://cdn.test/index.m3u8")
	if m.OK || !m.TimedOut {
		t.Fatalf("unexpected measurement: %+v", m)
	}
}

func TestMeasureCachesResult(t *testing.T) {
	calls := 0
	doer := &fakeDoer{fn: func(context.Context, string, upstream.Options) (*upstream.Response, error) {
		calls++
		return response(200, "#EXTM3U\n"), nil
	}}
	p := New(Options{Client: doer, Timeout: time.Second, CacheTTL: time.Minute})

	if m := p.Measure(context.Background(), "https://cdn.test/index.m3u8"); !m.OK {
		t.Fatalf("first call failed: %+v", m)
	}
	if m := p.Measure(context.Background(), "https://cdn.test/index.m3u8"); !m.OK {
		t.Fatalf("second call failed: %+v", m)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestMeasureInvalidURL(t *testing.T) {
	p := New(Options{Client: &fakeDoer{fn: func(context.Context, string, upstream.Options) (*upstream.Response, error) {
		return nil, errors.New("should not be called")
	}}})
	if m := p.Measure(context.Background(), "file:///tmp/a.m3u8"); m.OK || m.Error == "" {
		t.Fatalf("unexpected measurement: %+v", m)
	}
}
