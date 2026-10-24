package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"cilicili/internal/auth"
	"cilicili/internal/upstream"
)

// ---- 测试替身 ----

type fakeCall struct {
	ctx  context.Context
	url  string
	opts upstream.Options
}

type fakeDoer struct {
	mu    sync.Mutex
	calls []fakeCall
	fn    func(context.Context, string, upstream.Options) (*upstream.Response, error)
}

func (f *fakeDoer) Do(ctx context.Context, rawURL string, opts upstream.Options) (*upstream.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{ctx: ctx, url: rawURL, opts: opts})
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(ctx, rawURL, opts)
	}
	return textResponse(rawURL, http.StatusOK, "ok", ""), nil
}

func (f *fakeDoer) GetJSON(context.Context, string, upstream.Options) ([]byte, error) {
	return nil, errors.New("fake: GetJSON 不应被调用")
}

func (f *fakeDoer) GetText(context.Context, string, upstream.Options) (string, error) {
	return "", errors.New("fake: GetText 不应被调用")
}

func (f *fakeDoer) snapshot() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

func (f *fakeDoer) count() int { return len(f.snapshot()) }

func (f *fakeDoer) onlyCall(t *testing.T) fakeCall {
	t.Helper()
	calls := f.snapshot()
	if len(calls) != 1 {
		t.Fatalf("上游调用次数 = %d, want 1", len(calls))
	}
	return calls[0]
}

// failReadCloser 用于断言 HEAD 不读取上游 body。
type failReadCloser struct{ t *testing.T }

func (f failReadCloser) Read([]byte) (int, error) {
	f.t.Errorf("HEAD 请求不应读取上游 body")
	return 0, io.EOF
}

func (f failReadCloser) Close() error { return nil }

func textResponse(finalURL string, status int, body, contentType string) *upstream.Response {
	hdr := make(http.Header)
	if contentType != "" {
		hdr.Set("Content-Type", contentType)
	}
	return &upstream.Response{
		StatusCode: status,
		Header:     hdr,
		Body:       io.NopCloser(strings.NewReader(body)),
		FinalURL:   finalURL,
	}
}

// ---- 请求/响应辅助 ----

func proxyRequest(method, target string) *http.Request {
	return httptest.NewRequest(method, proxyPathPrefix+"?url="+url.QueryEscape(target), nil)
}

func liveRequest(method, target string) *http.Request {
	return httptest.NewRequest(method, livePathPrefix+"?url="+url.QueryEscape(target), nil)
}

func withSession(t *testing.T, m *auth.Manager, r *http.Request) *http.Request {
	t.Helper()
	token, _ := m.Sign()
	r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
	return r
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON 错误：%v（body=%q）", err, rec.Body.String())
	}
	if payload.Error == "" {
		t.Fatalf("错误信息为空：body=%q", rec.Body.String())
	}
	return payload.Error
}

func assertHeader(t *testing.T, rec *httptest.ResponseRecorder, name, want string) {
	t.Helper()
	if got := rec.Header().Get(name); got != want {
		t.Errorf("响应头 %s = %q, want %q", name, got, want)
	}
}

func assertNoHeader(t *testing.T, rec *httptest.ResponseRecorder, name string) {
	t.Helper()
	if got := rec.Header().Get(name); got != "" {
		t.Errorf("响应头 %s 不应出现，got %q", name, got)
	}
}

func proxyOf(proxyURLBase, abs string) string {
	return proxyURLBase + url.QueryEscape(abs)
}

// ---- 400 / 403 / 502 ----

func TestBadTargetReturns400(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		target string
	}{
		{"空 url", ""},
		{"ftp 协议", "ftp://example.com/a.ts"},
		{"缺少 host", "http:///a.ts"},
		{"相对地址", "/a.ts"},
		{"非法 host", "https://exa mple.com/a.ts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeDoer{}
			rec := httptest.NewRecorder()
			New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodGet, tc.target))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400（body=%q）", rec.Code, rec.Body.String())
			}
			decodeError(t, rec)
			if n := client.count(); n != 0 {
				t.Fatalf("非法目标不应访问上游，实际调用 %d 次", n)
			}
		})
	}

	// live 版同样缺 url → 400。
	rec := httptest.NewRecorder()
	NewLive(Options{Client: &fakeDoer{}}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, livePathPrefix, nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("live 缺 url：status = %d, want 400", rec.Code)
	}
}

