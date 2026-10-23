package recommend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cilicili/internal/upstream"
)

func TestHotListIDs(t *testing.T) {
	ids := HotListIDs()
	want := []string{
		"douban_movie_weekly", "douban_tv_chinese", "douban_tv_global",
		"douban_show_chinese", "douban_show_global", "baidu_teleplay",
	}
	if len(ids) != len(want) {
		t.Fatalf("榜单数量 = %d，期望 %d", len(ids), len(want))
	}
	for i, id := range want {
		if ids[i] != id {
			t.Fatalf("榜单顺序 [%d] = %q，期望 %q", i, ids[i], id)
		}
		if !IsHotListID(id) {
			t.Fatalf("IsHotListID(%q) = false", id)
		}
	}
	for _, id := range []string{"", "douban", "douban_movie", "baidu", "UNKNOWN"} {
		if IsHotListID(id) {
			t.Fatalf("IsHotListID(%q) 应为 false", id)
		}
	}
}

func TestHotListDoubanWeeklyPaths(t *testing.T) {
	cases := map[string]struct {
		path string
		isTV bool
	}{
		"douban_movie_weekly": {path: "/v2/douban/weekly/movie", isTV: false},
		"douban_tv_chinese":   {path: "/v2/douban/weekly/tv_chinese", isTV: true},
		"douban_tv_global":    {path: "/v2/douban/weekly/tv_global", isTV: true},
		"douban_show_chinese": {path: "/v2/douban/weekly/show_chinese", isTV: true},
		"douban_show_global":  {path: "/v2/douban/weekly/show_global", isTV: true},
	}
	body := readFixture(t, "sixty_douban_weekly.json")
	for id, want := range cases {
		t.Run(id, func(t *testing.T) {
			doer := doerContent(body)
			fetcher := newTestFetcher(doer, nil)

			items, err := fetcher.HotList(context.Background(), id)
			if err != nil {
				t.Fatalf("HotList 返回错误: %v", err)
			}
			if len(items) != 2 {
				t.Fatalf("条目数 = %d，期望 2", len(items))
			}
			if items[0].ID != "37322586" || items[0].Title != "生日快乐" {
				t.Fatalf("首条字段异常: %+v", items[0])
			}
			if items[0].Rating != "8.9" {
				t.Fatalf("评分 = %q，期望 8.9", items[0].Rating)
			}
			// 第二条 fixture 只有 cover_proxy，应作为兜底封面。
			if items[1].Cover != "https://doubanio.viki.moe/view/photo/m_ratio_poster/public/p2935498318.jpg" {
				t.Fatalf("cover_proxy 兜底失败: %q", items[1].Cover)
			}
			if items[0].IsTV != want.isTV {
				t.Fatalf("IsTV = %v，期望 %v", items[0].IsTV, want.isTV)
			}

			call := doer.lastCall(t)
			if !strings.HasSuffix(call.URL, want.path) {
				t.Fatalf("请求地址 = %q，期望以 %q 结尾", call.URL, want.path)
			}
			if !strings.HasPrefix(call.URL, defaultSixtyAPIBase) {
				t.Fatalf("默认应请求主实例，实际 %q", call.URL)
			}
		})
	}
}

