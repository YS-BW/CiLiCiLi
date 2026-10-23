package recommend

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"cilicili/internal/upstream"
)

// parseCallURL 解析 fakeDoer 记录的请求地址。
func parseCallURL(t *testing.T, raw string) (*url.URL, url.Values) {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("请求地址无法解析 %q: %v", raw, err)
	}
	return parsed, parsed.Query()
}

func TestDoubanParsesFixture(t *testing.T) {
	doer := doerContent(readFixture(t, "douban_tv.json"))
	fetcher := newTestFetcher(doer, newFakeCache())

	items, err := fetcher.Douban(context.Background(), "tv", "热门", 0, 25)
	if err != nil {
		t.Fatalf("Douban 返回错误: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("条目数 = %d，期望 3", len(items))
	}
	first := items[0]
	if first.ID != "36675942" || first.Title != "小城良方" {
		t.Fatalf("首条字段异常: %+v", first)
	}
	if !strings.HasPrefix(first.Cover, "https://img2.doubanio.com/") {
		t.Fatalf("封面异常: %q", first.Cover)
	}
	if first.Rating != "" {
		t.Fatalf("空评分应省略，实际 %q", first.Rating)
	}
	if !first.IsTV {
		t.Fatal("type=tv 的请求应标记 IsTV")
	}
	if items[2].Rating != "6.8" {
		t.Fatalf("第三条评分 = %q，期望 6.8", items[2].Rating)
	}

	// 请求地址与参数必须与豆瓣真实接口一致（实测路径见 douban.go 注释）。
	parsed, query := parseCallURL(t, doer.lastCall(t).URL)
	if parsed.Host != "movie.douban.com" || parsed.Path != "/j/search_subjects" {
		t.Fatalf("请求地址异常: %s", parsed)
	}
	for key, want := range map[string]string{
		"type": "tv", "tag": "热门", "sort": "recommend",
		"page_limit": "25", "page_start": "0",
	} {
		if got := query.Get(key); got != want {
			t.Fatalf("查询参数 %s = %q，期望 %q", key, got, want)
		}
	}

	call := doer.lastCall(t)
	if got := call.Opts.Headers.Get("User-Agent"); got == "" {
		t.Fatal("缺少 User-Agent 头")
	}
	if got := call.Opts.Headers.Get("Referer"); got != doubanReferer {
		t.Fatalf("Referer = %q，期望 %q", got, doubanReferer)
	}
	if !call.Opts.SafeRedirects {
		t.Fatal("必须显式开启 SafeRedirects")
	}
	if call.Opts.Timeout != defaultTimeout {
		t.Fatalf("超时 = %v，期望 %v", call.Opts.Timeout, defaultTimeout)
	}
	if call.Opts.MaxBytes != maxRecommendBody {
		t.Fatalf("body 上限 = %d，期望 %d", call.Opts.MaxBytes, maxRecommendBody)
	}
}

func TestDoubanUsesCacheWithTenMinuteWindow(t *testing.T) {
	doer := doerContent(readFixture(t, "douban_tv.json"))
	store := newFakeCache()
	fetcher := newTestFetcher(doer, store)
	ctx := context.Background()

	if _, err := fetcher.Douban(ctx, "tv", "热门", 0, 25); err != nil {
		t.Fatalf("首次请求失败: %v", err)
	}
	if _, err := fetcher.Douban(ctx, "tv", "热门", 0, 25); err != nil {
		t.Fatalf("缓存命中请求失败: %v", err)
	}
	if got := doer.callCount(); got != 1 {
		t.Fatalf("出网次数 = %d，期望命中缓存后只请求 1 次", got)
	}

	key := doubanCacheKeyPrefix + "tv:热门:0:25"
	ttl, stale := store.windowOf(t, key)
	if ttl != 10*time.Minute {
		t.Fatalf("TTL = %v，期望 10 分钟", ttl)
	}
	if stale != 5*10*time.Minute {
		t.Fatalf("staleTTL = %v，期望 5 倍 TTL", stale)
	}
}

func TestDoubanNormalizesParams(t *testing.T) {
	doer := doerContent([]byte(`{"subjects":[]}`))
	store := newFakeCache()
	fetcher := newTestFetcher(doer, store)

	if _, err := fetcher.Douban(context.Background(), "TV", "   ", -5, 999); err != nil {
		t.Fatalf("Douban 返回错误: %v", err)
	}
	_, query := parseCallURL(t, doer.lastCall(t).URL)
	if got := query.Get("type"); got != "tv" {
		t.Fatalf("type = %q，期望 tv", got)
	}
	if got := query.Get("tag"); got != defaultDoubanTag {
		t.Fatalf("空 tag 应回退 %q，实际 %q", defaultDoubanTag, got)
	}
	if got := query.Get("page_start"); got != "0" {
		t.Fatalf("负 page_start 应归零，实际 %q", got)
	}
	if got := query.Get("page_limit"); got != "50" {
		t.Fatalf("超大 pageSize 应截到 50，实际 %q", got)
	}

	// 未知 type 按电影处理。
	doer2 := doerContent([]byte(`{"subjects":[]}`))
	fetcher2 := newTestFetcher(doer2, nil)
	if _, err := fetcher2.Douban(context.Background(), "anime", "", 0, 0); err != nil {
		t.Fatalf("Douban 返回错误: %v", err)
	}
	_, query2 := parseCallURL(t, doer2.lastCall(t).URL)
	if got := query2.Get("type"); got != "movie" {
		t.Fatalf("未知 type 应回退 movie，实际 %q", got)
	}
	if got := query2.Get("page_limit"); got != "25" {
		t.Fatalf("pageSize=0 应回退 25，实际 %q", got)
	}
}

func TestDoubanEmptySubjectsIsNotAnError(t *testing.T) {
	fetcher := newTestFetcher(doerContent([]byte(`{"subjects":[]}`)), nil)

	items, err := fetcher.Douban(context.Background(), "movie", "热门", 0, 25)
	if err != nil {
		t.Fatalf("空数组不应报错: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("条目数 = %d，期望 0", len(items))
	}
}

func TestDoubanMissingSubjectsIsAnError(t *testing.T) {
	fetcher := newTestFetcher(doerContent([]byte(`{"msg":"blocked"}`)), nil)

	if _, err := fetcher.Douban(context.Background(), "movie", "热门", 0, 25); err == nil {
		t.Fatal("缺少 subjects 字段应报错")
	}
}

func TestDoubanDropsDirtySubjects(t *testing.T) {
	body := []byte(`{"subjects":[
		{"id":"","title":"无 id","cover":"https://img.test/a.jpg","rate":"8.1"},
		{"id":"1","title":"","cover":"https://img.test/a.jpg","rate":"8.1"},
		{"id":"2","title":"无封面","cover":"","rate":"8.1"},
		{"id":"3","title":"零分","cover":"https://img.test/c.jpg","rate":"0"},
		{"id":"4","title":"正常","cover":"https://img.test/d.jpg","rate":"7.5"}
	]}`)
	fetcher := newTestFetcher(doerContent(body), nil)

	items, err := fetcher.Douban(context.Background(), "movie", "热门", 0, 25)
	if err != nil {
		t.Fatalf("Douban 返回错误: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("条目数 = %d，期望 2（脏数据被丢弃）", len(items))
	}
	if items[0].ID != "3" || items[0].Rating != "" {
		t.Fatalf("rate=0 应省略评分: %+v", items[0])
	}
	if items[1].ID != "4" || items[1].Rating != "7.5" || items[1].IsTV {
		t.Fatalf("正常条目字段异常: %+v", items[1])
	}
}

func TestDoubanBadJSONIsAnError(t *testing.T) {
	fetcher := newTestFetcher(doerContent([]byte(`<html>403 Forbidden</html>`)), nil)

	_, err := fetcher.Douban(context.Background(), "movie", "热门", 0, 25)
	if err == nil {
		t.Fatal("非 JSON 响应应报错")
	}
	if !strings.Contains(err.Error(), "豆瓣推荐响应异常") {
		t.Fatalf("错误文案 = %q，期望包含「豆瓣推荐响应异常」", err)
	}
}

func TestDoubanUpstreamErrorIsSanitized(t *testing.T) {
	raw := &url.Error{
		Op:  "Get",
		URL: "http://10.1.2.3:8080/secret?token=abc",
		Err: errors.New("dial tcp 10.1.2.3:8080: connect: connection refused"),
	}
	fetcher := newTestFetcher(doerError(raw), nil)

	_, err := fetcher.Douban(context.Background(), "movie", "热门", 0, 25)
	if err == nil {
		t.Fatal("上游错误应向上返回")
	}
	text := err.Error()
	for _, leaked := range []string{"10.1.2.3", "http", "secret", "token", "8080"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("错误文案泄露 %q: %q", leaked, text)
		}
	}
	if !strings.Contains(text, "豆瓣推荐失败") {
		t.Fatalf("错误文案 = %q，期望包含「豆瓣推荐失败」", text)
	}
}

func TestDoubanStatusErrorKeepsStatusCode(t *testing.T) {
	fetcher := newTestFetcher(doerStatus(403), nil)

	_, err := fetcher.Douban(context.Background(), "movie", "热门", 0, 25)
	if err == nil {
		t.Fatal("非 2xx 应报错")
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("错误文案 = %q，期望包含 HTTP 403", err)
	}
}

func TestDoubanServesStaleOnFailure(t *testing.T) {
	doer := doerContent(readFixture(t, "douban_tv.json"))
	store := newFakeCache()
	fetcher := newTestFetcher(doer, store)
	ctx := context.Background()

	if _, err := fetcher.Douban(ctx, "tv", "热门", 0, 25); err != nil {
		t.Fatalf("首次请求失败: %v", err)
	}
	store.expireAll()
	doer.setFn(doerError(errors.New("upstream exploded")).fn)

	items, err := fetcher.Douban(ctx, "tv", "热门", 0, 25)
	if err != nil {
		t.Fatalf("上游故障时应回退陈旧缓存，实际报错: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("陈旧缓存条目数 = %d，期望 3", len(items))
	}
}

func TestDoubanWithoutClient(t *testing.T) {
	fetcher := New(Options{})

	_, err := fetcher.Douban(context.Background(), "movie", "热门", 0, 25)
	if err == nil || !strings.Contains(err.Error(), "未配置上游客户端") {
		t.Fatalf("错误 = %v，期望提示未配置上游客户端", err)
	}
}

func TestDescribeError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"blocked", upstream.ErrBlocked, "目标地址不可用"},
		{"too-large", upstream.ErrTooLarge, "超出大小限制"},
		{"status", &upstream.StatusError{StatusCode: 502, URL: "https://x.test/a"}, "HTTP 502"},
		{"timeout", context.DeadlineExceeded, "请求超时"},
		{"canceled", context.Canceled, "请求已取消"},
		{"wrapped", errors.Join(errors.New("boom"), upstream.ErrBlocked), "目标地址不可用"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := describeError("豆瓣推荐", tc.err)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 = %q，期望包含 %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "https://") {
				t.Fatalf("错误文案泄露 URL: %q", err)
			}
		})
	}

	// 数据类错误原样返回（文案本身已足够友好）。
	if err := describeError("x", badData("榜单数据为空")); err.Error() != "榜单数据为空" {
		t.Fatalf("badData 文案被改写: %q", err)
	}
	if err := describeError("影视榜单", errors.New("boom")); err.Error() != "影视榜单失败" {
		t.Fatalf("通用错误 = %q", err)
	}
}