func TestBlockedTargetReturns403WithoutLeak(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{fn: func(context.Context, string, upstream.Options) (*upstream.Response, error) {
		return nil, fmt.Errorf("拒绝访问 10.0.0.5: %w", upstream.ErrBlocked)
	}}
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodGet, "http://10.0.0.5/secret"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	msg := decodeError(t, rec)
	for _, leak := range []string{"10.0.0.5", "拒绝访问"} {
		if strings.Contains(msg, leak) {
			t.Errorf("错误信息泄露内部细节 %q：%q", leak, msg)
		}
	}
}

func TestUpstreamFailureReturns502WithoutLeak(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{fn: func(context.Context, string, upstream.Options) (*upstream.Response, error) {
		return nil, errors.New("dial tcp 10.0.0.5:80: connect: connection refused")
	}}
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodGet, "https://cdn.example.com/seg.ts"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	msg := decodeError(t, rec)
	for _, leak := range []string{"10.0.0.5", "dial tcp", "refused"} {
		if strings.Contains(msg, leak) {
			t.Errorf("错误信息泄露内部细节 %q：%q", leak, msg)
		}
	}
}

func TestNilClientReturns502(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	New(Options{}).ServeHTTP(rec, proxyRequest(http.MethodGet, "https://cdn.example.com/seg.ts"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestInvalidUpstreamStatusIsNormalized(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return textResponse(rawURL, 9999, "x", "text/plain"), nil
	}}
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodGet, "https://cdn.example.com/seg.ts"))
	if rec.Code != http.StatusOK {
		t.Fatalf("非法上游状态码应归一化为 200，got %d", rec.Code)
	}
}

// ---- 鉴权 ----

func TestAnonymousImageWhitelist(t *testing.T) {
	t.Parallel()

	m := auth.New("secret", "", 0)
	allowed := []string{
		"https://img1.doubanio.com/pic.jpg",
		"https://doubanio.com/pic.jpg",
		"https://bgm.tv/pic.jpg",
		"https://lain.bgm.tv/pic.jpg",
		"https://doubanio.viki.moe/pic.jpg",
		"https://img1.doubanio.com:8443/not-an-image/path",
		"https://img1.doubanio.com./pic.jpg", // FQDN 末尾点不应绕过匹配
	}
	denied := []string{
		"https://evil-doubanio.com/pic.jpg", // 裸 HasSuffix 会误判的关键用例
		"https://doubanio.com.evil.com/pic.jpg",
		"https://evil-bgm.tv/pic.jpg",
		"https://douban.com/pic.jpg", // 不在白名单里
		"https://cdn.example.com/seg.ts",
	}

	client := &fakeDoer{}
	handler := New(Options{
		Client:              client,
		Auth:                m,
		AnonymousImageHosts: []string{"doubanio.com", "bgm.tv", "doubanio.viki.moe"},
	})

	for _, target := range allowed {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, proxyRequest(http.MethodGet, target))
		if rec.Code != http.StatusOK {
			t.Errorf("匿名图片域 %q 应放行，got %d（body=%q）", target, rec.Code, rec.Body.String())
		}
	}
	if got := client.count(); got != len(allowed) {
		t.Fatalf("放行时上游调用次数 = %d, want %d", got, len(allowed))
	}

	for _, target := range denied {
		before := client.count()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, proxyRequest(http.MethodGet, target))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("匿名非白名单域 %q 应拒绝，got %d", target, rec.Code)
			continue
		}
		if msg := decodeError(t, rec); msg != "未登录" {
			t.Errorf("错误信息 = %q, want 未登录", msg)
		}
		if after := client.count(); after != before {
			t.Errorf("拒绝时不应访问上游 %q：调用次数 %d → %d", target, before, after)
		}
	}
}

