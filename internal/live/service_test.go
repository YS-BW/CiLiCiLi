package live

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"cilicili/internal/model"
	"cilicili/internal/upstream"
)

const playlistBody = `#EXTM3U
#PLAYLIST:测试订阅
#EXTINF:-1 tvg-id="cctv1" group-title="央视",CCTV-1
https://live.example.com/cctv1.m3u8
#EXTINF:-1 group-title="卫视",湖南卫视
https://live.example.com/hunan.m3u8
`

// xmltvAround 构造一份包含「正在播 / 下一档」的 XMLTV 文本。
func xmltvAround(now time.Time, channel, currentTitle, nextTitle string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<tv>
  <programme start="` + xmltvTimestamp(now.Add(-30*time.Minute)) + `" stop="` + xmltvTimestamp(now.Add(30*time.Minute)) +
		`" channel="` + channel + `"><title>` + currentTitle + `</title></programme>
  <programme start="` + xmltvTimestamp(now.Add(50*time.Minute)) + `" stop="` + xmltvTimestamp(now.Add(110*time.Minute)) +
		`" channel="` + channel + `"><title>` + nextTitle + `</title><desc>简介</desc></programme>
</tv>`
}

func TestPlaylistCachesAndForce(t *testing.T) {
	t.Parallel()

	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return testResponse(200, "audio/x-mpegurl", playlistBody, rawURL), nil
	}}
	svc, _ := newTestService(t, doer)

	res, err := svc.Playlist(context.Background(), "https://sub.example.com/list.m3u", false)
	if err != nil {
		t.Fatalf("Playlist 失败: %v", err)
	}
	if res.Name != "测试订阅" || len(res.Channels) != 2 {
		t.Fatalf("Playlist = %+v", res)
	}
	if len(res.Groups) != 2 {
		t.Fatalf("Groups = %v", res.Groups)
	}

	// 第二次应命中 10 分钟缓存。
	if _, err = svc.Playlist(context.Background(), "https://sub.example.com/list.m3u", false); err != nil {
		t.Fatalf("Playlist 缓存命中失败: %v", err)
	}
	if got := doer.count(); got != 1 {
		t.Fatalf("上游调用次数 = %d，期望 1（第二次命中缓存）", got)
	}

	// force 跳过缓存。
	if _, err = svc.Playlist(context.Background(), "https://sub.example.com/list.m3u", true); err != nil {
		t.Fatalf("Playlist(force) 失败: %v", err)
	}
	if got := doer.count(); got != 2 {
		t.Fatalf("上游调用次数 = %d，期望 2（force 强制刷新）", got)
	}
}

func TestPlaylistUsesSubscriptionURLAsBase(t *testing.T) {
	t.Parallel()

	body := "#EXTM3U\n#EXTINF:-1,频道\nstream/ch.m3u8\n"
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return testResponse(200, "audio/x-mpegurl", body, rawURL), nil
	}}
	svc, _ := newTestService(t, doer)

	res, err := svc.Playlist(context.Background(), "https://sub.example.com/live/list.m3u", false)
	if err != nil {
		t.Fatalf("Playlist 失败: %v", err)
	}
	want := "https://sub.example.com/live/stream/ch.m3u8"
	if res.Channels[0].URL != want {
		t.Errorf("URL = %q，期望 %q", res.Channels[0].URL, want)
	}
}

func TestPlaylistInvalidURL(t *testing.T) {
	t.Parallel()

	doer := &fakeDoer{fn: func(context.Context, string, upstream.Options) (*upstream.Response, error) {
		t.Fatal("非法地址不应发起请求")
		return nil, nil
	}}
	svc, _ := newTestService(t, doer)

	for _, raw := range []string{"", "   ", "ftp://example.com/a.m3u", "not a url", "file:///etc/passwd"} {
		if _, err := svc.Playlist(context.Background(), raw, false); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("Playlist(%q) err = %v，期望 ErrInvalidURL", raw, err)
		}
	}
}

func TestPlaylistNoClient(t *testing.T) {
	t.Parallel()

	svc := New(Options{Logger: testLogger()})
	if _, err := svc.Playlist(context.Background(), "https://sub.example.com/list.m3u", false); !errors.Is(err, ErrNoClient) {
		t.Fatalf("err = %v，期望 ErrNoClient", err)
	}
	if _, err := svc.EPG(context.Background(), "https://epg.example.com/epg.xml", "cctv1", false); !errors.Is(err, ErrNoClient) {
		t.Fatalf("err = %v，期望 ErrNoClient", err)
	}
}

