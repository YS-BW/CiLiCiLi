package live

import (
	"errors"
	"strings"
	"testing"
	"time"

	"cilicili/internal/model"
)

// xmltvTimestamp 把时间格式化为 `YYYYMMDDHHMMSS +0800`（XMLTV 最常见的写法）。
func xmltvTimestamp(t time.Time) string {
	zone := time.FixedZone("CST", 8*3600)
	return t.In(zone).Format("20060102150405") + " +0800"
}

func TestParseXMLTVBasic(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	currentStart := now.Add(-30 * time.Minute)
	currentStop := now.Add(30 * time.Minute)
	nextStart := now.Add(time.Hour)
	nextStop := now.Add(2 * time.Hour)

	body := `<?xml version="1.0" encoding="UTF-8"?>
<tv>
  <channel id="cctv1"><display-name>CCTV-1</display-name></channel>
  <programme start="` + xmltvTimestamp(currentStart) + `" stop="` + xmltvTimestamp(currentStop) + `" channel="cctv1">
    <title lang="zh">新闻 30&#39; 分 &amp; 天气</title>
    <desc><![CDATA[午间<新闻>]]></desc>
  </programme>
  <programme start="` + xmltvTimestamp(nextStart) + `" stop="` + xmltvTimestamp(nextStop) + `" channel="cctv1">
    <title>未来节目</title>
  </programme>
  <programme start="` + xmltvTimestamp(now.Add(-3*time.Hour)) + `" stop="` + xmltvTimestamp(now.Add(-2*time.Hour)) + `" channel="cctv1">
    <title>已结束</title>
  </programme>
  <programme start="` + xmltvTimestamp(now.Add(25*time.Hour)) + `" stop="` + xmltvTimestamp(now.Add(26*time.Hour)) + `" channel="cctv1">
    <title>窗口之外</title>
  </programme>
  <programme start="bad-time" stop="` + xmltvTimestamp(nextStop) + `" channel="cctv1">
    <title>坏时间</title>
  </programme>
  <programme start="` + xmltvTimestamp(nextStart) + `" stop="` + xmltvTimestamp(nextStop) + `" channel="cctv1">
    <desc>没有标题</desc>
  </programme>
  <programme start="` + xmltvTimestamp(nextStart) + `" stop="` + xmltvTimestamp(nextStop) + `">
    <title>没有频道</title>
  </programme>
</tv>`

	programs, err := ParseXMLTV([]byte(body), 24*time.Hour, now)
	if err != nil {
		t.Fatalf("ParseXMLTV 失败: %v", err)
	}
	list := programs["cctv1"]
	if len(list) != 2 {
		t.Fatalf("cctv1 节目数 = %d，期望 2: %+v", len(list), list)
	}
	if list[0].Title != "新闻 30' 分 & 天气" {
		t.Errorf("Title = %q（实体/引号解码失败）", list[0].Title)
	}
	if list[0].Desc != "午间<新闻>" {
		t.Errorf("Desc = %q（CDATA 解码失败）", list[0].Desc)
	}
	if list[0].Start != currentStart.UnixMilli() || list[0].Stop != currentStop.UnixMilli() {
		t.Errorf("时间 = %d~%d，期望 %d~%d", list[0].Start, list[0].Stop,
			currentStart.UnixMilli(), currentStop.UnixMilli())
	}
	if list[1].Start != nextStart.UnixMilli() {
		t.Errorf("未来节目 start = %d，期望 %d", list[1].Start, nextStart.UnixMilli())
	}
	if _, ok := programs["没有频道"]; ok {
		t.Error("缺少 channel 的节目不应被索引")
	}
}

func TestParseXMLTVSortsByStart(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	body := `<tv>
  <programme start="` + xmltvTimestamp(now.Add(2*time.Hour)) + `" stop="` + xmltvTimestamp(now.Add(3*time.Hour)) + `" channel="a"><title>第三个</title></programme>
  <programme start="` + xmltvTimestamp(now.Add(-time.Minute)) + `" stop="` + xmltvTimestamp(now.Add(30*time.Minute)) + `" channel="a"><title>第一个</title></programme>
  <programme start="` + xmltvTimestamp(now.Add(time.Hour)) + `" stop="` + xmltvTimestamp(now.Add(2*time.Hour)) + `" channel="a"><title>第二个</title></programme>
</tv>`

	programs, err := ParseXMLTV([]byte(body), 24*time.Hour, now)
	if err != nil {
		t.Fatalf("ParseXMLTV 失败: %v", err)
	}
	list := programs["a"]
	if len(list) != 3 {
		t.Fatalf("节目数 = %d", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].Start > list[i].Start {
			t.Fatalf("节目未按开播时间排序: %+v", list)
		}
	}
}

func TestParseXMLTVWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	body := `<tv>
  <programme start="` + xmltvTimestamp(now.Add(-time.Hour)) + `" stop="` + xmltvTimestamp(now) + `" channel="a"><title>正好结束</title></programme>
  <programme start="` + xmltvTimestamp(now.Add(59*time.Minute)) + `" stop="` + xmltvTimestamp(now.Add(2*time.Hour)) + `" channel="a"><title>窗口内</title></programme>
  <programme start="` + xmltvTimestamp(now.Add(time.Hour)) + `" stop="` + xmltvTimestamp(now.Add(2*time.Hour)) + `" channel="a"><title>正好在窗口边界</title></programme>
</tv>`

	programs, err := ParseXMLTV([]byte(body), time.Hour, now)
	if err != nil {
		t.Fatalf("ParseXMLTV 失败: %v", err)
	}
	list := programs["a"]
	if len(list) != 1 {
		t.Fatalf("窗口内节目数 = %d，期望 1: %+v", len(list), list)
	}
	if list[0].Title != "窗口内" {
		t.Errorf("Title = %q", list[0].Title)
	}
}

func TestParseXMLTVGzip(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	body := `<tv><programme start="` + xmltvTimestamp(now.Add(-time.Minute)) +
		`" stop="` + xmltvTimestamp(now.Add(time.Hour)) + `" channel="a"><title>压缩节目</title></programme></tv>`

	programs, err := ParseXMLTV(gzipBytes(t, body), 24*time.Hour, now)
	if err != nil {
		t.Fatalf("ParseXMLTV(gzip) 失败: %v", err)
	}
	list := programs["a"]
	if len(list) != 1 || list[0].Title != "压缩节目" {
		t.Fatalf("gzip 解析结果不符合预期: %+v", list)
	}
}

func TestParseXMLTVBadGzip(t *testing.T) {
	t.Parallel()

	data := []byte{0x1f, 0x8b, 0x00, 0x01, 0x02}
	if _, err := ParseXMLTV(data, time.Hour, time.Now()); !errors.Is(err, ErrInvalidXMLTV) {
		t.Fatalf("err = %v，期望 ErrInvalidXMLTV", err)
	}
}

func TestParseXMLTVRejectsNonXMLTV(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"空内容":     "",
		"HTML错误页": "<html><body>404 Not Found</body></html>",
		"其他XML":   `<rss><channel><title>订阅</title></channel></rss>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseXMLTV([]byte(body), time.Hour, time.Now()); !errors.Is(err, ErrInvalidXMLTV) {
				t.Fatalf("err = %v，期望 ErrInvalidXMLTV", err)
			}
		})
	}
}

func TestParseXMLTVEmptyReturnsEmptyMap(t *testing.T) {
	t.Parallel()

	programs, err := ParseXMLTV([]byte("<tv></tv>"), time.Hour, time.Now())
	if err != nil {
		t.Fatalf("ParseXMLTV 失败: %v", err)
	}
	if programs == nil {
		t.Fatal("programs 不应为 nil")
	}
	if len(programs) != 0 {
		t.Errorf("programs = %+v，期望空", programs)
	}
}

func TestParseXMLTVLatin1(t *testing.T) {
	t.Parallel()

	now := time.Now()
	body := `<?xml version="1.0" encoding="ISO-8859-1"?><tv><programme start="` +
		xmltvTimestamp(now.Add(-time.Minute)) + `" stop="` + xmltvTimestamp(now.Add(time.Hour)) +
		`" channel="a"><title>Caf` + string([]byte{0xE9}) + `</title></programme></tv>`

	programs, err := ParseXMLTV([]byte(body), time.Hour, now)
	if err != nil {
		t.Fatalf("ParseXMLTV(latin1) 失败: %v", err)
	}
	if got := programs["a"][0].Title; got != "Café" {
		t.Errorf("Title = %q，期望 Café", got)
	}
}