func TestAuthRequirement(t *testing.T) {
	t.Parallel()

	configured := auth.New("secret", "", 0)
	unconfigured := auth.New("", "", 0)
	const target = "https://cdn.example.com/seg.ts"

	t.Run("Auth 为 nil 时放行", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		New(Options{Client: &fakeDoer{}}).ServeHTTP(rec, proxyRequest(http.MethodGet, target))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("未配置 PASSWORD 时放行", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		New(Options{Client: &fakeDoer{}, Auth: unconfigured}).ServeHTTP(rec, proxyRequest(http.MethodGet, target))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("未登录返回 401", func(t *testing.T) {
		t.Parallel()
		client := &fakeDoer{}
		rec := httptest.NewRecorder()
		New(Options{Client: client, Auth: configured}).ServeHTTP(rec, proxyRequest(http.MethodGet, target))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if client.count() != 0 {
			t.Fatalf("未登录不应访问上游")
		}
	})

	t.Run("有效 cookie 放行", func(t *testing.T) {
		t.Parallel()
		req := withSession(t, configured, proxyRequest(http.MethodGet, target))
		rec := httptest.NewRecorder()
		New(Options{Client: &fakeDoer{}, Auth: configured}).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body=%q）", rec.Code, rec.Body.String())
		}
	})

	t.Run("有效 Bearer 放行", func(t *testing.T) {
		t.Parallel()
		token, _ := configured.Sign()
		req := proxyRequest(http.MethodGet, target)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		New(Options{Client: &fakeDoer{}, Auth: configured}).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("伪造 cookie 拒绝", func(t *testing.T) {
		t.Parallel()
		req := proxyRequest(http.MethodGet, target)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "v1.AAAA.deadbeef"})
		rec := httptest.NewRecorder()
		New(Options{Client: &fakeDoer{}, Auth: configured}).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}

func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{}
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodPost, "https://cdn.example.com/seg.ts"))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	assertHeader(t, rec, "Allow", "GET, HEAD")
	if client.count() != 0 {
		t.Fatalf("非 GET/HEAD 不应访问上游")
	}
}

// ---- 上游参数与请求头 ----

func TestUpstreamOptionsAndHeaderPassthrough(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{}
	handler := New(Options{
		Client:        client,
		UA:            "custom-ua",
		DoubanReferer: true,
		HeaderTimeout: 3 * time.Second,
	})
	req := proxyRequest(http.MethodGet, "https://img1.doubanio.com/pic.jpg")
	req.Header.Set("Range", "bytes=0-99")
	req.Header.Set("If-Range", `"etag-r"`)
	req.Header.Set("If-None-Match", `"etag-1"`)
	req.Header.Set("If-Modified-Since", "Wed, 21 Oct 2015 07:28:00 GMT")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	call := client.onlyCall(t)
	if call.url != "https://img1.doubanio.com/pic.jpg" {
		t.Errorf("上游 URL = %q", call.url)
	}
	if call.opts.Method != http.MethodGet {
		t.Errorf("上游 Method = %q, want GET", call.opts.Method)
	}
	if !call.opts.SafeRedirects {
		t.Error("SafeRedirects 应为 true")
	}
	if call.opts.Retries != 1 {
		t.Errorf("Retries = %d, want 1", call.opts.Retries)
	}
	if call.opts.Timeout != proxyTotalTimeout {
		t.Errorf("Timeout = %v, want %v", call.opts.Timeout, proxyTotalTimeout)
	}
	if call.opts.HeaderTimeout != 3*time.Second {
		t.Errorf("HeaderTimeout = %v, want 3s", call.opts.HeaderTimeout)
	}
	if call.opts.MaxBytes != 0 {
		t.Errorf("MaxBytes = %d, want 0（流式不设上限）", call.opts.MaxBytes)
	}
	if call.opts.AllowPrivate {
		t.Error("点播代理不应放行内网")
	}

	wantHeaders := map[string]string{
		"User-Agent":        "custom-ua",
		"Accept":            "*/*",
		"Range":             "bytes=0-99",
		"If-Range":          `"etag-r"`,
		"If-None-Match":     `"etag-1"`,
		"If-Modified-Since": "Wed, 21 Oct 2015 07:28:00 GMT",
		"Referer":           "https://movie.douban.com/",
	}
	for name, want := range wantHeaders {
		if got := call.opts.Headers.Get(name); got != want {
			t.Errorf("上游请求头 %s = %q, want %q", name, got, want)
		}
	}
}

