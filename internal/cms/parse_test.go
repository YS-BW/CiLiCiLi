package cms

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cilicili/internal/model"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取 fixture %s 失败: %v", name, err)
	}
	return data
}

func lzSource() model.SourceConfig {
	return model.SourceConfig{
		Key:  "lz",
		Name: "量子资源",
		URL:  "https://cj.lziapi.com/api.php/provide/vod/",
	}
}

func ffzySource() model.SourceConfig {
	return model.SourceConfig{
		Key:  "ffzy",
		Name: "非凡资源",
		URL:  "https://cj.ffzyapi.com/api.php/provide/vod/",
	}
}

// ---------------------------------------------------------------- 请求头

func TestRequestHeaders(t *testing.T) {
	h := RequestHeaders("  custom-ua  ")
	if got := h.Get("User-Agent"); got != "custom-ua" {
		t.Fatalf("User-Agent = %q, want custom-ua", got)
	}
	if got := h.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q, want application/json", got)
	}
	for _, ua := range []string{"", "   "} {
		if got := RequestHeaders(ua).Get("User-Agent"); got != DefaultUserAgent {
			t.Fatalf("空 UA 应回退默认值，got %q", got)
		}
	}
}

// ---------------------------------------------------------------- 搜索

func TestParseSearchListRealLZ(t *testing.T) {
	data := readFixture(t, "search_lz.json")
	src := lzSource()
	items, pageCount, err := ParseSearchList(data, src)
	if err != nil {
		t.Fatalf("ParseSearchList: %v", err)
	}
	if pageCount != 1 {
		t.Fatalf("pageCount = %d, want 1", pageCount)
	}
	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3", len(items))
	}

	first := items[0]
	want := model.SearchResultItem{
		SourceKey:  "lz",
		SourceName: "量子资源",
		VodID:      "88770",
		Name:       "庆余年第二季",
		Pic:        "https://img.lzipic.com/upload/vod/20240516-1/a6531f51b9ed9393d6647aa8d5340ca0.jpg",
		TypeName:   "国产剧",
		Year:       "2024",
		Area:       "大陆",
		Remarks:    "已完结",
		SourceURL:  "https://cj.lziapi.com/api.php/provide/vod/",
		IsAdult:    false,
	}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("first item = %#v\nwant %#v", first, want)
	}
	if items[1].Name != "庆余年[电影解说]" || items[1].TypeName != "电影解说" {
		t.Fatalf("second item = %#v", items[1])
	}
	if items[2].VodID != "17701" || items[2].Name != "庆余年第一季" {
		t.Fatalf("third item = %#v", items[2])
	}
}

