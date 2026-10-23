package cms

import (
	"encoding/json"
	"reflect"
	"testing"

	"cilicili/internal/model"
)

func detailJSON(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	fields["vod_id"] = 1
	fields["vod_name"] = "测试影片"
	body, err := json.Marshal(map[string]any{"code": 1, "list": []any{fields}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestParseDetailPlayLines(t *testing.T) {
	src := model.SourceConfig{Key: "s1", Name: "源1", URL: "https://s1.test/api.php/provide/vod"}
	data := detailJSON(t, map[string]any{
		"vod_play_from": "ffm3u8$$$llm3u8",
		"vod_play_url": "第01集$https://a.test/1/index.m3u8#第02集$https://a.test/2/index.m3u8" +
			"$$$正片$https://b.test/full.mp4#预告$https://b.test/trailer.mp4",
	})
	d, err := ParseDetail(data, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.PlayLines) != 2 {
		t.Fatalf("playLines = %d: %+v", len(d.PlayLines), d.PlayLines)
	}
	line0 := d.PlayLines[0]
	if line0.ID != "l1" || line0.Name != "ffm3u8" || line0.Index != 1 || !line0.Preferred {
		t.Fatalf("line0 = %+v", line0)
	}
	if len(line0.Episodes) != 2 {
		t.Fatalf("line0 episodes = %+v", line0.Episodes)
	}
	if got := line0.Episodes[0]; got.ID != "l1e1" || got.Name != "第01集" || got.Index != 1 ||
		got.URL != "https://a.test/1/index.m3u8" {
		t.Fatalf("episode0 = %+v", got)
	}
	if got := line0.Episodes[1]; got.ID != "l1e2" || got.Name != "第02集" || got.Index != 2 {
		t.Fatalf("episode1 = %+v", got)
	}
	line1 := d.PlayLines[1]
	if line1.Name != "llm3u8" || line1.Index != 2 || len(line1.Episodes) != 2 || line1.Preferred {
		t.Fatalf("line1 = %+v", line1)
	}
	if line1.Episodes[0].Name != "正片" || line1.Episodes[1].Name != "预告" {
		t.Fatalf("line1 names = %+v", line1.Episodes)
	}
	// 旧字段优先 m3u8 线路。
	wantEpisodes := []string{"https://a.test/1/index.m3u8", "https://a.test/2/index.m3u8"}
	if !reflect.DeepEqual(d.Episodes, wantEpisodes) {
		t.Fatalf("episodes = %#v, want %#v", d.Episodes, wantEpisodes)
	}
}

func TestParseDetailPlayLineNamesFallback(t *testing.T) {
	src := model.SourceConfig{Key: "s1", Name: "源1", URL: "https://s1.test/api.php/provide/vod"}
	data := detailJSON(t, map[string]any{
		"vod_play_url": "第01集$https://a.test/1/index.m3u8$$$第01集$https://b.test/1/index.m3u8",
	})
	d, err := ParseDetail(data, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.PlayLines) != 2 {
		t.Fatalf("playLines = %+v", d.PlayLines)
	}
	if d.PlayLines[0].Name != "线路1" || d.PlayLines[1].Name != "线路2" {
		t.Fatalf("fallback names = %q, %q", d.PlayLines[0].Name, d.PlayLines[1].Name)
	}
}

func TestParseDetailBareEpisodeURLKept(t *testing.T) {
	src := model.SourceConfig{Key: "s1", Name: "源1", URL: "https://s1.test/api.php/provide/vod"}
	data := detailJSON(t, map[string]any{
		"vod_play_url": "https://a.test/1/index.m3u8#第02集$https://a.test/2/index.m3u8",
	})
	d, err := ParseDetail(data, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.PlayLines) != 1 || len(d.PlayLines[0].Episodes) != 2 {
		t.Fatalf("playLines = %+v", d.PlayLines)
	}
	if d.PlayLines[0].Episodes[0].Name != "" {
		t.Fatalf("bare episode name should be empty: %+v", d.PlayLines[0].Episodes[0])
	}
	if d.PlayLines[0].Episodes[0].ID != "l1e1" {
		t.Fatalf("id = %q", d.PlayLines[0].Episodes[0].ID)
	}
	// 旧字段仍按原语义丢弃无集名地址。
	if len(d.Episodes) != 1 || d.Episodes[0] != "https://a.test/2/index.m3u8" {
		t.Fatalf("legacy episodes = %#v", d.Episodes)
	}
}

func TestParseDetailContentFallbackPlayLines(t *testing.T) {
	src := model.SourceConfig{Key: "s1", Name: "源1", URL: "https://s1.test/api.php/provide/vod"}
	data := detailJSON(t, map[string]any{
		"vod_content": "简介里给了地址 $https://cdn.test/1/index.m3u8 和 $https://cdn.test/2/index.m3u8",
	})
	d, err := ParseDetail(data, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.PlayLines) != 1 || len(d.PlayLines[0].Episodes) != 2 {
		t.Fatalf("playLines = %+v", d.PlayLines)
	}
	if got := d.PlayLines[0].Episodes[0]; got.Name != "" || got.ID != "l1e1" || got.Index != 1 {
		t.Fatalf("episode = %+v", got)
	}
	if len(d.Episodes) != 2 {
		t.Fatalf("legacy episodes = %#v", d.Episodes)
	}
}

func TestParseDetailPageHTMLPlayLines(t *testing.T) {
	src := model.SourceConfig{Key: "s1", Name: "源1", URL: "https://s1.test/api.php/provide/vod"}
	page := []byte(`<html><h1>标题</h1><div class="sketch">简介</div>
<script>var p = "$https://cdn.test/1/index.m3u8#$https://cdn.test/2/index.m3u8";</script></html>`)
	d := ParseDetailPageHTML(page, src)
	if d == nil {
		t.Fatal("nil detail")
	}
	if len(d.PlayLines) != 1 || len(d.PlayLines[0].Episodes) != 2 {
		t.Fatalf("playLines = %+v", d.PlayLines)
	}
	if d.PlayLines[0].Episodes[0].ID != "l1e1" || d.PlayLines[0].Name != "线路1" || !d.PlayLines[0].Preferred {
		t.Fatalf("line = %+v", d.PlayLines[0])
	}
	if len(d.Episodes) != 2 {
		t.Fatalf("episodes = %#v", d.Episodes)
	}
}

func TestParsePlayLinesEmpty(t *testing.T) {
	if got := ParsePlayLines("", ""); got == nil || len(got) != 0 {
		t.Fatalf("empty = %#v", got)
	}
	if got := ParsePlayLines("第01集$ftp://a/1.m3u8", ""); got == nil || len(got) != 0 {
		t.Fatalf("non-http = %#v", got)
	}
}
