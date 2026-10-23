package subscribe

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"cilicili/internal/model"
)

var sourceKeyPattern = regexp.MustCompile(`^sub_[0-9a-f]{8}$`)

// requireInvalidContent 断言错误属于「内容问题」。
func requireInvalidContent(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("期望报错，实际为 nil")
	}
	if !errors.Is(err, ErrInvalidContent) {
		t.Fatalf("错误 = %v，期望包含 ErrInvalidContent", err)
	}
}

func TestParseLibreTVFullFixture(t *testing.T) {
	payload, err := Parse(readFixture(t, "libretv_full.json"))
	if err != nil {
		t.Fatalf("Parse 返回错误: %v", err)
	}
	if payload.Name != "我的全站源" {
		t.Fatalf("订阅名 = %q", payload.Name)
	}
	if payload.Stats == nil || payload.Stats.Format != formatLibreTV {
		t.Fatalf("stats.format 异常: %+v", payload.Stats)
	}
	// 老格式保持既有行为：libretv 的非法/重复条目静默丢弃，不计入跳过统计。
	if payload.Stats.Skipped != 0 || payload.Stats.Truncated != 0 || len(payload.Stats.SkippedByReason) != 0 {
		t.Fatalf("libretv 统计应为空: %+v", payload.Stats)
	}

	if len(payload.Sources) != 2 {
		t.Fatalf("点播源数 = %d，期望 2", len(payload.Sources))
	}
	first := payload.Sources[0]
	if first.Name != "源A" || first.URL != "https://vod.example.com/api.php/provide/vod" {
		t.Fatalf("首个点播源异常: %+v", first)
	}
	if first.Detail != "https://vod.example.com" || !first.IsAdult {
		t.Fatalf("detail/isAdult 异常: %+v", first)
	}
	if !sourceKeyPattern.MatchString(first.Key) {
		t.Fatalf("key = %q，期望 sub_<8hex>", first.Key)
	}
	// 点播地址去掉尾部斜杠，保证同一接口只有一条。
	if payload.Sources[1].URL != "https://vod2.example.com/api.php/provide/vod" {
		t.Fatalf("尾斜杠未归一: %q", payload.Sources[1].URL)
	}
	if payload.Sources[1].IsAdult {
		t.Fatalf("isAdult=false 不应导入: %+v", payload.Sources[1])
	}

	if len(payload.LiveSources) != 1 {
		t.Fatalf("直播源数 = %d，期望 1", len(payload.LiveSources))
	}
	live := payload.LiveSources[0]
	if live.Name != "直播A" || live.URL != "https://live.example.com/list.m3u" {
		t.Fatalf("直播源异常: %+v", live)
	}
	if live.EPG != "https://live.example.com/epg.xml.gz" {
		t.Fatalf("EPG 异常: %q", live.EPG)
	}
	if !sourceKeyPattern.MatchString(live.Key) {
		t.Fatalf("直播 key = %q，期望 sub_<8hex>", live.Key)
	}
}

func TestParseLibreTVLegacyAndBareArray(t *testing.T) {
	payload, err := Parse(readFixture(t, "libretv_legacy.json"))
	if err != nil {
		t.Fatalf("老格式解析失败: %v", err)
	}
	if payload.Name != "老订阅" || len(payload.Sources) != 1 || len(payload.LiveSources) != 0 {
		t.Fatalf("老格式结果异常: %+v", payload)
	}

	bare, err := Parse(readFixture(t, "bare_array.json"))
	if err != nil {
		t.Fatalf("裸数组解析失败: %v", err)
	}
	if bare.Name != "" || len(bare.Sources) != 2 {
		t.Fatalf("裸数组结果异常: %+v", bare)
	}
	// 名称缺省时用主机名兜底。
	if bare.Sources[1].Name != "b.example.com" {
		t.Fatalf("主机名兜底失败: %q", bare.Sources[1].Name)
	}
	if !bare.Sources[1].IsAdult {
		t.Fatalf("isAdult=true 未导入: %+v", bare.Sources[1])
	}
	if bare.LiveSources == nil {
		t.Fatal("liveSources 不应为 nil（序列化为 [] 而非 null）")
	}
}

func TestParseLiveOnly(t *testing.T) {
	payload, err := Parse([]byte(`{"name":"纯直播","liveSources":[{"url":"https://live.example.com/list.m3u"}]}`))
	if err != nil {
		t.Fatalf("纯直播订阅解析失败: %v", err)
	}
	if len(payload.Sources) != 0 || len(payload.LiveSources) != 1 {
		t.Fatalf("结果异常: %+v", payload)
	}
	if payload.LiveSources[0].Name != "live.example.com" {
		t.Fatalf("主机名兜底失败: %q", payload.LiveSources[0].Name)
	}
}

