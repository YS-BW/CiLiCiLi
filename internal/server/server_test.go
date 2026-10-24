package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cilicili/internal/api"
	"cilicili/internal/auth"
	"cilicili/internal/cache"
	"cilicili/internal/config"
	"cilicili/internal/detail"
	"cilicili/internal/media"
	"cilicili/internal/model"
	"cilicili/internal/probe"
	"cilicili/internal/proxy"
	"cilicili/internal/search"
	"cilicili/internal/server"
	"cilicili/internal/source"
	"cilicili/internal/sources"
	"cilicili/internal/upstream"
	"cilicili/internal/userdata"
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

func body(status int, contentType, body string) *upstream.Response {
	return bodyRaw(status, contentType, []byte(body))
}

func bodyRaw(status int, contentType string, raw []byte) *upstream.Response {
	return &upstream.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(bytes.NewReader(raw)),
		FinalURL:   "https://final.test/",
	}
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestServer(t *testing.T) (*httptest.Server, *auth.Manager) {
	t.Helper()
	pngData := testPNG(t)
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist&wd=test"):
			return body(200, "application/json", `{"code":1,"pagecount":"1","list":[{"vod_id":"1","vod_name":"测试资源","type_name":"电影"}]}`), nil
		case strings.Contains(rawURL, "ac=videolist&ids="):
			return body(200, "application/json", `{"code":1,"list":[{"vod_id":"1","vod_name":"庆余年","vod_play_from":"lineA$$$lineB","vod_play_url":"第1集$https://cdn.test/1.m3u8#第2集$https://cdn.test/2.m3u8$$$正片$https://cdn2.test/full.mp4"}]}`), nil
		case strings.Contains(rawURL, "ac=videolist&wd="):
			return body(200, "application/json", `{"code":1,"pagecount":"1","list":[{"vod_id":"1","vod_name":"庆余年","vod_pic":"https://img.test/1.jpg","type_name":"剧集","vod_year":"2019"}]}`), nil
		case strings.HasSuffix(rawURL, ".jpg"):
			return bodyRaw(200, "image/png", pngData), nil
		default:
			return nil, errors.New("unexpected url: " + rawURL)
		}
	}}

	store, err := cache.New(cache.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	health, err := source.NewHealthStore(source.HealthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = health.Close() })

	src := model.SourceConfig{Key: "a", Name: "源A", URL: "https://a.test/api.php/provide/vod"}
	cfg := &config.Config{
		Version:             "test",
		Password:            "pw",
		UserAgent:           "test-ua",
		DefaultSources:      []model.SourceConfig{src},
		SearchMaxPages:      1,
		SearchSourceTimeout: time.Second,
		SearchPageTimeout:   time.Second,
		SearchSoftDeadline:  100 * time.Millisecond,
		SearchHardDeadline:  time.Second,
		SearchCacheFresh:    time.Minute,
		SearchCacheStale:    time.Minute,
		DetailCacheFresh:    time.Minute,
		DetailCacheStale:    time.Minute,
		RequestTimeout:      time.Second,
		CorsOrigins:         []string{"http://localhost:3000"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authMgr := auth.New(cfg.Password, "secret", time.Hour)
	sourceStore, err := sources.New(filepath.Join(t.TempDir(), "sources.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sourceStore.Close() })
	if err := sourceStore.Seed(cfg.DefaultSources); err != nil {
		t.Fatal(err)
	}
	enabledSources, err := sourceStore.ListEnabled()
	if err != nil {
		t.Fatal(err)
	}
	registry := source.NewRegistry(enabledSources, health)
	searcher := search.New(doer, store, health, log, search.Config{
		MaxPages: 1, SourceTimeout: time.Second, PageTimeout: time.Second,
		SoftDeadline: 100 * time.Millisecond, HardDeadline: time.Second,
		CacheFresh: time.Minute, CacheStale: time.Minute, SourceFresh: time.Minute, SourceStale: time.Minute,
		FailCacheTTL: time.Second, UserAgent: cfg.UserAgent,
	})
	resolver := detail.New(doer, store, log, detail.Config{Timeout: time.Second, CacheFresh: time.Minute, CacheStale: time.Minute})
	proxyHandler := proxy.New(proxy.Options{
		Client: doer, UA: cfg.UserAgent, Auth: authMgr, Logger: log,
		AnonymousImageHosts: []string{"img.test"}, HeaderTimeout: time.Second,
	})
	liveHandler := proxy.NewLive(proxy.Options{Client: doer, UA: cfg.UserAgent, Auth: authMgr, Logger: log, HeaderTimeout: time.Second})
	userStore, err := userdata.New(filepath.Join(t.TempDir(), "userdata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = userStore.Close() })
	mediaStore, err := media.NewStore(filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mediaStore.Close() })
	mediaMgr, err := media.NewManager(media.Config{
		Store:     mediaStore,
		Client:    doer,
		CoverDir:  filepath.Join(t.TempDir(), "covers"),
		Logger:    log,
		CoverBase: "/api/media",
	})
	if err != nil {
		t.Fatal(err)
	}
	prober := probe.New(doer, registry, health, sourceStore, log, probe.Config{Timeout: time.Second, Concurrency: 2})
	prober.Start(context.Background())
	t.Cleanup(prober.Stop)
	apiHandler := api.New(api.Deps{
		Cfg: cfg, Log: log, Auth: authMgr, Searcher: searcher, Detail: resolver,
		Registry: registry, Cache: store, Client: doer, Proxy: proxyHandler, LiveProxy: liveHandler,
		UserData: userStore, Media: mediaMgr, Sources: sourceStore,
		Subscribe: fakeSubscribe{}, Prober: prober, Version: cfg.Version,
	})
	h := server.New(server.Deps{API: apiHandler, Cfg: cfg, Log: log, Auth: authMgr})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, authMgr
}

type fakeSubscribe struct{}

func (fakeSubscribe) Fetch(_ context.Context, rawURL string) (*model.SourceListPayload, error) {
	return &model.SourceListPayload{
		Name: "测试订阅",
		Sources: []model.SourceConfig{
			{Key: "sub_aaa", Name: "订阅源A", URL: "https://sub-a.test/api.php/provide/vod"},
			{Key: "sub_bbb", Name: "订阅源B", URL: "https://sub-b.test/api.php/provide/vod"},
		},
	}, nil
}

func login(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	res, err := http.Post(ts.URL+"/api/auth", "application/json", strings.NewReader(`{"password":"pw"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", res.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Token == "" {
		t.Fatal("empty token")
	}
	return out.Token
}

func TestHealthAndStatus(t *testing.T) {
	ts, _ := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("health = %d", res.StatusCode)
	}
	res, err = http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var st struct {
		PasswordRequired bool                 `json:"passwordRequired"`
		Verified         bool                 `json:"verified"`
		DefaultSources   []model.SourceConfig `json:"defaultSources"`
		Capabilities     []string             `json:"capabilities"`
	}
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if !st.PasswordRequired || st.Verified || len(st.DefaultSources) != 1 || len(st.Capabilities) == 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestAuthGuard(t *testing.T) {
	ts, _ := newTestServer(t)
	res, err := http.Post(ts.URL+"/api/search", "application/json", strings.NewReader(`{"wd":"庆余年"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized search = %d", res.StatusCode)
	}
}

func TestSearchJSONAndStream(t *testing.T) {
	ts, _ := newTestServer(t)
	token := login(t, ts)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/search", strings.NewReader(`{"wd":"庆余年"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var sr model.SearchResponse
	if err := json.NewDecoder(res.Body).Decode(&sr); err != nil {
		t.Fatal(err)
	}
	if len(sr.List) != 1 || sr.List[0].VodID != "1" {
		t.Fatalf("search = %+v", sr)
	}

	// 流式：新关键词避免命中缓存。
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/search?stream=1", strings.NewReader(`{"wd":"狂飙"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	var types []string
	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("bad ndjson line %q: %v", sc.Text(), err)
		}
		types = append(types, ev["type"].(string))
	}
	joined := strings.Join(types, ",")
	if !strings.Contains(joined, "source") || !strings.HasSuffix(joined, "done") {
		t.Fatalf("stream events = %v", types)
	}
}

func TestDetailAndProxy(t *testing.T) {
	ts, _ := newTestServer(t)
	token := login(t, ts)
	src := model.SourceConfig{Key: "a", Name: "源A", URL: "https://a.test/api.php/provide/vod"}
	q := url.Values{}
	q.Set("id", "1")
	b, _ := json.Marshal(src)
	q.Set("source", string(b))
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/detail?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var d model.VideoDetail
	if err := json.NewDecoder(res.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if len(d.Episodes) != 2 {
		t.Fatalf("episodes = %v", d.Episodes)
	}
	if len(d.PlayLines) != 2 {
		t.Fatalf("playLines = %+v", d.PlayLines)
	}
	if d.PlayLines[0].Name != "lineA" || d.PlayLines[0].Episodes[0].Name != "第1集" {
		t.Fatalf("line0 = %+v", d.PlayLines[0])
	}
	if d.PlayLines[1].Name != "lineB" || d.PlayLines[1].Episodes[0].Name != "正片" {
		t.Fatalf("line1 = %+v", d.PlayLines[1])
	}
	if d.PlayLines[0].Episodes[0].ID != "e:第1集" || d.PlayLines[1].Episodes[0].ID != "e:正片" {
		t.Fatalf("episode ids = %q / %q", d.PlayLines[0].Episodes[0].ID, d.PlayLines[1].Episodes[0].ID)
	}

	// 匿名图片代理放行；非图片未登录 401。
	res, err = http.Get(ts.URL + "/api/proxy?url=" + url.QueryEscape("https://img.test/a.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("anon image = %d", res.StatusCode)
	}
	res, err = http.Get(ts.URL + "/api/proxy?url=" + url.QueryEscape("https://a.test/api.php/x"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon non-image = %d", res.StatusCode)
	}
}

func TestCORS(t *testing.T) {
	ts, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/api/me/progress", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	req.Header.Set("Access-Control-Request-Headers", "content-type,authorization")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight = %d", res.StatusCode)
	}
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("allow-origin = %q", got)
	}
	if got := res.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "PUT") {
		t.Fatalf("allow-methods = %q", got)
	}
	if got := res.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(strings.ToLower(got), "authorization") {
		t.Fatalf("allow-headers = %q", got)
	}

	// 非白名单 Origin 不应获得放行头。
	req, _ = http.NewRequest(http.MethodOptions, ts.URL+"/api/me/progress", nil)
	req.Header.Set("Origin", "https://evil.example")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("unexpected allow-origin for evil origin: %q", got)
	}
}
