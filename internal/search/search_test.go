package search

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
	"testing"
	"time"

	"cilicili/internal/cache"
	"cilicili/internal/model"
	"cilicili/internal/source"
	"cilicili/internal/upstream"
)

type fakeDoer struct {
	mu    sync.Mutex
	calls []string
	fn    func(ctx context.Context, rawURL string, opts upstream.Options) (*upstream.Response, error)
}

func (f *fakeDoer) Do(ctx context.Context, rawURL string, opts upstream.Options) (*upstream.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, rawURL)
	f.mu.Unlock()
	return f.fn(ctx, rawURL, opts)
}

func (f *fakeDoer) GetJSON(context.Context, string, upstream.Options) ([]byte, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeDoer) GetText(context.Context, string, upstream.Options) (string, error) {
	return "", errors.New("not implemented")
}

func (f *fakeDoer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func jsonResponse(t *testing.T, body string) *upstream.Response {
	t.Helper()
	return &upstream.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		FinalURL:   "http://final.test/",
	}
}

func cmsBody(items ...string) string {
	return `{"code":1,"list":[` + strings.Join(items, ",") + `],"pagecount":"1"}`
}

func item(id, name, typeName string) string {
	return fmt.Sprintf(`{"vod_id":%q,"vod_name":%q,"type_name":%q,"vod_pic":"https://img.test/%s.jpg"}`, id, name, typeName, id)
}

func newSearcher(t *testing.T, doer upstream.Doer, cfg Config) (*Searcher, cache.Store, source.HealthStore) {
	t.Helper()
	store, err := cache.New(cache.Config{MaxMemoryEntries: 128})
	if err != nil {
		t.Fatal(err)
	}
	health, err := source.NewHealthStore(source.HealthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(); _ = health.Close() })
	if cfg.UserAgent == "" {
		cfg.UserAgent = "test-agent"
	}
	return New(doer, store, health, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg), store, health
}

func srcA() model.SourceConfig {
	return model.SourceConfig{Key: "a", Name: "源A", URL: "https://a.test/api.php/provide/vod"}
}

func srcB() model.SourceConfig {
	return model.SourceConfig{Key: "b", Name: "源B", URL: "https://b.test/api.php/provide/vod"}
}

func TestSearchAggregatesFiltersAndCaches(t *testing.T) {
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "a.test"):
			return jsonResponse(t, cmsBody(
				item("1", "庆余年", "国产剧"),
				item("2", "庆余年 第二季", "国产剧"),
				item("3", "无关影片", "国产剧"),
				item("4", "福利片", "福利片"),
			)), nil
		case strings.Contains(rawURL, "b.test"):
			return jsonResponse(t, cmsBody(
				item("1", "庆余年", "国产剧"),
				item("9", "庆余年2", "国产剧"),
			)), nil
		default:
			t.Fatalf("unexpected url %s", rawURL)
			return nil, errors.New("unexpected")
		}
	}}
	s, _, _ := newSearcher(t, doer, Config{
		MaxPages: 1, SourceTimeout: time.Second, PageTimeout: time.Second,
		HardDeadline: 2 * time.Second, SoftDeadline: time.Second,
		CacheFresh: time.Minute, CacheStale: time.Minute, SourceFresh: time.Minute, SourceStale: time.Minute,
		FailCacheTTL: time.Second,
	})

	resp, err := s.Search(context.Background(), "庆余年", []model.SourceConfig{srcA(), srcB()}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.List) != 4 {
		t.Fatalf("list = %d: %+v", len(resp.List), resp.List)
	}
	if resp.List[0].Name == "无关影片" || resp.List[0].TypeName == "福利片" {
		t.Fatalf("filters not applied: %+v", resp.List)
	}
	if resp.List[0].Name != "庆余年" {
		t.Fatalf("exact match should be first, got %+v", resp.List[0])
	}
	if len(resp.Failures) != 0 {
		t.Fatalf("failures = %+v", resp.Failures)
	}

	// 搜索级缓存命中：不应再打上游。
	before := doer.callCount()
	resp2, err := s.Search(context.Background(), "庆余年", []model.SourceConfig{srcA(), srcB()}, true)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.List == nil || len(resp2.List) != 4 {
		t.Fatalf("cached list = %+v", resp2.List)
	}
	if doer.callCount() != before {
		t.Fatalf("cache miss: calls %d -> %d", before, doer.callCount())
	}

	// 换一个源集合（搜索 key 不同），源级缓存应避免重新请求 A。
	resp3, err := s.Search(context.Background(), "庆余年", []model.SourceConfig{srcA()}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp3.List) != 2 {
		t.Fatalf("source-cache list = %d", len(resp3.List))
	}
	if doer.callCount() != before {
		t.Fatalf("source cache miss: calls %d -> %d", before, doer.callCount())
	}
}

