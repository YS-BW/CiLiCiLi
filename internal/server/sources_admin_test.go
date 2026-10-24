package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSourceAdminRequiresAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	status, _ := doJSON(t, ts, http.MethodPost, "/api/sources", "", map[string]any{
		"name": "x", "url": "https://x.test/api",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous create = %d", status)
	}
}

func TestSourceAdminCRUD(t *testing.T) {
	ts, _ := newTestServer(t)
	token := login(t, ts)

	// 新增
	status, data := doJSON(t, ts, http.MethodPost, "/api/sources", token, map[string]any{
		"name": "新增源", "url": "https://new.test/api.php/provide/vod/",
	})
	if status != http.StatusOK {
		t.Fatalf("create = %d %s", status, data)
	}
	var created struct {
		Source struct {
			Key     string `json:"key"`
			Name    string `json:"name"`
			URL     string `json:"url"`
			Enabled bool   `json:"enabled"`
		} `json:"source"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	if created.Source.Key == "" || created.Source.URL != "https://new.test/api.php/provide/vod" || !created.Source.Enabled {
		t.Fatalf("created = %+v", created)
	}
	key := created.Source.Key

	// 列表包含且带 enabled 字段
	status, data = doJSON(t, ts, http.MethodGet, "/api/sources", token, nil)
	if status != http.StatusOK {
		t.Fatalf("list = %d", status)
	}
	if !strings.Contains(string(data), key) || !strings.Contains(string(data), `"enabled":true`) {
		t.Fatalf("list body = %s", data)
	}

	// Registry 已热更新（status.defaultSources）
	status, data = doJSON(t, ts, http.MethodGet, "/api/status", token, nil)
	var st struct {
		DefaultSources []struct {
			Key string `json:"key"`
		} `json:"defaultSources"`
	}
	if err := json.Unmarshal(data, &st); err != nil || status != http.StatusOK {
		t.Fatalf("status = %d %v", status, err)
	}
	found := false
	for _, s := range st.DefaultSources {
		if s.Key == key {
			found = true
		}
	}
	if !found {
		t.Fatalf("registry not synced: %+v", st.DefaultSources)
	}

	// 禁用后 Registry 不再包含
	status, data = doJSON(t, ts, http.MethodPut, "/api/sources/"+key, token, map[string]any{"enabled": false})
	if status != http.StatusOK {
		t.Fatalf("disable = %d %s", status, data)
	}
	_, data = doJSON(t, ts, http.MethodGet, "/api/status", token, nil)
	if strings.Contains(string(data), key) {
		t.Fatalf("disabled source still in registry: %s", data)
	}

	// 无效 URL / 冲突
	if status, _ = doJSON(t, ts, http.MethodPost, "/api/sources", token, map[string]any{
		"name": "坏", "url": "ftp://x",
	}); status != http.StatusBadRequest {
		t.Fatalf("invalid url = %d", status)
	}
	if status, _ = doJSON(t, ts, http.MethodPost, "/api/sources", token, map[string]any{
		"name": "重复", "url": "https://new.test/api.php/provide/vod",
	}); status != http.StatusConflict {
		t.Fatalf("duplicate = %d", status)
	}

	// 删除
	status, _ = doJSON(t, ts, http.MethodDelete, "/api/sources/"+key, token, nil)
	if status != http.StatusOK {
		t.Fatalf("delete = %d", status)
	}
	if status, _ = doJSON(t, ts, http.MethodDelete, "/api/sources/"+key, token, nil); status != http.StatusNotFound {
		t.Fatalf("delete again = %d", status)
	}
}

func TestSourceReorder(t *testing.T) {
	ts, _ := newTestServer(t)
	token := login(t, ts)
	keys := []string{}
	for _, name := range []string{"甲", "乙"} {
		status, data := doJSON(t, ts, http.MethodPost, "/api/sources", token, map[string]any{
			"name": name, "url": "https://" + name + ".test/api",
		})
		if status != http.StatusOK {
			t.Fatalf("create %s = %d", name, status)
		}
		var out struct {
			Source struct {
				Key string `json:"key"`
			} `json:"source"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, out.Source.Key)
	}
	status, data := doJSON(t, ts, http.MethodPost, "/api/sources/reorder", token, map[string]any{
		"keys": []string{keys[1], keys[0]},
	})
	if status != http.StatusOK {
		t.Fatalf("reorder = %d %s", status, data)
	}
	var out struct {
		Sources []struct {
			Key       string `json:"key"`
			SortOrder int    `json:"sortOrder"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Sources[0].Key != keys[1] {
		t.Fatalf("first = %+v", out.Sources)
	}
}

func TestSubscriptionRefreshAndApply(t *testing.T) {
	ts, _ := newTestServer(t)
	token := login(t, ts)

	status, data := doJSON(t, ts, http.MethodPost, "/api/subscriptions", token, map[string]any{
		"name": "测试订阅", "url": "https://sub.test/sources.json",
	})
	if status != http.StatusOK {
		t.Fatalf("create sub = %d %s", status, data)
	}
	var created struct {
		Subscription struct {
			ID int64 `json:"id"`
		} `json:"subscription"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	if created.Subscription.ID == 0 {
		t.Fatalf("sub = %s", data)
	}
	id := created.Subscription.ID

	// 刷新拿到预览
	status, data = doJSON(t, ts, http.MethodPost, "/api/subscriptions/"+itoa(id)+"/refresh", token, nil)
	if status != http.StatusOK {
		t.Fatalf("refresh = %d %s", status, data)
	}
	if !strings.Contains(string(data), "订阅源A") || !strings.Contains(string(data), "订阅源B") {
		t.Fatalf("preview = %s", data)
	}

	// 应用：新增 2 个
	status, data = doJSON(t, ts, http.MethodPost, "/api/subscriptions/"+itoa(id)+"/apply", token, nil)
	if status != http.StatusOK {
		t.Fatalf("apply = %d %s", status, data)
	}
	var applied struct {
		Added   int `json:"added"`
		Skipped int `json:"skipped"`
	}
	if err := json.Unmarshal(data, &applied); err != nil {
		t.Fatal(err)
	}
	if applied.Added != 2 {
		t.Fatalf("applied = %s", data)
	}

	// 再应用一次：全部跳过
	status, data = doJSON(t, ts, http.MethodPost, "/api/subscriptions/"+itoa(id)+"/apply", token, nil)
	if status != http.StatusOK {
		t.Fatalf("apply again = %d", status)
	}
	if err := json.Unmarshal(data, &applied); err != nil {
		t.Fatal(err)
	}
	if applied.Added != 0 || applied.Skipped != 2 {
		t.Fatalf("second apply = %s", data)
	}

	// Registry 同步了订阅源
	_, data = doJSON(t, ts, http.MethodGet, "/api/status", token, nil)
	if !strings.Contains(string(data), "sub-a.test") {
		t.Fatalf("registry not synced: %s", data)
	}

	// 删除订阅
	if status, _ = doJSON(t, ts, http.MethodDelete, "/api/subscriptions/"+itoa(id), token, nil); status != http.StatusOK {
		t.Fatalf("delete sub = %d", status)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