func TestParseXMLTVTime(t *testing.T) {
	t.Parallel()

	utc := func(hour, minute, second int) int64 {
		return time.Date(2024, 1, 1, hour, minute, second, 0, time.UTC).UnixMilli()
	}
	cases := []struct {
		raw    string
		want   int64
		wantOK bool
	}{
		{"20240101120000 +0800", utc(4, 0, 0), true},
		{"20240101120000", utc(12, 0, 0), true},
		{"20240101120000Z", utc(12, 0, 0), true},
		{"20240101120000 -0500", utc(17, 0, 0), true},
		{"20240101120000 +0000", utc(12, 0, 0), true},
		{"2024010112", utc(12, 0, 0), true},
		{"20240101120000.5 +0000", utc(12, 0, 0) + 500, true},
		{"20240101120000.123456 +0000", utc(12, 0, 0) + 123, true},
		{"20240101120000.123456789 +0000", utc(12, 0, 0) + 123, true},
		{"  20240101120000 +0800  ", utc(4, 0, 0), true},
		{"2024-01-01 12:00:00 +0800", 0, false},
		{"20241301120000 +0800", 0, false},
		{"", 0, false},
		{"not-a-time", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseXMLTVTime(tc.raw)
		if ok != tc.wantOK {
			t.Errorf("parseXMLTVTime(%q) ok = %v，期望 %v", tc.raw, ok, tc.wantOK)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseXMLTVTime(%q) = %d，期望 %d", tc.raw, got, tc.want)
		}
	}
}

func TestCurrentAndNext(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	programs := []model.EpgProgram{
		{ChannelID: "c", Start: ms(-2 * time.Hour), Stop: ms(-time.Hour), Title: "past"},
		{ChannelID: "c", Start: ms(-time.Hour), Stop: ms(time.Hour), Title: "current-1"},
		{ChannelID: "c", Start: ms(-time.Minute), Stop: ms(2 * time.Hour), Title: "current-2"},
		{ChannelID: "c", Start: ms(3 * time.Hour), Stop: ms(4 * time.Hour), Title: "next"},
		{ChannelID: "c", Start: ms(5 * time.Hour), Stop: ms(6 * time.Hour), Title: "later"},
	}

	current, next := CurrentAndNext(programs, now)
	if current == nil || current.Title != "current-2" {
		t.Fatalf("current = %+v，期望重叠节目取最后一个（current-2）", current)
	}
	if next == nil || next.Title != "next" {
		t.Fatalf("next = %+v，期望 next", next)
	}

	// 无覆盖节目时 current 为 nil；无未来节目时 next 为 nil。
	current, next = CurrentAndNext([]model.EpgProgram{
		{ChannelID: "c", Start: ms(2 * time.Hour), Stop: ms(3 * time.Hour), Title: "future"},
	}, now)
	if current != nil {
		t.Errorf("current = %+v，期望 nil", current)
	}
	if next == nil || next.Title != "future" {
		t.Errorf("next = %+v", next)
	}

	current, next = CurrentAndNext(nil, now)
	if current != nil || next != nil {
		t.Errorf("空节目单应返回 nil/nil，实际 %+v/%+v", current, next)
	}

	// 未排序的输入也要找到最早开播的未来节目。
	current, next = CurrentAndNext([]model.EpgProgram{
		{ChannelID: "c", Start: ms(5 * time.Hour), Stop: ms(6 * time.Hour), Title: "later"},
		{ChannelID: "c", Start: ms(2 * time.Hour), Stop: ms(3 * time.Hour), Title: "soon"},
	}, now)
	if current != nil {
		t.Errorf("current = %+v，期望 nil", current)
	}
	if next == nil || !strings.HasPrefix(next.Title, "soon") {
		t.Errorf("next = %+v，期望 soon", next)
	}
}

func TestParseXMLTVToleratesTruncatedBody(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	// 第二段节目在标题处被截断：已解析的第一段应保留，而不是整份 EPG 报错。
	body := `<tv>
  <programme start="` + xmltvTimestamp(now.Add(-time.Minute)) + `" stop="` + xmltvTimestamp(now.Add(time.Hour)) + `" channel="a"><title>完整节目</title></programme>
  <programme start="` + xmltvTimestamp(now.Add(2*time.Hour)) + `" stop="` + xmltvTimestamp(now.Add(3*time.Hour)) + `" channel="a"><ti`

	programs, err := ParseXMLTV([]byte(body), 24*time.Hour, now)
	if err != nil {
		t.Fatalf("截断的节目单应尽力解析，实际错误: %v", err)
	}
	list := programs["a"]
	if len(list) != 1 || list[0].Title != "完整节目" {
		t.Fatalf("截断处理结果 = %+v，期望保留第一段节目", list)
	}
}
