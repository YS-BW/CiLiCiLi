package subscribe

import (
	"fmt"
	"strings"
	"testing"

	"cilicili/internal/model"
)

func TestParseTVBOXFixture(t *testing.T) {
	payload, err := Parse(readFixture(t, "tvbox.json"))
	if err != nil {
		t.Fatalf("Parse 返回错误: %v", err)
	}
	if payload.Stats == nil || payload.Stats.Format != formatTVBOX {
		t.Fatalf("stats.format 异常: %+v", payload.Stats)
	}
	if payload.Name != "某大佬的接口" {
		t.Fatalf("订阅名 = %q", payload.Name)
	}

	// 只导入 type=1 的 CMS 与「类型缺失但地址命中 CMS 特征」的站点。
	wantSources := map[string]string{
		"正版源":  "https://ok.example.com/api.php/provide/vod",
		"省略类型": "https://a.example.com/api.php/provide/vod",
	}
	if len(payload.Sources) != len(wantSources) {
		t.Fatalf("点播源数 = %d，期望 %d: %+v", len(payload.Sources), len(wantSources), payload.Sources)
	}
	for _, source := range payload.Sources {
		want, ok := wantSources[source.Name]
		if !ok {
			t.Fatalf("导入了不该导入的站点: %+v", source)
		}
		if source.URL != want {
			t.Fatalf("站点 %q URL = %q，期望 %q", source.Name, source.URL, want)
		}
		if !sourceKeyPattern.MatchString(source.Key) {
			t.Fatalf("key = %q，期望 sub_<8hex>", source.Key)
		}
	}

	// 跳过统计：spider 4（type=3 / .jar / .js / type=4 外链）、xml 1、unsearchable 1。
	if got := payload.Stats.SkippedByReason[model.SkipSpider]; got != 4 {
		t.Fatalf("spider 跳过数 = %d，期望 4", got)
	}
	if got := payload.Stats.SkippedByReason[model.SkipXML]; got != 1 {
		t.Fatalf("xml 跳过数 = %d，期望 1", got)
	}
	if got := payload.Stats.SkippedByReason[model.SkipUnsearchable]; got != 1 {
		t.Fatalf("unsearchable 跳过数 = %d，期望 1", got)
	}
	wantSamples := []string{"蜘蛛A", "蜘蛛B", "本地JS"}
	if len(payload.Stats.SkippedSamples) != len(wantSamples) {
		t.Fatalf("跳过样例 = %+v，期望 %+v", payload.Stats.SkippedSamples, wantSamples)
	}
	for i, sample := range wantSamples {
		if payload.Stats.SkippedSamples[i] != sample {
			t.Fatalf("跳过样例 = %+v，期望 %+v", payload.Stats.SkippedSamples, wantSamples)
		}
	}

	// 直播只导入 M3U：单仓 JSON 与 txt 列表计 nonM3uLive，ftp 地址计 invalidUrl。
	if len(payload.LiveSources) != 1 {
		t.Fatalf("直播源数 = %d，期望 1: %+v", len(payload.LiveSources), payload.LiveSources)
	}
	live := payload.LiveSources[0]
	if live.Name != "直播A" || live.EPG != "https://live.example.com/epg.xml.gz" {
		t.Fatalf("直播源异常: %+v", live)
	}
	if got := payload.Stats.SkippedByReason[model.SkipNonM3U]; got != 2 {
		t.Fatalf("nonM3uLive 跳过数 = %d，期望 2", got)
	}
	if got := payload.Stats.SkippedByReason[model.SkipInvalidURL]; got != 1 {
		t.Fatalf("invalidUrl 跳过数 = %d，期望 1", got)
	}
	if payload.Stats.Skipped != 9 {
		t.Fatalf("总跳过数 = %d，期望 9（点播 6 + 直播 3）", payload.Stats.Skipped)
	}
}

func TestParseTVBOXTypeTolerance(t *testing.T) {
	body := []byte(`{"sites":[
	  {"name":"字符串类型","type":"1","api":"https://a.example.com/api.php/provide/vod"},
	  {"name":"XML类型但命中CMS","type":0,"api":"https://b.example.com/api.php/provide/vod/"},
	  {"name":"缺类型但命中CMS","api":"https://c.example.com/api.php/provide/vod"},
	  {"name":"缺类型且非CMS","api":"https://d.example.com/v1/list"},
	  {"name":"未知类型","type":9,"api":"https://e.example.com/api.php/provide/vod"}
	]}`)
	payload, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse 返回错误: %v", err)
	}
	got := make([]string, 0, len(payload.Sources))
	for _, source := range payload.Sources {
		got = append(got, source.Name)
	}
	want := []string{"字符串类型", "XML类型但命中CMS", "缺类型但命中CMS"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("导入站点 = %v，期望 %v", got, want)
	}
	if payload.Stats.SkippedByReason[model.SkipXML] != 2 {
		t.Fatalf("xml 跳过数 = %d，期望 2（d/e 都无法识别）: %+v",
			payload.Stats.SkippedByReason[model.SkipXML], payload.Stats.SkippedByReason)
	}
}