func TestDoubanRefererRules(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		target    string
		refererOn bool
		want      string
	}{
		{"豆瓣主域", "https://movie.douban.com/x.jpg", true, doubanReferer},
		{"豆瓣图床子域", "https://img9.doubanio.com/x.jpg", true, doubanReferer},
		{"相似域不伪装", "https://evil-douban.com/x.jpg", true, ""},
		{"未开启开关", "https://img9.doubanio.com/x.jpg", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeDoer{}
			handler := New(Options{Client: client, DoubanReferer: tc.refererOn})
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, proxyRequest(http.MethodGet, tc.target))
			if got := client.onlyCall(t).opts.Headers.Get("Referer"); got != tc.want {
				t.Fatalf("Referer = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDefaultUAWhenEmpty(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{}
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodGet, "https://cdn.example.com/seg.ts"))
	if got := client.onlyCall(t).opts.Headers.Get("User-Agent"); got != defaultUA {
		t.Fatalf("默认 UA = %q, want %q", got, defaultUA)
	}
}

// ---- 流式响应头与 body ----

func TestStreamResponsePassthrough(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("0123456789abcdef"), 8192) // 128 KiB
	client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		hdr := http.Header{}
		hdr.Set("Content-Type", "image/jpeg")
		hdr.Set("Accept-Ranges", "bytes")
		hdr.Set("Content-Range", "bytes 0-131071/131072")
		hdr.Set("ETag", `"abc"`)
		hdr.Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		hdr.Set("Content-Length", "999") // 不应透传
		hdr.Set("Content-Encoding", "gzip")
		hdr.Set("Cache-Control", "public, max-age=1") // 由代理重设
		return &upstream.Response{StatusCode: http.StatusPartialContent, Header: hdr, Body: io.NopCloser(bytes.NewReader(payload)), FinalURL: rawURL}, nil
	}}

	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodGet, "https://cdn.example.com/pic.jpg"))

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("body 不一致：got %d bytes, want %d", rec.Body.Len(), len(payload))
	}
	assertHeader(t, rec, "Content-Type", "image/jpeg")
	assertHeader(t, rec, "Accept-Ranges", "bytes")
	assertHeader(t, rec, "Content-Range", "bytes 0-131071/131072")
	assertHeader(t, rec, "ETag", `"abc"`)
	assertHeader(t, rec, "Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
	assertHeader(t, rec, "Cache-Control", imageCacheControl)
	assertHeader(t, rec, "Access-Control-Allow-Origin", "*")
	assertHeader(t, rec, "X-Content-Type-Options", "nosniff")
	assertNoHeader(t, rec, "Content-Length")
	assertNoHeader(t, rec, "Content-Encoding")
	if !rec.Flushed {
		t.Error("流式响应应 flush")
	}
}

func TestStreamCacheControlByContentType(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return textResponse(rawURL, http.StatusOK, "data", "application/octet-stream"), nil
	}}
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodGet, "https://cdn.example.com/seg.ts"))
	assertHeader(t, rec, "Cache-Control", defaultCacheControl)
}

func TestNotModifiedPassesThroughWithoutBody(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		resp := textResponse(rawURL, http.StatusNotModified, "should-not-be-sent", "image/png")
		return resp, nil
	}}
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodGet, "https://cdn.example.com/pic.png"))
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 不应有 body，got %q", rec.Body.String())
	}
}

