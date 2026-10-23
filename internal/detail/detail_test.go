package detail

import (
	"testing"

	"cilicili/internal/model"
)

func TestDecodeNormalizesPreferred(t *testing.T) {
	// 模拟升级前写入的缓存：没有 preferred 字段。
	raw := []byte(`{
		"episodes":["https://a.test/2/index.m3u8"],
		"playLines":[
			{"id":"l1","name":"share","index":1,"episodes":[{"id":"l1e1","name":"第01集","index":1,"url":"https://a.test/share/1"}]},
			{"id":"l2","name":"ffm3u8","index":2,"episodes":[{"id":"l2e1","name":"第01集","index":1,"url":"https://a.test/1/index.m3u8"}]}
		],
		"videoInfo":{"title":"t","sourceKey":"s","sourceName":"n"}
	}`)
	d, err := decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.PlayLines) != 2 {
		t.Fatalf("playLines = %+v", d.PlayLines)
	}
	if d.PlayLines[0].Preferred {
		t.Fatalf("first line should not be preferred: %+v", d.PlayLines[0])
	}
	if !d.PlayLines[1].Preferred {
		t.Fatalf("m3u8 line should be preferred: %+v", d.PlayLines[1])
	}
	if d.PlayLines[0].ID != "ln:share" || d.PlayLines[0].Episodes[0].ID != "e:第01集" {
		t.Fatalf("old ids should be rebuilt: %+v", d.PlayLines[0])
	}
}

func TestNormalizeNoPlayLines(t *testing.T) {
	d := normalize(&model.VideoDetail{})
	if d.Episodes == nil || d.PlayLines == nil {
		t.Fatalf("slices should be non-nil: %+v", d)
	}
	if len(d.PlayLines) != 0 {
		t.Fatalf("playLines = %+v", d.PlayLines)
	}
}

func TestNormalizeNoM3U8(t *testing.T) {
	d := normalize(&model.VideoDetail{PlayLines: []model.PlayLine{
		{ID: "l1", Episodes: []model.Episode{{ID: "l1e1", URL: "https://a.test/1.mp4"}}},
	}})
	if d.PlayLines[0].Preferred {
		t.Fatal("mp4-only line should not be preferred")
	}
}

func TestNormalizeKeepsSinglePreferred(t *testing.T) {
	d := normalize(&model.VideoDetail{PlayLines: []model.PlayLine{
		{ID: "l1", Preferred: true, Episodes: []model.Episode{{ID: "l1e1", URL: "https://a.test/1.mp4"}}},
		{ID: "l2", Episodes: []model.Episode{{ID: "l2e1", URL: "https://b.test/1/index.m3u8"}}},
	}})
	if d.PlayLines[0].Preferred || !d.PlayLines[1].Preferred {
		t.Fatalf("preferred flags = %+v", d.PlayLines)
	}
}

func TestCacheKeyVersioned(t *testing.T) {
	src := model.SourceConfig{URL: "https://a.test/api.php/provide/vod"}
	key := cacheKey("1", src)
	if key[:10] != "detail:v4:" {
		t.Fatalf("key = %q", key)
	}
}