func TestParseTVBOXTruncatesAndDedupes(t *testing.T) {
	var builder strings.Builder
	builder.WriteString(`{"sites":[`)
	for i := 0; i < 10; i++ {
		builder.WriteString(`{"name":"重复","type":1,"api":"https://dup.example.com/api.php/provide/vod"},`)
	}
	for i := 0; i < MaxVodSources+5; i++ {
		fmt.Fprintf(&builder, `{"name":"站点%d","type":1,"api":"https://vod%d.example.com/api.php/provide/vod"},`, i, i)
	}
	builder.WriteString(`],"lives":[`)
	for i := 0; i < MaxLiveSources+3; i++ {
		fmt.Fprintf(&builder, `{"name":"直播%d","type":0,"url":"https://live%d.example.com/list.m3u"},`, i, i)
	}
	builder.WriteString(`]}`)

	payload, err := Parse([]byte(builder.String()))
	if err != nil {
		t.Fatalf("Parse 返回错误: %v", err)
	}
	if len(payload.Sources) != MaxVodSources {
		t.Fatalf("点播源数 = %d，期望 %d", len(payload.Sources), MaxVodSources)
	}
	if payload.Sources[0].URL != "https://dup.example.com/api.php/provide/vod" {
		t.Fatalf("前段重复应只占 1 个名额: %q", payload.Sources[0].URL)
	}
	if len(payload.LiveSources) != MaxLiveSources {
		t.Fatalf("直播源数 = %d，期望 %d", len(payload.LiveSources), MaxLiveSources)
	}
	// 1 + 99 达到上限，余下 6 个点播与 3 个直播被截断。
	if payload.Stats.Truncated != 9 {
		t.Fatalf("截断数 = %d，期望 9", payload.Stats.Truncated)
	}
	if payload.Stats.Skipped != 0 {
		t.Fatalf("重复条目应静默跳过、不计入跳过统计: %+v", payload.Stats)
	}
}

func TestParseTVBOXAllSkipped(t *testing.T) {
	_, err := Parse([]byte(`{"sites":[{"name":"蜘蛛A","type":3,"api":"csp_AppYs"}]}`))
	requireInvalidContent(t, err)
	if !strings.Contains(err.Error(), "已跳过 1 个条目（Spider 引擎 1）") {
		t.Fatalf("错误文案 = %q", err)
	}
}

func TestParseTVBOXEmptySites(t *testing.T) {
	_, err := Parse([]byte(`{"sites":[],"lives":[]}`))
	requireInvalidContent(t, err)
	if !strings.Contains(err.Error(), "没有可用的站点") {
		t.Fatalf("错误文案 = %q", err)
	}
}

func TestParseTVBOXLiveLeniency(t *testing.T) {
	// type 缺省且地址不是 m3u 也不是 txt：按 TVBOX 既有语义导入（避免误杀无扩展名的播放列表）。
	payload, err := Parse([]byte(`{"lives":[{"name":"直播","url":"https://live.example.com/playlist"}]}`))
	if err != nil {
		t.Fatalf("Parse 返回错误: %v", err)
	}
	if len(payload.LiveSources) != 1 {
		t.Fatalf("直播源数 = %d，期望 1", len(payload.LiveSources))
	}
}

func TestIsAppleCMSAPI(t *testing.T) {
	cases := map[string]bool{
		"https://a.example.com/api.php/provide/vod":               true,
		"https://a.example.com/api.php/provide/vod/":              true,
		"https://a.example.com/api.php/provide/vod?ac=list":       true,
		"https://a.example.com/API.PHP/PROVIDE/VOD":               true,
		"https://a.example.com/api.php/provide/vod/at/xml":        false,
		"https://a.example.com/api.php/provide/vod/at/xml?x=1":    false,
		"https://a.example.com/api.php/provide/vod/at/xml/../vod": false,
		"https://a.example.com/v1/list":                           false,
		"csp_AppYs":                                               false,
		"":                                                        false,
	}
	for api, want := range cases {
		if got := isAppleCMSAPI(api); got != want {
			t.Fatalf("isAppleCMSAPI(%q) = %v，期望 %v", api, got, want)
		}
	}
}