func TestUpstreamErrorStatusPassesThrough(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return textResponse(rawURL, http.StatusNotFound, "not found", "text/plain"), nil
	}}
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodGet, "https://cdn.example.com/missing.ts"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404（点播代理透传上游状态）", rec.Code)
	}
	if rec.Body.String() != "not found" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// ---- m3u8 ----

func TestM3U8RewriteThroughProxy(t *testing.T) {
	t.Parallel()

	const (
		target   = "https://cdn.example.com/live/index.m3u8"
		finalURL = "https://cdn.example.com/live/index.m3u8"
	)
	playlist := strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-STREAM-INF:BANDWIDTH=1280000",
		"720p/index.m3u8",
		`#EXT-X-KEY:METHOD=AES-128,URI="key.bin"`,
		`#EXT-X-MAP:URI="init.mp4"`,
		"seg1.ts",
		"#EXT-X-ENDLIST",
	}, "\n")

	client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		resp := textResponse(finalURL, http.StatusOK, playlist, "application/vnd.apple.mpegurl")
		resp.FinalURL = finalURL
		return resp, nil
	}}
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodGet, target))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	assertHeader(t, rec, "Content-Type", "application/vnd.apple.mpegurl")
	assertHeader(t, rec, "Cache-Control", "no-store")
	assertHeader(t, rec, "Access-Control-Allow-Origin", "*")

	body := rec.Body.String()
	wants := []string{
		proxyOf(proxyURLBase, "https://cdn.example.com/live/720p/index.m3u8"),
		`URI="` + proxyOf(proxyURLBase, "https://cdn.example.com/live/key.bin") + `"`,
		`URI="` + proxyOf(proxyURLBase, "https://cdn.example.com/live/init.mp4") + `"`,
		proxyOf(proxyURLBase, "https://cdn.example.com/live/seg1.ts"),
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("重写结果缺少 %q；body=%q", want, body)
		}
	}
	if !strings.Contains(body, "#EXTM3U") || !strings.Contains(body, "#EXT-X-ENDLIST") {
		t.Errorf("播放列表标签丢失：body=%q", body)
	}
}

func TestM3U8RewriteCarriesQueryToken(t *testing.T) {
	t.Parallel()

	const (
		target   = "https://cdn.example.com/live/index.m3u8"
		finalURL = "https://cdn.example.com/live/index.m3u8"
	)
	playlist := "#EXTM3U\nseg1.ts\n#EXT-X-ENDLIST"
	client := &fakeDoer{fn: func(_ context.Context, _ string, _ upstream.Options) (*upstream.Response, error) {
		resp := textResponse(finalURL, http.StatusOK, playlist, "application/vnd.apple.mpegurl")
		resp.FinalURL = finalURL
		return resp, nil
	}}
	r := httptest.NewRequest(http.MethodGet,
		proxyPathPrefix+"?token=abc123&url="+url.QueryEscape(target), nil)
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	want := "/api/proxy?token=abc123&url=" + url.QueryEscape("https://cdn.example.com/live/seg1.ts")
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("分片地址未携带 token：body=%q want=%q", rec.Body.String(), want)
	}
}

func TestM3U8DetectionFallsBackToURLAndFinalURL(t *testing.T) {
	t.Parallel()

	t.Run("content-type 不可信但 URL 是 m3u8", func(t *testing.T) {
		t.Parallel()
		client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
			return textResponse(rawURL, http.StatusOK, "seg.ts", "text/plain"), nil
		}}
		rec := httptest.NewRecorder()
		New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodGet, "https://cdn.example.com/live/index.m3u8"))
		assertHeader(t, rec, "Content-Type", "application/vnd.apple.mpegurl")
		if got, want := rec.Body.String(), proxyOf(proxyURLBase, "https://cdn.example.com/live/seg.ts"); got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
	})

	t.Run("重定向后最终 URL 作为相对路径基准", func(t *testing.T) {
		t.Parallel()
		const finalURL = "https://edge.example.com/redirected/real/index.m3u8"
		client := &fakeDoer{fn: func(_ context.Context, _ string, _ upstream.Options) (*upstream.Response, error) {
			resp := textResponse(finalURL, http.StatusOK, "seg.ts", "application/vnd.apple.mpegurl")
			resp.FinalURL = finalURL
			return resp, nil
		}}
		rec := httptest.NewRecorder()
		// 入口无 .m3u8 后缀，靠 content-type 识别。
		New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodGet, "https://cdn.example.com/play/1"))
		want := proxyOf(proxyURLBase, "https://edge.example.com/redirected/real/seg.ts")
		if got := rec.Body.String(); got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
	})
}