func TestSearchPages(t *testing.T) {
	var mu sync.Mutex
	pages := map[string]int{}
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		u, _ := url.Parse(rawURL)
		pg := u.Query().Get("pg")
		mu.Lock()
		pages[pg]++
		mu.Unlock()
		return jsonResponse(t, `{"code":1,"pagecount":"3","list":[`+item("p"+pg, "庆余年", "剧集")+`]}`), nil
	}}
	s, _, _ := newSearcher(t, doer, Config{
		MaxPages: 3, SourceTimeout: 2 * time.Second, PageTimeout: time.Second,
		HardDeadline: 3 * time.Second, SoftDeadline: time.Second,
		CacheFresh: time.Minute, CacheStale: time.Minute, SourceFresh: time.Minute, SourceStale: time.Minute,
		FailCacheTTL: time.Second,
	})
	resp, err := s.Search(context.Background(), "庆余年", []model.SourceConfig{srcA()}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.List) != 3 {
		t.Fatalf("list = %d (%+v)", len(resp.List), resp.List)
	}
	for _, pg := range []string{"1", "2", "3"} {
		if pages[pg] != 1 {
			t.Fatalf("page %s calls = %d", pg, pages[pg])
		}
	}
}

func TestStreamSoftDeadline(t *testing.T) {
	doer := &fakeDoer{fn: func(ctx context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		if strings.Contains(rawURL, "a.test") {
			return jsonResponse(t, cmsBody(item("1", "庆余年", "剧集"))), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	s, _, _ := newSearcher(t, doer, Config{
		MaxPages: 1, SourceTimeout: 150 * time.Millisecond, PageTimeout: 500 * time.Millisecond,
		SoftDeadline: 50 * time.Millisecond, HardDeadline: 400 * time.Millisecond,
		CacheFresh: time.Minute, CacheStale: time.Minute, SourceFresh: time.Minute, SourceStale: time.Minute,
		FailCacheTTL: time.Second,
	})

	events := make([]string, 0, 4)
	resp, err := s.Stream(context.Background(), "庆余年", []model.SourceConfig{srcA(), srcB()}, true, func(ev any) error {
		switch e := ev.(type) {
		case model.SourceEvent:
			events = append(events, "source:"+e.SourceKey)
		case model.PartialDoneEvent:
			events = append(events, "partial")
		case model.DoneEvent:
			events = append(events, "done")
		default:
			t.Fatalf("unknown event %T", ev)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(events, "partial") {
		t.Fatalf("missing partial_done: %v", events)
	}
	if events[len(events)-1] != "done" {
		t.Fatalf("last event should be done: %v", events)
	}
	if len(resp.Failures) != 1 || !resp.Failures[0].TimedOut {
		t.Fatalf("failures = %+v", resp.Failures)
	}
}

func TestHealthSkipAndFailureCache(t *testing.T) {
	doer := &fakeDoer{fn: func(_ context.Context, _ string, _ upstream.Options) (*upstream.Response, error) {
		return nil, errors.New("dial tcp 10.0.0.1:80: connection reset by peer")
	}}
	s, _, health := newSearcher(t, doer, Config{
		MaxPages: 1, SourceTimeout: time.Second, PageTimeout: time.Second,
		HardDeadline: 2 * time.Second, SoftDeadline: time.Second,
		CacheFresh: time.Minute, CacheStale: time.Minute, SourceFresh: time.Minute, SourceStale: time.Minute,
		FailCacheTTL: 50 * time.Millisecond,
	})
	src := srcA()

	// 连续两次失败后熔断；第三次不应再发出网络请求。
	for i := 0; i < 2; i++ {
		resp, err := s.Search(context.Background(), fmt.Sprintf("关键词%d", i), []model.SourceConfig{src}, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Failures) != 1 {
			t.Fatalf("failures = %+v", resp.Failures)
		}
		if strings.Contains(resp.Failures[0].Error, "10.0.0.1") {
			t.Fatalf("error leaks internal address: %q", resp.Failures[0].Error)
		}
		if resp.Failures[0].Error != "连接被重置" {
			t.Fatalf("error = %q, want 连接被重置", resp.Failures[0].Error)
		}
	}
	if !health.IsOpen(src.Key, time.Now()) {
		t.Fatal("source should be open after two failures")
	}
	before := doer.callCount()
	resp, err := s.Search(context.Background(), "第三次", []model.SourceConfig{src}, true)
	if err != nil {
		t.Fatal(err)
	}
	if doer.callCount() != before {
		t.Fatalf("open source should be skipped, calls %d -> %d", before, doer.callCount())
	}
	if len(resp.Failures) != 1 || !strings.Contains(resp.Failures[0].Error, "熔断") {
		t.Fatalf("failures = %+v", resp.Failures)
	}
}

func TestValidateInput(t *testing.T) {
	s, _, _ := newSearcher(t, &fakeDoer{}, Config{})
	if _, err := s.Search(context.Background(), "", []model.SourceConfig{srcA()}, true); err == nil {
		t.Fatal("empty keyword should fail")
	}
	if _, err := s.Search(context.Background(), "x", nil, true); err == nil {
		t.Fatal("empty sources should fail")
	}
	if _, err := s.Search(context.Background(), strings.Repeat("长", 101), []model.SourceConfig{srcA()}, true); err == nil {
		t.Fatal("too long keyword should fail")
	}
}

func TestCacheKeyStable(t *testing.T) {
	a := searchCacheKey("庆余年", []model.SourceConfig{srcA(), srcB()}, true)
	b := searchCacheKey("庆余年", []model.SourceConfig{srcB(), srcA()}, true)
	if a != b {
		t.Fatalf("key should not depend on source order")
	}
	c := searchCacheKey("庆余年", []model.SourceConfig{srcA()}, true)
	if a == c {
		t.Fatal("key should depend on source set")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