func TestParseSearchListRealFFZY(t *testing.T) {
	src := ffzySource()
	items, pageCount, err := ParseSearchList(readFixture(t, "search_ffzy.json"), src)
	if err != nil {
		t.Fatalf("ParseSearchList: %v", err)
	}
	if pageCount != 1 {
		t.Fatalf("pageCount = %d, want 1（源站 pagecount 为字符串 \"1\"）", pageCount)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	// 源站 vod_id 是数字 79613，要转成字符串。
	if items[0].VodID != "79613" || items[0].Name != "庆余年第二季粤语" {
		t.Fatalf("first item = %#v", items[0])
	}
	if items[1].VodID != "75904" || items[1].Name != "庆余年之风起沧州" {
		t.Fatalf("second item = %#v", items[1])
	}
}

func TestParseSearchListEmptyVariants(t *testing.T) {
	cases := map[string]string{
		"list 为 null":   `{"code":1,"msg":"数据列表","page":1,"pagecount":"0","total":0,"list":null}`,
		"缺少 list":       `{"code":1,"msg":"数据列表"}`,
		"空对象":           `{}`,
		"空数组":           `{"list":[]}`,
		"带 BOM 与空白":     "\xef\xbb\xbf  \n{\"list\":null}\n ",
		"pagecount 非法":  `{"list":null,"pagecount":"abc"}`,
		"pagecount 为 0": `{"list":[],"pagecount":0}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			items, pageCount, err := ParseSearchList([]byte(payload), lzSource())
			if err != nil {
				t.Fatalf("ParseSearchList(%s): %v", payload, err)
			}
			if items == nil {
				t.Fatal("空结果应返回非 nil 切片，便于序列化为 []")
			}
			if len(items) != 0 {
				t.Fatalf("len(items) = %d, want 0", len(items))
			}
			if pageCount != 1 {
				t.Fatalf("pageCount = %d, want 1", pageCount)
			}
		})
	}
}

func TestParseSearchListInvalid(t *testing.T) {
	cases := map[string]string{
		"null":      `null`,
		"JSON 数组":   `[]`,
		"JSON 字符串":  `"data"`,
		"空输入":       ``,
		"list 是对象":  `{"list":{}}`,
		"list 是字符串": `{"list":"x"}`,
		"list 是数字":  `{"list":123}`,
		"畸形 JSON":   `{"list":[}`,
		"截断 JSON":   `{"list":null`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			items, _, err := ParseSearchList([]byte(payload), lzSource())
			if err == nil {
				t.Fatalf("ParseSearchList(%s) 应报错，got items=%#v", payload, items)
			}
			if !strings.Contains(err.Error(), "数据格式无效") {
				t.Fatalf("错误信息不够清晰: %v", err)
			}
		})
	}
}

func TestParseSearchListPageCount(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    int
	}{
		{"数字", `{"pagecount":3}`, 3},
		{"字符串", `{"pagecount":"3"}`, 3},
		{"带空白字符串", `{"pagecount":" 3 "}`, 3},
		{"小数前缀", `{"pagecount":"3.9"}`, 3},
		{"数字后缀", `{"pagecount":"3abc"}`, 3},
		{"浮点数字段", `{"pagecount":2.0}`, 2},
		{"零", `{"pagecount":0}`, 1},
		{"负数", `{"pagecount":-5}`, 1},
		{"空字符串", `{"pagecount":""}`, 1},
		{"非数字", `{"pagecount":"abc"}`, 1},
		{"null", `{"pagecount":null}`, 1},
		{"缺失", `{}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, pageCount, err := ParseSearchList([]byte(tc.payload), lzSource())
			if err != nil {
				t.Fatalf("ParseSearchList: %v", err)
			}
			if pageCount != tc.want {
				t.Fatalf("pageCount = %d, want %d", pageCount, tc.want)
			}
		})
	}
}

func TestParseSearchListTrimsEntitiesAndOmitEmpty(t *testing.T) {
	payload := `{"list":[{
		"vod_id": 42,
		"vod_name": "  摔跤吧！爸爸 &amp; 番外  ",
		"vod_pic": "  https://img.example.com/a.jpg  ",
		"type_name": " 动作片 ",
		"vod_year": 2023,
		"vod_area": "",
		"vod_remarks": "HD&nbsp;"
	}]}`
	items, _, err := ParseSearchList([]byte(payload), lzSource())
	if err != nil {
		t.Fatalf("ParseSearchList: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	got := items[0]
	if got.VodID != "42" {
		t.Fatalf("VodID = %q, want 42（数字要转字符串）", got.VodID)
	}
	if got.Name != "摔跤吧！爸爸 & 番外" {
		t.Fatalf("Name = %q", got.Name)
	}
	if got.Pic != "https://img.example.com/a.jpg" {
		t.Fatalf("Pic = %q", got.Pic)
	}
	if got.TypeName != "动作片" {
		t.Fatalf("TypeName = %q", got.TypeName)
	}
	if got.Year != "2023" {
		t.Fatalf("Year = %q", got.Year)
	}
	if got.Area != "" {
		t.Fatalf("Area = %q, want 空串", got.Area)
	}
	if got.Remarks != "HD" {
		t.Fatalf("Remarks = %q, want HD", got.Remarks)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	jsonText := string(raw)
	if strings.Contains(jsonText, `"area"`) {
		t.Fatalf("空字段不应出现在 JSON 中: %s", jsonText)
	}
	if strings.Contains(jsonText, `"isAdult"`) {
		t.Fatalf("isAdult=false 不应出现在 JSON 中: %s", jsonText)
	}
}

func TestParseSearchListSkipsMalformedRows(t *testing.T) {
	payload := `{"list":[{"vod_id":"1","vod_name":"正常"},42,"x",null,{"vod_name":"无 ID"}]}`
	items, _, err := ParseSearchList([]byte(payload), lzSource())
	if err != nil {
		t.Fatalf("ParseSearchList: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2（非对象行应跳过）", len(items))
	}
	if items[0].Name != "正常" || items[1].Name != "无 ID" {
		t.Fatalf("items = %#v", items)
	}
}

func TestParseSearchListPropagatesIsAdult(t *testing.T) {
	src := lzSource()
	src.IsAdult = true
	items, _, err := ParseSearchList([]byte(`{"list":[{"vod_id":"1","vod_name":"x"}]}`), src)
	if err != nil {
		t.Fatalf("ParseSearchList: %v", err)
	}
	if !items[0].IsAdult {
		t.Fatal("IsAdult 应透传源配置")
	}
}

// ---------------------------------------------------------------- 详情

func TestParseDetailRealLZ(t *testing.T) {
	detail, err := ParseDetail(readFixture(t, "detail_lz.json"), lzSource())
	if err != nil {
		t.Fatalf("ParseDetail: %v", err)
	}
	if len(detail.Episodes) != 36 {
		t.Fatalf("len(Episodes) = %d, want 36", len(detail.Episodes))
	}
	// 第一条线路是网页中转页，必须选第二条 m3u8 线路。
	if got := detail.Episodes[0]; got != "https://v.lzcdn28.com/20250923/2326_a9bf234a/index.m3u8" {
		t.Fatalf("Episodes[0] = %q", got)
	}
	for i, ep := range detail.Episodes {
		if !strings.Contains(ep, ".m3u8") || strings.Contains(ep, "/share/") {
			t.Fatalf("Episodes[%d] = %q，应全部是 m3u8 直链", i, ep)
		}
	}

	info := detail.VideoInfo
	if info.Title != "庆余年第二季" {
		t.Fatalf("Title = %q", info.Title)
	}
	if info.TypeName != "国产剧" || info.Year != "2024" || info.Area != "大陆" {
		t.Fatalf("元信息 = %#v", info)
	}
	if info.Director != "孙皓" {
		t.Fatalf("Director = %q", info.Director)
	}
	if !strings.Contains(info.Actor, "张若昀") {
		t.Fatalf("Actor = %q", info.Actor)
	}
	if info.Cover == "" || info.Remarks != "已完结" {
		t.Fatalf("Cover/Remarks = %#v", info)
	}
	if !strings.Contains(info.Desc, "范闲") || strings.Contains(info.Desc, "<p>") {
		t.Fatalf("Desc 未正确处理: %q", info.Desc)
	}
	if info.SourceKey != "lz" || info.SourceName != "量子资源" || info.SourceURL != lzSource().URL {
		t.Fatalf("来源字段 = %#v", info)
	}
}

func TestParseDetailContentFallback(t *testing.T) {
	payload := `{"list":[{
		"vod_name": "某剧",
		"vod_play_url": "",
		"vod_content": "<p>若播放列表失效，可访问 $https://cdn.example.com/20240101/123_abcdef/index.m3u8 观看</p>"
	}]}`
	detail, err := ParseDetail([]byte(payload), lzSource())
	if err != nil {
		t.Fatalf("ParseDetail: %v", err)
	}
	want := []string{"https://cdn.example.com/20240101/123_abcdef/index.m3u8"}
	if !reflect.DeepEqual(detail.Episodes, want) {
		t.Fatalf("Episodes = %#v, want %#v", detail.Episodes, want)
	}
}

func TestParseDetailOnlyFallsBackWhenEpisodesEmpty(t *testing.T) {
	// 与 LibreTV 一致：只要播放列表能解析出 http 地址（哪怕是中转页），就不再读简介兜底。
	payload := `{"list":[{
		"vod_play_url": "第01集$https://cdn.example.com/share/abc",
		"vod_content": "$https://cdn.example.com/20240101/123_abcdef/index.m3u8"
	}]}`
	detail, err := ParseDetail([]byte(payload), lzSource())
	if err != nil {
		t.Fatalf("ParseDetail: %v", err)
	}
	want := []string{"https://cdn.example.com/share/abc"}
	if !reflect.DeepEqual(detail.Episodes, want) {
		t.Fatalf("Episodes = %#v, want %#v", detail.Episodes, want)
	}
}

func TestParseDetailInvalid(t *testing.T) {
	cases := map[string]string{
		"null":           `null`,
		"JSON 数组":        `[]`,
		"缺少 list":        `{}`,
		"空列表":            `{"list":[]}`,
		"list 为 null":    `{"list":null}`,
		"list 是对象":       `{"list":{}}`,
		"list[0] 是 null": `{"list":[null]}`,
		"list[0] 是字符串":   `{"list":["x"]}`,
		"畸形 JSON":        `{"list":[{`,
		"空输入":            ``,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			detail, err := ParseDetail([]byte(payload), lzSource())
			if err == nil {
				t.Fatalf("ParseDetail(%s) 应报错，got %#v", payload, detail)
			}
			if !strings.Contains(err.Error(), "详情内容无效") {
				t.Fatalf("错误信息不够清晰: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------- 播放地址提取

func TestExtractEpisodesFromPlayURL(t *testing.T) {
	cases := []struct {
		name    string
		playURL string
		want    []string
	}{
		{"空字符串", "", []string{}},
		{"单线路", "第01集$https://a.com/1/index.m3u8#第02集$https://a.com/2/index.m3u8", []string{
			"https://a.com/1/index.m3u8", "https://a.com/2/index.m3u8",
		}},
		{"优先 m3u8 线路", "第01集$https://a.com/share/1#第02集$https://a.com/share/2$$$第01集$https://b.com/1/index.m3u8#第02集$https://b.com/2/index.m3u8", []string{
			"https://b.com/1/index.m3u8", "https://b.com/2/index.m3u8",
		}},
		{"无 m3u8 回退第一条线路", "第01集$https://a.com/1.mp4#第02集$https://a.com/2.mp4$$$第01集$https://b.com/1.mp4", []string{
			"https://a.com/1.mp4", "https://a.com/2.mp4",
		}},
		{"空线路被忽略", "$$$第01集$https://a.com/1/index.m3u8", []string{"https://a.com/1/index.m3u8"}},
		{"裸 URL 丢弃", "https://a.com/1/index.m3u8", []string{}},
		{"非 http 协议丢弃", "第01集$ftp://a.com/1/index.m3u8#第02集$magnet:?xt=urn:btih:x", []string{}},
		{"分段前后空白", " 第01集$https://a.com/1/index.m3u8 # 第02集$https://a.com/2/index.m3u8 ", []string{
			"https://a.com/1/index.m3u8", "https://a.com/2/index.m3u8",
		}},
		{"还原 HTML 实体", "第01集$https://a.com/1/index.m3u8?token=1&amp;sign=2", []string{
			"https://a.com/1/index.m3u8?token=1&sign=2",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractEpisodesFromPlayURL(tc.playURL)
			if got == nil {
				t.Fatal("应返回非 nil 切片")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestExtractM3U8FromText(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"空字符串", "", []string{}},
		{"单个链接", "播放地址 $https://a.com/x/index.m3u8 备用", []string{"https://a.com/x/index.m3u8"}},
		{"多个链接", "$https://a.com/1/index.m3u8和$http://b.com/2/index.m3u8", []string{
			"https://a.com/1/index.m3u8", "http://b.com/2/index.m3u8",
		}},
		{"无 $ 前缀不提取", "https://a.com/x/index.m3u8", []string{}},
		{"引号内链接", `src="$https://a.com/x/index.m3u8"`, []string{"https://a.com/x/index.m3u8"}},
		{"截断到 .m3u8 为止", "$https://a.com/x/index.m3u8?token=1&a=2", []string{"https://a.com/x/index.m3u8"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractM3U8FromText(tc.text)
			if got == nil {
				t.Fatal("应返回非 nil 切片")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- 详情页 HTML 兜底

func TestParseDetailPageHTML(t *testing.T) {
	detail := ParseDetailPageHTML(readFixture(t, "detail_page_ffzy.html"), ffzySource())
	wantEpisodes := []string{
		"https://vip.ffzy-online2.com/20250420/49522_e3154fc9/index.m3u8",
		"https://vip.ffzy-online2.com/20250422/49553_6e2f66ca/index.m3u8",
		"https://vip.ffzy-online2.com/20250423/49563_ba2fec29/index.m3u8",
	}
	if !reflect.DeepEqual(detail.Episodes, wantEpisodes) {
		t.Fatalf("Episodes = %#v\nwant %#v", detail.Episodes, wantEpisodes)
	}
	if detail.VideoInfo.Title != "庆余年 第二季 & 番外" {
		t.Fatalf("Title = %q", detail.VideoInfo.Title)
	}
	wantDesc := "该剧改编自猫腻同名畅销小说，承接上季， 范闲（张若昀 饰）率领使团回归途中。 庆帝遭遇三连刺杀。"
	if detail.VideoInfo.Desc != wantDesc {
		t.Fatalf("Desc = %q\nwant %q", detail.VideoInfo.Desc, wantDesc)
	}
	if detail.VideoInfo.SourceKey != "ffzy" || detail.VideoInfo.SourceName != "非凡资源" {
		t.Fatalf("来源字段 = %#v", detail.VideoInfo)
	}
}

func TestParseDetailPageHTMLParenSuffix(t *testing.T) {
	page := []byte(`<div class="playlist"><li>第01集$https://cdn.example.com/vod/a(1)/index.m3u8</li></div>`)
	detail := ParseDetailPageHTML(page, lzSource())
	want := []string{"https://cdn.example.com/vod/a"}
	if !reflect.DeepEqual(detail.Episodes, want) {
		t.Fatalf("Episodes = %#v, want %#v（应截断 ( 之后的尾缀）", detail.Episodes, want)
	}
}

func TestParseDetailPageHTMLDateHashFallback(t *testing.T) {
	// 日期哈希路径同样以 $ 开头；通用规则先命中，两个模式结果一致（与 LibreTV 的兜底顺序对齐）。
	page := []byte(`<script>var u="第01集$https://vip.ffzy-online2.com/20250420/49522_e3154fc9/index.m3u8";</script>`)
	detail := ParseDetailPageHTML(page, lzSource())
	want := []string{"https://vip.ffzy-online2.com/20250420/49522_e3154fc9/index.m3u8"}
	if !reflect.DeepEqual(detail.Episodes, want) {
		t.Fatalf("Episodes = %#v, want %#v", detail.Episodes, want)
	}
}

func TestParseDetailPageHTMLEmpty(t *testing.T) {
	for _, page := range [][]byte{nil, {}} {
		detail := ParseDetailPageHTML(page, lzSource())
		if detail == nil {
			t.Fatal("不应返回 nil")
		}
		if detail.Episodes == nil {
			t.Fatal("Episodes 应返回非 nil 切片")
		}
		if len(detail.Episodes) != 0 || detail.VideoInfo.Title != "" || detail.VideoInfo.Desc != "" {
			t.Fatalf("空页面结果 = %#v", detail)
		}
		if detail.VideoInfo.SourceKey != "lz" {
			t.Fatalf("SourceKey = %q", detail.VideoInfo.SourceKey)
		}
	}
}

func TestParseDetailPageHTMLBOM(t *testing.T) {
	page := []byte("\xef\xbb\xbf<h1>测试标题</h1>")
	detail := ParseDetailPageHTML(page, lzSource())
	if detail.VideoInfo.Title != "测试标题" {
		t.Fatalf("Title = %q", detail.VideoInfo.Title)
	}
}