func TestM3U8TooLargeReturns502(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return textResponse(rawURL, http.StatusOK, strings.Repeat("a", maxPlaylistBytes+1), "application/vnd.apple.mpegurl"), nil
	}}
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodGet, "https://cdn.example.com/live/index.m3u8"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502（超限播放列表）", rec.Code)
	}
	decodeError(t, rec)
}

// ---- HEAD ----

func TestHeadRequest(t *testing.T) {
	t.Parallel()

	t.Run("普通资源不读取 body", func(t *testing.T) {
		t.Parallel()
		client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
			hdr := http.Header{}
			hdr.Set("Content-Type", "video/mp2t")
			hdr.Set("ETag", `"seg"`)
			return &upstream.Response{StatusCode: http.StatusOK, Header: hdr, Body: failReadCloser{t: t}, FinalURL: rawURL}, nil
		}}
		rec := httptest.NewRecorder()
		New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodHead, "https://cdn.example.com/seg.ts"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("HEAD 不应写 body，got %q", rec.Body.String())
		}
		assertHeader(t, rec, "ETag", `"seg"`)
		if got := client.onlyCall(t).opts.Method; got != http.MethodHead {
			t.Fatalf("上游 Method = %q, want HEAD", got)
		}
	})

	t.Run("playlist 只回头不改写不读 body", func(t *testing.T) {
		t.Parallel()
		client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
			hdr := http.Header{}
			hdr.Set("Content-Type", "application/vnd.apple.mpegurl")
			return &upstream.Response{StatusCode: http.StatusOK, Header: hdr, Body: failReadCloser{t: t}, FinalURL: rawURL}, nil
		}}
		rec := httptest.NewRecorder()
		New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodHead, "https://cdn.example.com/live/index.m3u8"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("HEAD 不应写 body")
		}
		assertHeader(t, rec, "Content-Type", "application/vnd.apple.mpegurl")
	})
}

// ---- 路径形式 ----

func TestPathFormTargets(t *testing.T) {
	t.Parallel()

	const target = "https://cdn.example.com/live/seg.ts"

	t.Run("点播路径形式", func(t *testing.T) {
		t.Parallel()
		client := &fakeDoer{}
		req := httptest.NewRequest(http.MethodGet, proxyPathPrefix+"/"+url.PathEscape(target), nil)
		rec := httptest.NewRecorder()
		New(Options{Client: client}).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body=%q）", rec.Code, rec.Body.String())
		}
		if got := client.onlyCall(t).url; got != target {
			t.Fatalf("上游 URL = %q, want %q", got, target)
		}
	})

	t.Run("网关已解码斜杠的路径形式", func(t *testing.T) {
		t.Parallel()
		client := &fakeDoer{}
		req := httptest.NewRequest(http.MethodGet, proxyPathPrefix+"/https://cdn.example.com/live/seg.ts", nil)
		rec := httptest.NewRecorder()
		New(Options{Client: client}).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body=%q）", rec.Code, rec.Body.String())
		}
		if got := client.onlyCall(t).url; got != target {
			t.Fatalf("上游 URL = %q, want %q", got, target)
		}
	})

	t.Run("直播路径形式", func(t *testing.T) {
		t.Parallel()
		client := &fakeDoer{}
		req := httptest.NewRequest(http.MethodGet, livePathPrefix+"/"+url.PathEscape(target), nil)
		rec := httptest.NewRecorder()
		NewLive(Options{Client: client}).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body=%q）", rec.Code, rec.Body.String())
		}
		if got := client.onlyCall(t).url; got != target {
			t.Fatalf("上游 URL = %q, want %q", got, target)
		}
	})
}

