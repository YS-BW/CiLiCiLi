package recommend

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

var ratingPattern = regexp.MustCompile(`^\d+\.\d$`)

func TestBangumiCalendarParsesFixture(t *testing.T) {
	doer := doerContent(readFixture(t, "bangumi_calendar.json"))
	store := newFakeCache()
	fetcher := newTestFetcher(doer, store)

	days, err := fetcher.BangumiCalendar(context.Background())
	if err != nil {
		t.Fatalf("BangumiCalendar 返回错误: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("天数 = %d，期望 2", len(days))
	}
	if days[0].Weekday != 1 || days[1].Weekday != 2 {
		t.Fatalf("星期未按升序排列: %+v", days)
	}
	if len(days[0].Items) != 2 {
		t.Fatalf("周一条目数 = %d，期望 2", len(days[0].Items))
	}
	item := days[0].Items[0]
	if item.Title == "" || item.Cover == "" || item.ID == "" {
		t.Fatalf("条目字段缺失: %+v", item)
	}
	if !strings.HasPrefix(item.Cover, "http://lain.bgm.tv/") {
		t.Fatalf("封面应优先大图: %q", item.Cover)
	}
	if item.Rating != "" && !ratingPattern.MatchString(item.Rating) {
		t.Fatalf("评分应保留一位小数: %q", item.Rating)
	}
	if !item.IsTV {
		t.Fatal("Bangumi 条目应标记为剧集")
	}

	// 真实接口：https://api.bgm.tv/calendar（免 key，需可识别 UA）。
	parsed, _ := parseCallURL(t, doer.lastCall(t).URL)
	if parsed.Scheme != "https" || parsed.Host != "api.bgm.tv" || parsed.Path != "/calendar" {
		t.Fatalf("请求地址异常: %s", parsed)
	}
	if got := doer.lastCall(t).Opts.Headers.Get("User-Agent"); got == "" {
		t.Fatal("缺少 User-Agent 头")
	}

	ttl, stale := store.windowOf(t, bangumiCacheKey)
	if ttl != 30*time.Minute {
		t.Fatalf("TTL = %v，期望 30 分钟", ttl)
	}
	if stale != 5*30*time.Minute {
		t.Fatalf("staleTTL = %v，期望 5 倍 TTL", stale)
	}

	// 第二次调用命中缓存。
	if _, err := fetcher.BangumiCalendar(context.Background()); err != nil {
		t.Fatalf("缓存命中请求失败: %v", err)
	}
	if got := doer.callCount(); got != 1 {
		t.Fatalf("出网次数 = %d，期望 1", got)
	}
}

func TestBangumiCalendarFiltersInvalidWeekdayAndItems(t *testing.T) {
	body := []byte(`[
	  {"weekday":{"id":8},"items":[{"id":1,"name":"非法星期","images":{"large":"https://img.test/1.jpg"}}]},
	  {"weekday":{"id":0},"items":[{"id":2,"name":"非法星期","images":{"large":"https://img.test/2.jpg"}}]},
	  {"weekday":{"id":3},"items":[
	    {"id":0,"name":"无 id","images":{"large":"https://img.test/3.jpg"}},
	    {"id":4,"name":"无封面"},
	    {"id":5,"name":"","name_cn":"","images":{"large":"https://img.test/5.jpg"}},
	    {"id":6,"name":"日文名","images":{"large":"https://img.test/6.jpg"},"rating":{"score":8}},
	    {"id":7,"name":"日文名","name_cn":"中文名","images":{"common":"https://img.test/7c.jpg"}}
	  ]},
	  {"weekday":{"id":1},"items":[{"id":8,"name":"周一","images":{"medium":"https://img.test/8m.jpg"}}]},
	  {"weekday":{"id":3},"items":[{"id":9,"name":"重复分组","images":{"large":"https://img.test/9.jpg"}}]}
	]`)
	fetcher := newTestFetcher(doerContent(body), nil)

	days, err := fetcher.BangumiCalendar(context.Background())
	if err != nil {
		t.Fatalf("BangumiCalendar 返回错误: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("天数 = %d，期望 2（非法星期被丢弃）", len(days))
	}
	if days[0].Weekday != 1 || days[1].Weekday != 3 {
		t.Fatalf("星期排序异常: %+v", days)
	}
	if len(days[0].Items) != 1 || days[0].Items[0].Title != "周一" {
		t.Fatalf("周一分组异常: %+v", days[0].Items)
	}
	if len(days[0].Items[0].Rating) != 0 {
		t.Fatalf("无评分应省略: %q", days[0].Items[0].Rating)
	}

	// 同一天拆成两个分组应合并，且脏数据被丢弃（只剩 id 6 / 7 / 9）。
	weekday3 := days[1].Items
	if len(weekday3) != 3 {
		t.Fatalf("周三条目数 = %d，期望 3: %+v", len(weekday3), weekday3)
	}
	if weekday3[0].Title != "日文名" || weekday3[0].Rating != "8.0" {
		t.Fatalf("评分格式化异常: %+v", weekday3[0])
	}
	if weekday3[1].Title != "中文名" {
		t.Fatalf("标题应优先 name_cn: %+v", weekday3[1])
	}
	if weekday3[1].Cover != "https://img.test/7c.jpg" {
		t.Fatalf("封面应回退 common: %q", weekday3[1].Cover)
	}
	if weekday3[2].Cover != "https://img.test/9.jpg" {
		t.Fatalf("合并分组封面异常: %q", weekday3[2].Cover)
	}
}

func TestBangumiCalendarEmptyIsAnError(t *testing.T) {
	fetcher := newTestFetcher(doerContent([]byte(`[]`)), nil)

	_, err := fetcher.BangumiCalendar(context.Background())
	if err == nil || !strings.Contains(err.Error(), "数据为空") {
		t.Fatalf("错误 = %v，期望提示数据为空", err)
	}
}

func TestBangumiCalendarNonArrayIsAnError(t *testing.T) {
	fetcher := newTestFetcher(doerContent([]byte(`{"err":"blocked"}`)), nil)

	_, err := fetcher.BangumiCalendar(context.Background())
	if err == nil || !strings.Contains(err.Error(), "数据异常") {
		t.Fatalf("错误 = %v，期望提示数据异常", err)
	}
}

func TestBangumiCalendarUpstreamErrorIsSanitized(t *testing.T) {
	raw := &url.Error{Op: "Get", URL: "https://api.bgm.tv/calendar", Err: errors.New("tls: handshake failure")}
	fetcher := newTestFetcher(doerError(raw), nil)

	_, err := fetcher.BangumiCalendar(context.Background())
	if err == nil {
		t.Fatal("上游错误应向上返回")
	}
	text := err.Error()
	if strings.Contains(text, "api.bgm.tv") || strings.Contains(text, "https") {
		t.Fatalf("错误文案泄露 URL: %q", text)
	}
	if !strings.Contains(text, "Bangumi 放送表失败") {
		t.Fatalf("错误文案 = %q", text)
	}
}