func TestPlaylistUpstreamErrors(t *testing.T) {
	t.Parallel()

	t.Run("非2xx", func(t *testing.T) {
		t.Parallel()
		doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
			return testResponse(502, "", "bad gateway", rawURL), nil
		}}
		svc, _ := newTestService(t, doer)
		if _, err := svc.Playlist(context.Background(), "https://sub.example.com/list.m3u", false); err == nil ||
			!strings.Contains(err.Error(), "502") {
			t.Fatalf("err = %v，期望包含 502", err)
		}
	})

	t.Run("传输错误", func(t *testing.T) {
		t.Parallel()
		doer := &fakeDoer{fn: func(context.Context, string, upstream.Options) (*upstream.Response, error) {
			return nil, errors.New("connection refused")
		}}
		svc, _ := newTestService(t, doer)
		if _, err := svc.Playlist(context.Background(), "https://sub.example.com/list.m3u", false); err == nil {
			t.Fatal("期望返回错误")
		}
	})

	t.Run("HTML错误页", func(t *testing.T) {
		t.Parallel()
		doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
			return testResponse(200, "text/html", "<html>登录后可见</html>", rawURL), nil
		}}
		svc, _ := newTestService(t, doer)
		if _, err := svc.Playlist(context.Background(), "https://sub.example.com/list.m3u", false); !errors.Is(err, ErrInvalidPlaylist) {
			t.Fatalf("err = %v，期望 ErrInvalidPlaylist", err)
		}
	})
}

func TestPlaylistStaleFallback(t *testing.T) {
	t.Parallel()

	doer := &fakeDoer{fn: func(context.Context, string, upstream.Options) (*upstream.Response, error) {
		return nil, errors.New("upstream down")
	}}
	svc, store := newTestService(t, doer)

	url := "https://sub.example.com/list.m3u"
	stale := &model.LivePlaylistResponse{
		Name:     "旧订阅",
		Channels: []model.LiveChannel{{ID: "old", Name: "旧频道", URL: "https://live.example.com/old.m3u8"}},
		Groups:   []string{},
	}
	raw, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	// 新鲜期已过、stale 期内：回源失败时应返回这份旧结果。
	store.Set(cacheKey("live:playlist:", url), raw, time.Nanosecond, time.Hour)

	res, err := svc.Playlist(context.Background(), url, false)
	if err != nil {
		t.Fatalf("Playlist 应回退到 stale 缓存，实际错误: %v", err)
	}
	if res.Name != "旧订阅" || res.Channels[0].Name != "旧频道" {
		t.Fatalf("stale 结果不符合预期: %+v", res)
	}
}

func TestPlaylistSingleflight(t *testing.T) {
	t.Parallel()

	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		time.Sleep(30 * time.Millisecond)
		return testResponse(200, "audio/x-mpegurl", playlistBody, rawURL), nil
	}}
	svc, _ := newTestService(t, doer)

	const callers = 8
	gate := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, callers)
	results := make([]*model.LivePlaylistResponse, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-gate
			results[idx], errs[idx] = svc.Playlist(context.Background(), "https://sub.example.com/list.m3u", false)
		}(i)
	}
	close(gate)
	wg.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("调用 %d 失败: %v", i, errs[i])
		}
		if results[i] == nil || len(results[i].Channels) != 2 {
			t.Fatalf("调用 %d 结果异常: %+v", i, results[i])
		}
	}
	if got := doer.count(); got != 1 {
		t.Fatalf("上游调用次数 = %d，期望 1（singleflight 合并并发请求）", got)
	}
}

func TestEPGCachesAndForce(t *testing.T) {
	t.Parallel()

	now := time.Now()
	body := xmltvAround(now, "cctv1", "正在播", "下一档")
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return testResponse(200, "application/xml", body, rawURL), nil
	}}
	svc, _ := newTestService(t, doer)
	epgURL := "https://epg.example.com/epg.xml"

	res, err := svc.EPG(context.Background(), epgURL, "cctv1", false)
	if err != nil {
		t.Fatalf("EPG 失败: %v", err)
	}
	if res.ChannelID != "cctv1" {
		t.Errorf("ChannelID = %q", res.ChannelID)
	}
	if res.Current == nil || res.Current.Title != "正在播" {
		t.Fatalf("Current = %+v，期望 正在播", res.Current)
	}
	if res.Next == nil || res.Next.Title != "下一档" {
		t.Fatalf("Next = %+v，期望 下一档", res.Next)
	}
	if len(res.Programs) != 2 {
		t.Errorf("Programs = %d，期望 2", len(res.Programs))
	}

	// 第二次命中 6 小时缓存。
	if _, err = svc.EPG(context.Background(), epgURL, "cctv1", false); err != nil {
		t.Fatalf("EPG 缓存命中失败: %v", err)
	}
	if got := doer.count(); got != 1 {
		t.Fatalf("上游调用次数 = %d，期望 1", got)
	}

	if _, err = svc.EPG(context.Background(), epgURL, "cctv1", true); err != nil {
		t.Fatalf("EPG(force) 失败: %v", err)
	}
	if got := doer.count(); got != 2 {
		t.Fatalf("上游调用次数 = %d，期望 2（force 强制刷新）", got)
	}

	// 未知频道返回空节目单（非 nil）。
	other, err := svc.EPG(context.Background(), epgURL, "unknown", false)
	if err != nil {
		t.Fatalf("EPG(unknown) 失败: %v", err)
	}
	if other.Current != nil || other.Next != nil || other.Programs == nil || len(other.Programs) != 0 {
		t.Fatalf("未知频道结果 = %+v", other)
	}
}