// ---- 直播 ----

func TestLiveStreamPassthrough(t *testing.T) {
	t.Parallel()

	m := auth.New("secret", "", 0)
	client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		hdr := http.Header{}
		hdr.Set("Content-Type", "video/x-flv")
		hdr.Set("Content-Length", "7") // 长连接不应透传长度
		return &upstream.Response{StatusCode: http.StatusOK, Header: hdr, Body: io.NopCloser(strings.NewReader("FLVDATA")), FinalURL: rawURL}, nil
	}}

	req := withSession(t, m, liveRequest(http.MethodGet, "https://live.example.com/channel.flv"))
	rec := httptest.NewRecorder()
	NewLive(Options{Client: client, Auth: m, LiveAllowPrivate: true}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "FLVDATA" {
		t.Fatalf("body = %q, want FLVDATA", got)
	}
	assertHeader(t, rec, "Content-Type", "video/x-flv")
	assertHeader(t, rec, "Cache-Control", liveCacheControl)
	assertHeader(t, rec, "X-Accel-Buffering", "no")
	assertHeader(t, rec, "Access-Control-Allow-Origin", "*")
	assertNoHeader(t, rec, "Content-Length")

	call := client.onlyCall(t)
	if call.opts.Timeout != 0 {
		t.Errorf("直播不应设 body 总超时，Timeout = %v", call.opts.Timeout)
	}
	if call.opts.Retries != 0 {
		t.Errorf("直播不应重试，Retries = %d", call.opts.Retries)
	}
	if !call.opts.AllowPrivate {
		t.Error("LiveAllowPrivate 应透传为 AllowPrivate")
	}
	if call.opts.HeaderTimeout != defaultHeaderTimeout {
		t.Errorf("HeaderTimeout = %v, want %v", call.opts.HeaderTimeout, defaultHeaderTimeout)
	}
	if !call.opts.SafeRedirects {
		t.Error("SafeRedirects 应为 true")
	}
}

func TestLiveContentTypeFallback(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return &upstream.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ts")), FinalURL: rawURL}, nil
	}}
	rec := httptest.NewRecorder()
	NewLive(Options{Client: client}).ServeHTTP(rec, liveRequest(http.MethodGet, "https://live.example.com/channel"))
	assertHeader(t, rec, "Content-Type", "video/mp2t")
}