func TestParseEmptyOrNull(t *testing.T) {
	cases := map[string]string{
		"null":         `null`,
		"空对象":          `{}`,
		"空 sources":    `{"name":"空的","sources":[]}`,
		"空数组":          `[]`,
		"字符串":          `"text"`,
		"数字":           `42`,
		"sources 不是数组": `{"sources":"x"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(body))
			requireInvalidContent(t, err)
		})
	}
}

func TestParseInvalidURLs(t *testing.T) {
	payload, err := Parse([]byte(`{
	  "sources": [
	    {"url": "ftp://vod.example.com/vod"},
	    {"url": ""},
	    {"name": "无 url"},
	    {"url": "https://ok.example.com/vod"}
	  ],
	  "liveSources": [
	    {"url": "rtmp://live.example.com/live"},
	    {"url": "https://live.example.com/list.m3u", "epg": "javascript:alert(1)"}
	  ]
	}`))
	if err != nil {
		t.Fatalf("Parse 返回错误: %v", err)
	}
	if len(payload.Sources) != 1 || payload.Sources[0].URL != "https://ok.example.com/vod" {
		t.Fatalf("非法点播地址未被过滤: %+v", payload.Sources)
	}
	if len(payload.LiveSources) != 1 {
		t.Fatalf("非法直播地址未被过滤: %+v", payload.LiveSources)
	}
	if payload.LiveSources[0].EPG != "" {
		t.Fatalf("非法 EPG 应被丢弃: %q", payload.LiveSources[0].EPG)
	}
}

func TestParseDedupesThenTruncates(t *testing.T) {
	// 60 条重复 + 100 条唯一：前段重复只占 1 个名额，最终恰好 100 条。
	var builder strings.Builder
	builder.WriteString(`{"sources":[`)
	for i := 0; i < 60; i++ {
		builder.WriteString(`{"url":"https://dup.example.com/vod"},`)
	}
	for i := 0; i < MaxVodSources; i++ {
		fmt.Fprintf(&builder, `{"url":"https://vod%d.example.com/vod"},`, i)
	}
	builder.WriteString(`],"liveSources":[`)
	for i := 0; i < MaxLiveSources+10; i++ {
		fmt.Fprintf(&builder, `{"url":"https://live%d.example.com/list.m3u"},`, i)
	}
	builder.WriteString(`]}`)

	payload, err := Parse([]byte(builder.String()))
	if err != nil {
		t.Fatalf("Parse 返回错误: %v", err)
	}
	if len(payload.Sources) != MaxVodSources {
		t.Fatalf("点播源数 = %d，期望 %d", len(payload.Sources), MaxVodSources)
	}
	if payload.Sources[0].URL != "https://dup.example.com/vod" {
		t.Fatalf("重复条目应只保留一次: %q", payload.Sources[0].URL)
	}
	if len(payload.LiveSources) != MaxLiveSources {
		t.Fatalf("直播源数 = %d，期望 %d", len(payload.LiveSources), MaxLiveSources)
	}
}

func TestParseKeysAreStable(t *testing.T) {
	a, err := Parse([]byte(`{"sources":[{"url":"https://a.example.com/vod"},{"url":"https://b.example.com/vod"}]}`))
	if err != nil {
		t.Fatalf("Parse 返回错误: %v", err)
	}
	// 顺序相反、地址带尾斜杠/空查询串时，key 仍按归一化 URL 生成。
	b, err := Parse([]byte(`{"sources":[{"url":"https://b.example.com/vod/"},{"url":"https://a.example.com/vod?"}]}`))
	if err != nil {
		t.Fatalf("Parse 返回错误: %v", err)
	}
	keysA := map[string]string{}
	for _, source := range a.Sources {
		keysA[source.URL] = source.Key
	}
	for _, source := range b.Sources {
		if keysA[source.URL] != source.Key {
			t.Fatalf("同一地址的 key 不稳定: %s → %q vs %q", source.URL, keysA[source.URL], source.Key)
		}
	}
	if keysA["https://a.example.com/vod"] == keysA["https://b.example.com/vod"] {
		t.Fatal("不同地址不应得到相同 key")
	}
}

func TestParseCatalogFixture(t *testing.T) {
	payload, err := Parse(readFixture(t, "catalog.json"))
	if err != nil {
		t.Fatalf("Parse 返回错误: %v", err)
	}
	if payload.Name != "" {
		t.Fatalf("接口目录不应有订阅名: %q", payload.Name)
	}
	if payload.Stats == nil || payload.Stats.Format != formatCatalog {
		t.Fatalf("stats.format 异常: %+v", payload.Stats)
	}
	if len(payload.Sources) != 2 {
		t.Fatalf("点播源数 = %d，期望 2: %+v", len(payload.Sources), payload.Sources)
	}
	if payload.Sources[0].Name != "接口A" || payload.Sources[0].URL != "https://a.example.com/api.php/provide/vod" {
		t.Fatalf("首个接口异常: %+v", payload.Sources[0])
	}
	// 名称缺省用主机名兜底，地址去掉尾斜杠并复用同一 key 规则。
	second := payload.Sources[1]
	if second.Name != "b.example.com" || second.URL != "https://b.example.com/api.php/provide/vod" {
		t.Fatalf("第二个接口异常: %+v", second)
	}
	if !sourceKeyPattern.MatchString(second.Key) {
		t.Fatalf("key = %q", second.Key)
	}
	if payload.Stats.Skipped != 1 || payload.Stats.SkippedByReason[model.SkipDisabled] != 1 {
		t.Fatalf("停用条目统计异常: %+v", payload.Stats)
	}
	if len(payload.Stats.SkippedSamples) != 1 || payload.Stats.SkippedSamples[0] != "停用" {
		t.Fatalf("跳过样例异常: %+v", payload.Stats.SkippedSamples)
	}
	if payload.LiveSources == nil || len(payload.LiveSources) != 0 {
		t.Fatalf("接口目录不应有直播源: %+v", payload.LiveSources)
	}
}

func TestParseCatalogErrors(t *testing.T) {
	// 全部不可导入：报错并带原因明细。
	_, err := Parse([]byte(`[{"name":"停用","baseUrl":"https://a.example.com/vod","enabled":false}]`))
	requireInvalidContent(t, err)
	if !strings.Contains(err.Error(), "已跳过 1 个条目（已停用 1）") {
		t.Fatalf("错误文案 = %q", err)
	}

	_, err = Parse([]byte(`[{"baseUrl":""}]`))
	requireInvalidContent(t, err)
	if !strings.Contains(err.Error(), "已跳过 1 个条目（地址不可用 1）") {
		t.Fatalf("错误文案 = %q", err)
	}
}

func TestParseCatalogTruncates(t *testing.T) {
	var builder strings.Builder
	builder.WriteString(`[`)
	for i := 0; i < MaxVodSources+5; i++ {
		fmt.Fprintf(&builder, `{"baseUrl":"https://vod%d.example.com/api.php/provide/vod"},`, i)
	}
	builder.WriteString(`]`)

	payload, err := Parse([]byte(builder.String()))
	if err != nil {
		t.Fatalf("Parse 返回错误: %v", err)
	}
	if len(payload.Sources) != MaxVodSources {
		t.Fatalf("点播源数 = %d，期望 %d", len(payload.Sources), MaxVodSources)
	}
	if payload.Stats.Truncated != 5 {
		t.Fatalf("截断数 = %d，期望 5", payload.Stats.Truncated)
	}
}

func TestParseMixedCatalogFields(t *testing.T) {
	_, err := Parse([]byte(`[{"baseUrl":"https://a.example.com/api.php/provide/vod"},{"url":"https://b.example.com/api.php/provide/vod"}]`))
	requireInvalidContent(t, err)
	if !strings.Contains(err.Error(), "混用了 baseUrl 与 url") {
		t.Fatalf("错误文案 = %q", err)
	}
}

func TestParseInvalidJSON(t *testing.T) {
	_, err := Parse([]byte(`<html>403 Forbidden</html>`))
	requireInvalidContent(t, err)
	if !strings.Contains(err.Error(), "不是合法的 JSON") {
		t.Fatalf("错误文案 = %q", err)
	}
}

func TestParseLenientJSON(t *testing.T) {
	// 行注释、块注释、尾随逗号；字符串内的 // 与转义引号不受影响。
	body := `{
	  // 站点列表
	  "name": "含注释配置",
	  "sources": [
	    { "url": "https://a.example.com/vod", "detail": "https://x//y" }, // 行内注释
	    /* 第二段 */
	    { "url": "https://b.example.com/vod", "name": "say \"hi\"" },
	  ],
	}`
	payload, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("宽容解析失败: %v", err)
	}
	if payload.Name != "含注释配置" || len(payload.Sources) != 2 {
		t.Fatalf("结果异常: %+v", payload)
	}
	if payload.Sources[0].Detail != "https://x//y" {
		t.Fatalf("字符串内的 // 被误伤: %q", payload.Sources[0].Detail)
	}
	if payload.Sources[1].Name != `say "hi"` {
		t.Fatalf("转义引号被误伤: %q", payload.Sources[1].Name)
	}

	// 字符串内的裸换行/制表符（共享配置实际写法）应被转义保留。
	payload, err = Parse([]byte("{\"name\":\"优\n酷\",\"sources\":[{\"url\":\"https://a.example.com/vod\"}]}"))
	if err != nil {
		t.Fatalf("控制字符宽容解析失败: %v", err)
	}
	if payload.Name != "优\n酷" {
		t.Fatalf("控制字符转义异常: %q", payload.Name)
	}
}

func TestParseLenientFixture(t *testing.T) {
	payload, err := Parse(readFixture(t, "tvbox_lenient.json"))
	if err != nil {
		t.Fatalf("Parse 返回错误: %v", err)
	}
	if payload.Stats.Format != formatTVBOX {
		t.Fatalf("format = %q，期望 tvbox", payload.Stats.Format)
	}
	if len(payload.Sources) != 1 || payload.Sources[0].URL != "https://a.example.com/api.php/provide/vod" {
		t.Fatalf("结果异常: %+v", payload.Sources)
	}
}

func TestParseBOM(t *testing.T) {
	payload, err := Parse([]byte("\uFEFF{\"sources\":[{\"url\":\"https://a.example.com/vod\"}]}"))
	if err != nil {
		t.Fatalf("带 BOM 的订阅解析失败: %v", err)
	}
	if len(payload.Sources) != 1 {
		t.Fatalf("点播源数 = %d，期望 1", len(payload.Sources))
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		trimSlash bool
		want      string
	}{
		{"空串", "   ", true, ""},
		{"非 http 协议", "ftp://a.example.com/vod", true, ""},
		{"尾部空查询", "https://a.example.com/api.php/provide/vod?", true, "https://a.example.com/api.php/provide/vod"},
		{"尾部空锚点", "https://a.example.com/api.php/provide/vod#", true, "https://a.example.com/api.php/provide/vod"},
		{"尾部空查询加锚点", "https://a.example.com/api.php/provide/vod?#", true, "https://a.example.com/api.php/provide/vod"},
		{"尾部斜杠与空查询", "https://a.example.com/api.php/provide/vod/?", true, "https://a.example.com/api.php/provide/vod"},
		{"点播去尾斜杠", "https://a.example.com/vod///", true, "https://a.example.com/vod"},
		{"直播保留尾斜杠", "https://a.example.com/live.m3u/", false, "https://a.example.com/live.m3u/"},
		{"带参数不受影响", "https://a.example.com/vod?ac=list", true, "https://a.example.com/vod?ac=list"},
		{"带锚点不受影响", "https://a.example.com/live.m3u?token=abc#t=10", false, "https://a.example.com/live.m3u?token=abc#t=10"},
		{"缺少主机", "https://", true, ""},
		{"大协议", "HTTPS://A.example.com/Vod", true, "HTTPS://A.example.com/Vod"},
		{"javascript 伪协议", "javascript:alert(1)", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeURL(tc.raw, tc.trimSlash); got != tc.want {
				t.Fatalf("NormalizeURL(%q) = %q，期望 %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestNormalizeSubscriptionURL(t *testing.T) {
	cases := map[string]string{
		"  https://a.example.com/list.json/  ":      "https://a.example.com/list.json",
		"https://a.example.com/list.json///":        "https://a.example.com/list.json",
		"https://a.example.com/list.json":           "https://a.example.com/list.json",
		"https://a.example.com/list.json?":          "https://a.example.com/list.json",
		"https://a.example.com/list.json#":          "https://a.example.com/list.json",
		"https://a.example.com/list.json/?":         "https://a.example.com/list.json",
		"https://a.example.com/list.json?token=abc": "https://a.example.com/list.json?token=abc",
	}
	for raw, want := range cases {
		if got := NormalizeSubscriptionURL(raw); got != want {
			t.Fatalf("NormalizeSubscriptionURL(%q) = %q，期望 %q", raw, got, want)
		}
	}
}