func TestHotListBaiduTeleplay(t *testing.T) {
	doer := doerContent(readFixture(t, "sixty_baidu_teleplay.json"))
	fetcher := newTestFetcher(doer, nil)

	items, err := fetcher.HotList(context.Background(), "baidu_teleplay")
	if err != nil {
		t.Fatalf("HotList 返回错误: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("条目数 = %d，期望 2", len(items))
	}
	if items[0].ID != "baidu_0" || items[1].ID != "baidu_1" {
		t.Fatalf("百度榜单 id 应带序号: %+v", []string{items[0].ID, items[1].ID})
	}
	if items[0].Title != "九门" || items[0].Cover == "" {
		t.Fatalf("首条字段异常: %+v", items[0])
	}
	if items[0].Rating != "" {
		t.Fatalf("百度榜单无评分，实际 %q", items[0].Rating)
	}
	if !items[0].IsTV {
		t.Fatal("百度热播剧应标记为剧集")
	}
	if !strings.HasSuffix(doer.lastCall(t).URL, sixtyBaiduTeleplayPath) {
		t.Fatalf("请求地址异常: %q", doer.lastCall(t).URL)
	}
}

func TestHotListUnknownID(t *testing.T) {
	doer := doerContent(nil)
	fetcher := newTestFetcher(doer, nil)

	_, err := fetcher.HotList(context.Background(), "douban_top250")
	if !errors.Is(err, ErrUnknownHotList) {
		t.Fatalf("错误 = %v，期望 ErrUnknownHotList", err)
	}
	if !strings.Contains(err.Error(), "未知榜单") {
		t.Fatalf("错误文案 = %q", err)
	}
	if doer.callCount() != 0 {
		t.Fatalf("未知榜单不应发起请求，实际 %d 次", doer.callCount())
	}

	// 超长输入会被截断，避免原样回显。
	long := strings.Repeat("x", 200)
	_, err = fetcher.HotList(context.Background(), long)
	if !errors.Is(err, ErrUnknownHotList) {
		t.Fatalf("错误 = %v，期望 ErrUnknownHotList", err)
	}
	if len([]rune(err.Error())) > maxHotListIDRunes+len("未知榜单: ")+1 {
		t.Fatalf("错误文案过长: %q", err)
	}
}

func TestHotListCacheWindow(t *testing.T) {
	doer := doerContent(readFixture(t, "sixty_douban_weekly.json"))
	store := newFakeCache()
	fetcher := newTestFetcher(doer, store)
	ctx := context.Background()

	if _, err := fetcher.HotList(ctx, "douban_tv_chinese"); err != nil {
		t.Fatalf("首次请求失败: %v", err)
	}
	ttl, stale := store.windowOf(t, hotListCacheKeyPrefix+"douban_tv_chinese")
	if ttl != time.Hour {
		t.Fatalf("TTL = %v，期望 1 小时", ttl)
	}
	if stale != 5*time.Hour {
		t.Fatalf("staleTTL = %v，期望 5 倍 TTL", stale)
	}

	if _, err := fetcher.HotList(ctx, "douban_tv_chinese"); err != nil {
		t.Fatalf("缓存命中请求失败: %v", err)
	}
	if got := doer.callCount(); got != 1 {
		t.Fatalf("出网次数 = %d，期望 1", got)
	}
}

func TestHotListFallsBackToOfficialInstance(t *testing.T) {
	body := readFixture(t, "sixty_douban_weekly.json")
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		if strings.Contains(rawURL, "crystelf") {
			return &upstream.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader("")),
				FinalURL:   rawURL,
			}, nil
		}
		return &upstream.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(string(body))),
			FinalURL:   rawURL,
		}, nil
	}}
	fetcher := newTestFetcher(doer, nil)

	items, err := fetcher.HotList(context.Background(), "douban_movie_weekly")
	if err != nil {
		t.Fatalf("主实例失败应回退官方实例，实际报错: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("条目数 = %d，期望 2", len(items))
	}
	calls := doer.allCalls()
	if len(calls) != 2 {
		t.Fatalf("出网次数 = %d，期望 2（主实例 + 官方实例）", len(calls))
	}
	if !strings.HasPrefix(calls[1].URL, officialSixtyAPIBase) {
		t.Fatalf("回退地址 = %q，期望官方实例", calls[1].URL)
	}
}

func TestHotListAllInstancesFailIsSanitized(t *testing.T) {
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return nil, &urlErr{raw: "http://192.168.1.10:9999/v2/douban/weekly/movie"}
	}}
	fetcher := newTestFetcher(doer, nil)

	_, err := fetcher.HotList(context.Background(), "douban_movie_weekly")
	if err == nil {
		t.Fatal("所有实例失败应报错")
	}
	text := err.Error()
	for _, leaked := range []string{"192.168.1.10", "http", "9999", "v2/douban"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("错误文案泄露 %q: %q", leaked, text)
		}
	}
	if !strings.Contains(text, "影视榜单失败") {
		t.Fatalf("错误文案 = %q", text)
	}
}

// urlErr 模拟一个把完整 URL 挂在错误里的上游失败（如 net/url.Error）。
type urlErr struct{ raw string }

func (e *urlErr) Error() string { return "Get " + e.raw + ": connection refused" }

func TestHotListEmptyData(t *testing.T) {
	cases := map[string]string{
		"空数组":  `{"code":200,"data":[]}`,
		"null": `{"code":200,"data":null}`,
		"缺字段":  `{"code":500,"message":"boom"}`,
		"非数组":  `{"code":200,"data":{"id":1}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			fetcher := newTestFetcher(doerContent([]byte(body)), nil)
			_, err := fetcher.HotList(context.Background(), "douban_movie_weekly")
			if err == nil {
				t.Fatal("应报错")
			}
		})
	}
}

func TestHotListAcceptsNumericID(t *testing.T) {
	body := []byte(`{"code":200,"data":[
	  {"rank":1,"title":"数字 id","id":123456,"rating":7.2,"cover":"https://img.test/a.jpg"},
	  {"rank":2,"title":"字符串 id","id":"789","rating":0,"cover":"https://img.test/b.jpg"}
	]}`)
	fetcher := newTestFetcher(doerContent(body), nil)

	items, err := fetcher.HotList(context.Background(), "douban_movie_weekly")
	if err != nil {
		t.Fatalf("HotList 返回错误: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("条目数 = %d，期望 2", len(items))
	}
	if items[0].ID != "123456" || items[0].Rating != "7.2" {
		t.Fatalf("数字 id/评分归一化异常: %+v", items[0])
	}
	if items[1].ID != "789" || items[1].Rating != "" {
		t.Fatalf("score=0 应省略评分: %+v", items[1])
	}
}

func TestHotListServesStaleOnFailure(t *testing.T) {
	doer := doerContent(readFixture(t, "sixty_douban_weekly.json"))
	store := newFakeCache()
	fetcher := newTestFetcher(doer, store)
	ctx := context.Background()

	if _, err := fetcher.HotList(ctx, "douban_tv_global"); err != nil {
		t.Fatalf("首次请求失败: %v", err)
	}
	store.expireAll()
	doer.setFn(doerError(errors.New("all instances down")).fn)

	items, err := fetcher.HotList(ctx, "douban_tv_global")
	if err != nil {
		t.Fatalf("上游故障时应回退陈旧缓存，实际报错: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("陈旧缓存条目数 = %d，期望 2", len(items))
	}
}
