package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

func TestSafeRedirectFollowsAndRevalidatesEachHop(t *testing.T) {
	var mu sync.Mutex
	var hostSeen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hostSeen = append(hostSeen, r.Host)
		mu.Unlock()
		switch r.URL.Path {
		case "/a":
			http.Redirect(w, r, "/b", http.StatusFound)
		case "/b":
			http.Redirect(w, r, "/c", http.StatusTemporaryRedirect)
		case "/c":
			_, _ = io.WriteString(w, "done")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	lookup := newTestLookup(map[string][]netip.Addr{
		"hop.test": {netip.MustParseAddr("127.0.0.1")},
	})
	c := mustNewClient(t, Config{AllowPrivate: true})
	useLookup(c, lookup)

	url := "http://hop.test:" + serverPort(t, srv) + "/a"
	resp, err := c.Do(context.Background(), url, Options{SafeRedirects: true})
	if err != nil {
		t.Fatalf("Do 返回错误: %v", err)
	}
	if body := mustReadBody(t, resp); body != "done" {
		t.Fatalf("body = %q", body)
	}
	if resp.FinalURL != "http://hop.test:"+serverPort(t, srv)+"/c" {
		t.Fatalf("FinalURL = %q", resp.FinalURL)
	}
	// 初始 + 2 次重定向，每跳都重新解析一次（不允许复用旧结果）。
	if got := lookup.callCount(); got != 3 {
		t.Fatalf("DNS 解析次数 = %d, 期望 3（每跳复检）", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hostSeen) != 3 {
		t.Fatalf("请求次数 = %d, 期望 3", len(hostSeen))
	}
	for _, host := range hostSeen {
		if !strings.HasPrefix(host, "hop.test:") {
			t.Fatalf("Host = %q, 期望保持原域名", host)
		}
	}
}

func TestSafeRedirectLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atoiOrZero(strings.TrimPrefix(r.URL.Path, "/r/"))
		if n < 20 {
			http.Redirect(w, r, "/r/"+itoa(n+1), http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "end")
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})

	_, err := c.GetText(context.Background(), srv.URL+"/r/0", Options{SafeRedirects: true})
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("err = %v, 期望 ErrTooManyRedirects", err)
	}
}

func TestSafeRedirectBlockedTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("to") {
		case "metadata":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		case "alibaba":
			http.Redirect(w, r, "http://100.100.100.200/latest/meta-data/", http.StatusFound)
		case "ftp":
			http.Redirect(w, r, "ftp://example.com/x", http.StatusFound)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})

	for _, target := range []string{"metadata", "alibaba", "ftp"} {
		t.Run(target, func(t *testing.T) {
			_, err := c.GetText(context.Background(), srv.URL+"/go?to="+target, Options{SafeRedirects: true})
			if !errors.Is(err, ErrBlocked) {
				t.Fatalf("err = %v, 期望 ErrBlocked", err)
			}
		})
	}
}

func TestSafeRedirectWithoutLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})

	resp, err := c.Do(context.Background(), srv.URL, Options{SafeRedirects: true})
	if err != nil {
		t.Fatalf("Do 返回错误: %v", err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("StatusCode = %d, 期望 302", resp.StatusCode)
	}
	resp.Close()
}