func TestEPGGzip(t *testing.T) {
	t.Parallel()

	now := time.Now()
	body := gzipBytes(t, xmltvAround(now, "cctv1", "压缩正在播", "压缩下一档"))
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return testResponse(200, "application/gzip", string(body), rawURL), nil
	}}
	svc, _ := newTestService(t, doer)

	res, err := svc.EPG(context.Background(), "https://epg.example.com/epg.xml.gz", "cctv1", false)
	if err != nil {
		t.Fatalf("EPG(gzip) 失败: %v", err)
	}
	if res.Current == nil || res.Current.Title != "压缩正在播" {
		t.Fatalf("Current = %+v", res.Current)
	}
}

func TestEPGErrors(t *testing.T) {
	t.Parallel()

	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return testResponse(200, "text/html", "<html>error</html>", rawURL), nil
	}}
	svc, _ := newTestService(t, doer)

	if _, err := svc.EPG(context.Background(), "https://epg.example.com/epg.xml", "cctv1", false); !errors.Is(err, ErrInvalidXMLTV) {
		t.Fatalf("err = %v，期望 ErrInvalidXMLTV", err)
	}
	if _, err := svc.EPG(context.Background(), "https://epg.example.com/epg.xml", "", false); !errors.Is(err, ErrMissingChannel) {
		t.Fatalf("err = %v，期望 ErrMissingChannel", err)
	}
	if _, err := svc.EPG(context.Background(), "ftp://epg.example.com/epg.xml", "cctv1", false); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("err = %v，期望 ErrInvalidURL", err)
	}
}

func TestRedactURL(t *testing.T) {
	t.Parallel()

	got := redactURL("https://user:pass@sub.example.com/live/list.m3u?token=secret#frag")
	if strings.Contains(got, "secret") || strings.Contains(got, "pass") || strings.Contains(got, "frag") {
		t.Fatalf("redactURL 泄露敏感信息: %q", got)
	}
	if !strings.HasPrefix(got, "https://sub.example.com/live/list.m3u") {
		t.Fatalf("redactURL = %q", got)
	}
	if got := redactURL("://bad"); got != "<非法 URL>" {
		t.Errorf("redactURL(非法) = %q", got)
	}
}

func TestPlaylistErrorDoesNotLeakToken(t *testing.T) {
	t.Parallel()

	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return nil, &url.Error{
			Op:  "Get",
			URL: rawURL,
			Err: errors.New("connection reset by peer"),
		}
	}}
	svc, _ := newTestService(t, doer)

	rawURL := "https://sub.example.com/list.m3u?token=super-secret"
	_, err := svc.Playlist(context.Background(), rawURL, false)
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), rawURL) {
		t.Fatalf("错误文案泄露订阅地址/token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Errorf("错误文案丢失底层原因: %q", err.Error())
	}
}

func TestEPGStaleFallback(t *testing.T) {
	t.Parallel()

	doer := &fakeDoer{fn: func(context.Context, string, upstream.Options) (*upstream.Response, error) {
		return nil, errors.New("epg down")
	}}
	svc, store := newTestService(t, doer)

	now := time.Now()
	epgURL := "https://epg.example.com/epg.xml"
	stale := map[string][]model.EpgProgram{
		"cctv1": {{
			ChannelID: "cctv1",
			Start:     now.Add(-time.Minute).UnixMilli(),
			Stop:      now.Add(time.Hour).UnixMilli(),
			Title:     "旧节目",
		}},
	}
	raw, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	store.Set(cacheKey("live:epg:", epgURL), raw, time.Nanosecond, time.Hour)

	res, err := svc.EPG(context.Background(), epgURL, "cctv1", false)
	if err != nil {
		t.Fatalf("EPG 应回退到 stale 缓存，实际错误: %v", err)
	}
	if res.Current == nil || res.Current.Title != "旧节目" {
		t.Fatalf("stale EPG 结果不符合预期: %+v", res)
	}
	if len(res.Programs) != 1 {
		t.Errorf("Programs = %d", len(res.Programs))
	}
}