func TestLiveM3U8RewriteUsesLivePrefix(t *testing.T) {
	t.Parallel()

	const finalURL = "https://live.example.com/live/index.m3u8"
	client := &fakeDoer{fn: func(_ context.Context, _ string, _ upstream.Options) (*upstream.Response, error) {
		resp := textResponse(finalURL, http.StatusOK, "seg1.ts", "application/vnd.apple.mpegurl")
		resp.FinalURL = finalURL
		return resp, nil
	}}
	rec := httptest.NewRecorder()
	NewLive(Options{Client: client}).ServeHTTP(rec, liveRequest(http.MethodGet, finalURL))
	assertHeader(t, rec, "Content-Type", "application/vnd.apple.mpegurl")
	assertHeader(t, rec, "Cache-Control", "no-store")
	want := proxyOf(liveURLBase, "https://live.example.com/live/seg1.ts")
	if got := rec.Body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestLiveRequiresAuth(t *testing.T) {
	t.Parallel()

	m := auth.New("secret", "", 0)
	client := &fakeDoer{}
	rec := httptest.NewRecorder()
	NewLive(Options{Client: client, Auth: m}).ServeHTTP(rec, liveRequest(http.MethodGet, "https://live.example.com/channel.flv"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if client.count() != 0 {
		t.Fatalf("未登录不应访问上游")
	}
}

func TestLiveUpstreamErrorStatusIsPassedThroughAsJSON(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return textResponse(rawURL, http.StatusForbidden, `{"detail":"token 过期，内网 10.0.0.5"}`, "application/json"), nil
	}}
	rec := httptest.NewRecorder()
	NewLive(Options{Client: client}).ServeHTTP(rec, liveRequest(http.MethodGet, "https://live.example.com/channel.flv"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	assertHeader(t, rec, "X-Live-Upstream-Status", "403")
	assertHeader(t, rec, "Cache-Control", "no-store")
	msg := decodeError(t, rec)
	if strings.Contains(msg, "10.0.0.5") || strings.Contains(msg, "token 过期") {
		t.Fatalf("不应回传上游错误细节：%q", msg)
	}
}

// ---- 取消与生命周期 ----

func TestUpstreamContextCanceledWhenHandlerReturns(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{}
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec, proxyRequest(http.MethodGet, "https://cdn.example.com/seg.ts"))

	call := client.onlyCall(t)
	if err := call.ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("handler 返回后上游 context 应已取消，got %v", err)
	}
}

func TestClientDisconnectDoesNotWriteJSON(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟客户端已断开

	client := &fakeDoer{fn: func(ctx context.Context, _ string, _ upstream.Options) (*upstream.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	req := proxyRequest(http.MethodGet, "https://cdn.example.com/seg.ts").WithContext(ctx)
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec, req)

	if rec.Body.Len() != 0 {
		t.Fatalf("客户端断开时不应写响应体，got %q", rec.Body.String())
	}
}

// 豆瓣图床对 Go TLS 指纹可能返回 200 + 反爬 HTML；代理应换镜像兜底。
func TestDoubanImageMirrorFallback(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		if strings.Contains(rawURL, doubanImageMirrorHost) {
			return textResponse(rawURL, http.StatusOK, "jpeg-bytes", "image/jpeg"), nil
		}
		return textResponse(rawURL, http.StatusOK, "<html>challenge</html>", "text/html"), nil
	}}
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec,
		proxyRequest(http.MethodGet, "https://img9.doubanio.com/view/photo/s_ratio_poster/public/p1.jpg"))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if rec.Body.String() != "jpeg-bytes" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	calls := client.snapshot()
	if len(calls) != 2 {
		t.Fatalf("上游调用次数 = %d, want 2", len(calls))
	}
	if !strings.Contains(calls[1].url, doubanImageMirrorHost) {
		t.Fatalf("第二次应请求镜像，got %s", calls[1].url)
	}
}

// 正常图片不应触发镜像兜底。
func TestDoubanImageNoFallbackWhenImage(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return textResponse(rawURL, http.StatusOK, "jpeg-bytes", "image/jpeg"), nil
	}}
	rec := httptest.NewRecorder()
	New(Options{Client: client}).ServeHTTP(rec,
		proxyRequest(http.MethodGet, "https://img9.doubanio.com/view/photo/s_ratio_poster/public/p1.jpg"))
	if rec.Code != http.StatusOK || rec.Body.String() != "jpeg-bytes" {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := client.count(); got != 1 {
		t.Fatalf("上游调用次数 = %d, want 1", got)
	}
}

// 图片失败后短时间内直接快速失败，不再重复等待上游超时。
func TestImageFailureNegativeCache(t *testing.T) {
	t.Parallel()

	client := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return nil, errors.New("dial tcp: i/o timeout")
	}}
	h := New(Options{Client: client})
	target := "https://dead.example.com/cover/deadbeef.jpg"
	first := httptest.NewRecorder()
	h.ServeHTTP(first, proxyRequest(http.MethodGet, target))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("first code = %d", first.Code)
	}
	if client.count() != 1 {
		t.Fatalf("upstream calls = %d", client.count())
	}
	second := httptest.NewRecorder()
	h.ServeHTTP(second, proxyRequest(http.MethodGet, target))
	if second.Code != http.StatusBadGateway {
		t.Fatalf("second code = %d", second.Code)
	}
	if client.count() != 1 {
		t.Fatalf("命中失败缓存后不应再访问上游，calls = %d", client.count())
	}
}