func TestRedactText(t *testing.T) {
	got := redactText(`Get "https://a.test/x?token=abc": dial tcp 10.0.0.9:443`)
	if strings.Contains(got, "a.test") || strings.Contains(got, "10.0.0.9") {
		t.Fatalf("日志脱敏失败: %q", got)
	}
	if !strings.Contains(got, "<url>") || !strings.Contains(got, "<ip>") {
		t.Fatalf("日志脱敏结果 = %q", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("豆瓣", 30); got != "豆瓣" {
		t.Fatalf("短串不应截断: %q", got)
	}
	got := truncateRunes(strings.Repeat("榜", 40), 30)
	if len([]rune(got)) != 31 || !strings.HasSuffix(got, "…") {
		t.Fatalf("截断结果 = %q", got)
	}
}

func TestNewDefaults(t *testing.T) {
	fetcher := New(Options{Client: doerContent(nil), SixtyAPIBase: "  https://sixty.test/  "})
	if fetcher.ua != defaultUserAgent {
		t.Fatalf("UA 默认值 = %q", fetcher.ua)
	}
	if fetcher.timeout != defaultTimeout {
		t.Fatalf("超时默认值 = %v", fetcher.timeout)
	}
	if fetcher.sixtyBase != "https://sixty.test" {
		t.Fatalf("SixtyAPIBase 归一化 = %q", fetcher.sixtyBase)
	}
	if fetcher.log == nil {
		t.Fatal("Logger 不应为 nil")
	}
}