func TestSafeRedirect303ChangesMethodToGet(t *testing.T) {
	var mu sync.Mutex
	var firstMethod, finalMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/post":
			mu.Lock()
			firstMethod = r.Method
			mu.Unlock()
			w.Header().Set("Location", "/final")
			w.WriteHeader(http.StatusSeeOther)
		case "/final":
			mu.Lock()
			finalMethod = r.Method
			mu.Unlock()
			_, _ = io.WriteString(w, "ok")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{AllowPrivate: true})
	resp, err := c.Do(context.Background(), srv.URL+"/post", Options{
		Method:        http.MethodPost,
		SafeRedirects: true,
	})
	if err != nil {
		t.Fatalf("Do 返回错误: %v", err)
	}
	if body := mustReadBody(t, resp); body != "ok" {
		t.Fatalf("body = %q", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if firstMethod != http.MethodPost {
		t.Fatalf("首跳 method = %q, 期望 POST", firstMethod)
	}
	if finalMethod != http.MethodGet {
		t.Fatalf("303 后 method = %q, 期望 GET", finalMethod)
	}
}

func TestSafeRedirect307PreservesMethod(t *testing.T) {
	var mu sync.Mutex
	var finalMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/post" {
			w.Header().Set("Location", "/final")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		mu.Lock()
		finalMethod = r.Method
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{AllowPrivate: true})
	_, err := c.GetText(context.Background(), srv.URL+"/post", Options{Method: http.MethodPost, SafeRedirects: true})
	if err != nil {
		t.Fatalf("GetText 返回错误: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if finalMethod != http.MethodPost {
		t.Fatalf("307 后 method = %q, 期望保留 POST", finalMethod)
	}
}

func TestSafeRedirectDropsSensitiveHeadersCrossHost(t *testing.T) {
	type captured struct{ auth, cookie, custom string }
	ch := make(chan captured, 1)
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- captured{
			auth:   r.Header.Get("Authorization"),
			cookie: r.Header.Get("Cookie"),
			custom: r.Header.Get("X-Custom"),
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srvB.Close()

	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://b.test:"+serverPort(t, srvB)+"/target", http.StatusFound)
	}))
	defer srvA.Close()

	lookup := newTestLookup(map[string][]netip.Addr{
		"a.test": {netip.MustParseAddr("127.0.0.1")},
		"b.test": {netip.MustParseAddr("127.0.0.1")},
	})
	c := mustNewClient(t, Config{AllowPrivate: true})
	useLookup(c, lookup)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer secret")
	headers.Set("Cookie", "session=secret")
	headers.Set("X-Custom", "keep-me")
	url := "http://a.test:" + serverPort(t, srvA) + "/start"
	if _, err := c.GetText(context.Background(), url, Options{SafeRedirects: true, Headers: headers}); err != nil {
		t.Fatalf("GetText 返回错误: %v", err)
	}
	got := waitChan(t, ch)
	if got.auth != "" || got.cookie != "" {
		t.Fatalf("跨主机重定向泄露敏感头: %+v", got)
	}
	if got.custom != "keep-me" {
		t.Fatalf("普通头应保留, X-Custom = %q", got.custom)
	}
}

func TestSafeRedirectKeepsSensitiveHeadersSameHost(t *testing.T) {
	type captured struct{ auth string }
	ch := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		ch <- captured{auth: r.Header.Get("Authorization")}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer secret")
	if _, err := c.GetText(context.Background(), srv.URL+"/start", Options{SafeRedirects: true, Headers: headers}); err != nil {
		t.Fatalf("GetText 返回错误: %v", err)
	}
	if got := waitChan(t, ch); got.auth != "Bearer secret" {
		t.Fatalf("同主机重定向应保留 Authorization, 实际 %q", got.auth)
	}
}

func TestDefaultRedirectIsStandardFollow(t *testing.T) {
	// 8 跳链路：标准库跟随（上限 10）成功；安全模式（上限 5）报错。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atoiOrZero(strings.TrimPrefix(r.URL.Path, "/n/"))
		if n < 8 {
			http.Redirect(w, r, "/n/"+itoa(n+1), http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "end")
	}))
	defer srv.Close()
	c := mustNewClient(t, Config{AllowPrivate: true})
	url := srv.URL + "/n/0"

	// 零值 Options：SafeRedirects=false → 标准库跟随。
	resp, err := c.Do(context.Background(), url, Options{})
	if err != nil {
		t.Fatalf("默认跟随重定向失败: %v", err)
	}
	if body := mustReadBody(t, resp); body != "end" {
		t.Fatalf("body = %q", body)
	}
	if want := srv.URL + "/n/8"; resp.FinalURL != want {
		t.Fatalf("FinalURL = %q, 期望 %q", resp.FinalURL, want)
	}

	if _, err := c.GetText(context.Background(), url, Options{SafeRedirects: true}); !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("安全模式 err = %v, 期望 ErrTooManyRedirects", err)
	}
}

func TestUnsafeRedirectLimit(t *testing.T) {
	// 12 跳链路超过标准跟随上限（10），返回可判断的 ErrTooManyRedirects。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atoiOrZero(strings.TrimPrefix(r.URL.Path, "/n/"))
		if n < 12 {
			http.Redirect(w, r, "/n/"+itoa(n+1), http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "end")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{AllowPrivate: true})
	_, err := c.GetText(context.Background(), srv.URL+"/n/0", Options{})
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("err = %v, 期望 ErrTooManyRedirects", err)
	}
}
