package subscribe

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"cilicili/internal/upstream"
)

func newTestFetcher(client upstream.Doer) *Fetcher {
	return New(Options{Client: client, Logger: slog.New(slog.DiscardHandler)})
}

func TestFetchParsesSubscription(t *testing.T) {
	doer := doerText(string(readFixture(t, "libretv_full.json")))
	fetcher := newTestFetcher(doer)

	payload, err := fetcher.Fetch(context.Background(), "  https://subs.example.com/list.json/  ")
	if err != nil {
		t.Fatalf("Fetch 返回错误: %v", err)
	}
	if payload.Name != "我的全站源" || len(payload.Sources) != 2 || len(payload.LiveSources) != 1 {
		t.Fatalf("解析结果异常: %+v", payload)
	}

	call := doer.lastCall(t)
	// 订阅地址先归一化（trim + 去尾斜杠）再请求。
	if call.URL != "https://subs.example.com/list.json" {
		t.Fatalf("请求地址 = %q，期望归一化后的地址", call.URL)
	}
	if got := call.Opts.Headers.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept 头 = %q", got)
	}
	if call.Opts.Timeout != 8*time.Second {
		t.Fatalf("超时 = %v，期望 8s", call.Opts.Timeout)
	}
	if !call.Opts.SafeRedirects {
		t.Fatal("必须显式开启 SafeRedirects")
	}
	if call.Opts.MaxBytes != maxSubscribeBytes {
		t.Fatalf("body 上限 = %d，期望 %d", call.Opts.MaxBytes, maxSubscribeBytes)
	}
}

func TestFetchTVBOXSubscription(t *testing.T) {
	doer := doerText(string(readFixture(t, "tvbox.json")))
	fetcher := newTestFetcher(doer)

	payload, err := fetcher.Fetch(context.Background(), "https://subs.example.com/tvbox.json")
	if err != nil {
		t.Fatalf("Fetch 返回错误: %v", err)
	}
	if payload.Stats == nil || payload.Stats.Format != formatTVBOX || payload.Stats.Skipped != 9 {
		t.Fatalf("TVBOX 统计异常: %+v", payload.Stats)
	}
}

func TestFetchInvalidURL(t *testing.T) {
	cases := []string{"", "   ", "ftp://subs.example.com/list.json", "javascript:alert(1)", "not-a-url", "https://"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			doer := doerText("{}")
			fetcher := newTestFetcher(doer)

			_, err := fetcher.Fetch(context.Background(), raw)
			if !errors.Is(err, ErrInvalidURL) {
				t.Fatalf("错误 = %v，期望 ErrInvalidURL", err)
			}
			if doer.callCount() != 0 {
				t.Fatalf("非法地址不应发起请求，实际 %d 次", doer.callCount())
			}
		})
	}
}

func TestFetchUpstreamErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"状态码", &upstream.StatusError{StatusCode: 429, URL: "https://subs.example.com/list.json"}, "HTTP 429"},
		{"SSRF 拒绝", upstream.ErrBlocked, "目标地址不可用"},
		{"超大响应", upstream.ErrTooLarge, "超出大小限制"},
		{"超时", context.DeadlineExceeded, "请求超时"},
		{"取消", context.Canceled, "请求已取消"},
		{"连接失败", errors.New("dial tcp: connection refused"), "无法访问订阅地址"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := newTestFetcher(doerError(tc.err))

			_, err := fetcher.Fetch(context.Background(), "https://subs.example.com/list.json")
			if !errors.Is(err, ErrFetchFailed) {
				t.Fatalf("错误 = %v，期望 ErrFetchFailed", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 = %q，期望包含 %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "subs.example.com") {
				t.Fatalf("错误文案泄露订阅地址: %q", err)
			}
		})
	}
}

func TestFetchNetworkErrorIsSanitized(t *testing.T) {
	raw := &url.Error{
		Op:  "Get",
		URL: "http://10.9.8.7:8080/list.json?token=abc",
		Err: errors.New("dial tcp 10.9.8.7:8080: connect: connection refused"),
	}
	fetcher := newTestFetcher(doerError(raw))

	_, err := fetcher.Fetch(context.Background(), "https://subs.example.com/list.json")
	if err == nil {
		t.Fatal("上游错误应向上返回")
	}
	text := err.Error()
	for _, leaked := range []string{"10.9.8.7", "http", "8080", "token"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("错误文案泄露 %q: %q", leaked, text)
		}
	}
}

func TestFetchInvalidContent(t *testing.T) {
	fetcher := newTestFetcher(doerText("<html>403 Forbidden</html>"))

	_, err := fetcher.Fetch(context.Background(), "https://subs.example.com/list.json")
	if !errors.Is(err, ErrInvalidContent) {
		t.Fatalf("错误 = %v，期望 ErrInvalidContent", err)
	}
}

func TestFetchEmptyPayloadIsAnError(t *testing.T) {
	// 内容合法但一条都导入不了：不能当成功返回，否则调用方会清空现有源。
	fetcher := newTestFetcher(doerText(`{"sources":[{"url":"ftp://bad.example.com/vod"}]}`))

	_, err := fetcher.Fetch(context.Background(), "https://subs.example.com/list.json")
	if !errors.Is(err, ErrInvalidContent) {
		t.Fatalf("错误 = %v，期望 ErrInvalidContent", err)
	}
	if !strings.Contains(err.Error(), "没有可导入的源") {
		t.Fatalf("错误文案 = %q", err)
	}
}

func TestFetchWithoutClient(t *testing.T) {
	fetcher := New(Options{})

	_, err := fetcher.Fetch(context.Background(), "https://subs.example.com/list.json")
	if err == nil || !strings.Contains(err.Error(), "未配置上游客户端") {
		t.Fatalf("错误 = %v，期望提示未配置上游客户端", err)
	}
}

func TestNewDefaultTimeout(t *testing.T) {
	fetcher := New(Options{Client: doerText("{}")})
	if fetcher.timeout != 8*time.Second {
		t.Fatalf("默认超时 = %v，期望 8s", fetcher.timeout)
	}
	if fetcher.log == nil {
		t.Fatal("Logger 不应为 nil")
	}
}
