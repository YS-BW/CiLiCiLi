package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestMediaSearchAndCover(t *testing.T) {
	ts, _ := newTestServer(t)

	// 未登录不能访问媒体详情。
	status, _ := doJSON(t, ts, http.MethodGet, "/api/media/media_x", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous media get = %d", status)
	}

	token := login(t, ts)
	status, data := doJSON(t, ts, http.MethodPost, "/api/search", token, map[string]any{
		"wd": "庆余年", "includeMedia": true,
	})
	if status != http.StatusOK {
		t.Fatalf("search = %d %s", status, data)
	}
	var sr struct {
		List  []map[string]any `json:"list"`
		Media []struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Year        string `json:"year"`
			SourceCount int    `json:"sourceCount"`
			Cover       string `json:"cover"`
		} `json:"media"`
	}
	if err := json.Unmarshal(data, &sr); err != nil {
		t.Fatal(err)
	}
	if len(sr.Media) != 1 || !strings.HasPrefix(sr.Media[0].ID, "media_") || sr.Media[0].SourceCount != 1 {
		t.Fatalf("media cards = %+v", sr.Media)
	}
	id := sr.Media[0].ID

	status, data = doJSON(t, ts, http.MethodGet, "/api/media/"+id, token, nil)
	if status != http.StatusOK {
		t.Fatalf("media get = %d %s", status, data)
	}
	var media struct {
		ID       string `json:"id"`
		Metadata struct {
			Title string `json:"title"`
			Year  string `json:"year"`
		} `json:"metadata"`
		Cover struct {
			LocalPath string `json:"localPath"`
		} `json:"cover"`
	}
	if err := json.Unmarshal(data, &media); err != nil {
		t.Fatal(err)
	}
	if media.ID != id || media.Metadata.Title != "庆余年" || media.Metadata.Year != "2019" {
		t.Fatalf("media = %+v", media)
	}

	// 还没有本地封面：匿名访问 404。
	status, _ = doJSON(t, ts, http.MethodGet, "/api/media/"+id+"/cover", "", nil)
	if status != http.StatusNotFound {
		t.Fatalf("cover before cache = %d", status)
	}

	// 人工设置封面（fake Doer 返回 PNG）。
	status, data = doJSON(t, ts, http.MethodPost, "/api/media/"+id+"/cover", token, map[string]any{
		"url": "https://img.test/poster.jpg",
	})
	if status != http.StatusOK {
		t.Fatalf("set cover = %d %s", status, data)
	}
	res, err := http.Get(ts.URL + "/api/media/" + id + "/cover")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("cover get = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Fatalf("cover content-type = %q", ct)
	}
}

func TestMediaResolveBatch(t *testing.T) {
	ts, _ := newTestServer(t)
	token := login(t, ts)
	status, data := doJSON(t, ts, http.MethodPost, "/api/media/resolve", token, map[string]any{
		"items": []map[string]any{
			{"sourceKey": "a", "sourceName": "源A", "vodId": "1", "name": "庆余年", "typeName": "剧集", "year": "2019", "pic": "https://img.test/1.jpg"},
			{"sourceKey": "b", "sourceName": "源B", "vodId": "2", "name": "庆余年 (2019)", "typeName": "剧集", "year": "2019", "pic": "https://img.test/2.jpg"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("resolve = %d %s", status, data)
	}
	var out struct {
		Items []struct {
			ID          string `json:"id"`
			SourceCount int    `json:"sourceCount"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0].SourceCount != 2 {
		t.Fatalf("items = %+v", out.Items)
	}

	status, data = doJSON(t, ts, http.MethodGet, "/api/media/"+out.Items[0].ID+"/metadata-candidates", token, nil)
	if status != http.StatusOK {
		t.Fatalf("candidates = %d %s", status, data)
	}
	if !strings.Contains(string(data), `"items":[]`) && !strings.Contains(string(data), `"items":null`) {
		t.Fatalf("candidates body = %s", data)
	}
}

func TestMediaResolveLookupOnly(t *testing.T) {
	ts, _ := newTestServer(t)
	token := login(t, ts)
	// 先通过搜索建媒体。
	status, _ := doJSON(t, ts, http.MethodPost, "/api/search", token, map[string]any{
		"wd": "庆余年", "includeMedia": true,
	})
	if status != http.StatusOK {
		t.Fatalf("search = %d", status)
	}
	// 只带 sourceKey+vodId：应返回已聚合卡片，而不是 404/空。
	status, data := doJSON(t, ts, http.MethodPost, "/api/media/resolve", token, map[string]any{
		"items": []map[string]any{{"sourceKey": "a", "vodId": "1"}},
	})
	if status != http.StatusOK {
		t.Fatalf("lookup resolve = %d %s", status, data)
	}
	var out struct {
		Items []struct {
			ID          string `json:"id"`
			SourceCount int    `json:"sourceCount"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || !strings.HasPrefix(out.Items[0].ID, "media_") {
		t.Fatalf("items = %+v", out.Items)
	}
}
