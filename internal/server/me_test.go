package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func doJSON(t *testing.T, ts *httptest.Server, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, data
}

func TestMeRequiresAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	status, _ := doJSON(t, ts, http.MethodPut, "/api/me/progress", "", map[string]any{
		"sourceKey": "a", "vodId": "1", "episodeId": "e:第01集", "position": 1,
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d", status)
	}
}

func TestProgressAndHistory(t *testing.T) {
	ts, _ := newTestServer(t)
	token := login(t, ts)

	body := map[string]any{
		"sourceKey": "a", "sourceName": "源A", "vodId": "1", "title": "庆余年", "cover": "https://img.test/1.jpg",
		"lineId": "ln:lineA", "lineName": "lineA", "episodeId": "e:第03集", "episodeName": "第03集", "episodeIndex": 3,
		"position": 120, "duration": 2700, "completed": false,
	}
	status, data := doJSON(t, ts, http.MethodPut, "/api/me/progress", token, body)
	if status != http.StatusOK {
		t.Fatalf("put status = %d body=%s", status, data)
	}
	var saved map[string]any
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["id"] != "a:1" || saved["episodeId"] != "e:第03集" || saved["lineName"] != "lineA" {
		t.Fatalf("saved = %v", saved)
	}
	if ts, ok := saved["updatedAt"].(float64); !ok || ts <= 0 {
		t.Fatalf("updatedAt = %v", saved["updatedAt"])
	}

	status, data = doJSON(t, ts, http.MethodGet, "/api/me/history?unfinished=1", token, nil)
	if status != http.StatusOK {
		t.Fatalf("history status = %d", status)
	}
	var hist struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(data, &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist.Items) != 1 || hist.Items[0]["id"] != "a:1" {
		t.Fatalf("history = %v", hist.Items)
	}

	// 同一影片改成已看完：继续观看里消失，全量历史仍在。
	body["position"] = 2700
	body["completed"] = true
	if status, data := doJSON(t, ts, http.MethodPut, "/api/me/progress", token, body); status != http.StatusOK {
		t.Fatalf("put completed = %d %s", status, data)
	}
	_, data = doJSON(t, ts, http.MethodGet, "/api/me/history?unfinished=1", token, nil)
	if err := json.Unmarshal(data, &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist.Items) != 0 {
		t.Fatalf("unfinished should be empty: %v", hist.Items)
	}
	_, data = doJSON(t, ts, http.MethodGet, "/api/me/history", token, nil)
	if err := json.Unmarshal(data, &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist.Items) != 1 || hist.Items[0]["completed"] != true {
		t.Fatalf("all history = %v", hist.Items)
	}

	status, data = doJSON(t, ts, http.MethodDelete, "/api/me/history/a:1", token, nil)
	if status != http.StatusOK || !bytes.Contains(data, []byte(`"deleted":true`)) {
		t.Fatalf("delete = %d %s", status, data)
	}
	_, data = doJSON(t, ts, http.MethodGet, "/api/me/history", token, nil)
	if err := json.Unmarshal(data, &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist.Items) != 0 {
		t.Fatalf("history after delete = %v", hist.Items)
	}
}

func TestProgressValidation(t *testing.T) {
	ts, _ := newTestServer(t)
	token := login(t, ts)
	cases := []map[string]any{
		{"sourceKey": "a", "vodId": "1", "position": 1},                                        // 缺 episodeId
		{"sourceKey": "", "vodId": "1", "episodeId": "e:第01集", "position": 1},                  // 缺 sourceKey
		{"sourceKey": "a", "vodId": "1", "episodeId": "e:第01集", "position": -1},                // 负位置
		{"sourceKey": "a", "vodId": "1", "episodeId": "e:第01集", "position": 1, "duration": -1}, // 负时长
	}
	for i, body := range cases {
		if status, data := doJSON(t, ts, http.MethodPut, "/api/me/progress", token, body); status != http.StatusBadRequest {
			t.Fatalf("case %d: status = %d body=%s", i, status, data)
		}
	}
}

func TestFavorites(t *testing.T) {
	ts, _ := newTestServer(t)
	token := login(t, ts)

	status, data := doJSON(t, ts, http.MethodPut, "/api/me/favorites", token, map[string]any{
		"sourceKey": "a", "sourceName": "源A", "vodId": "1", "title": "庆余年", "cover": "https://img.test/1.jpg",
	})
	if status != http.StatusOK {
		t.Fatalf("put favorite = %d %s", status, data)
	}
	var fav map[string]any
	if err := json.Unmarshal(data, &fav); err != nil {
		t.Fatal(err)
	}
	if fav["id"] != "a:1" || fav["title"] != "庆余年" {
		t.Fatalf("favorite = %v", fav)
	}

	// 更新快照：标题变化，createdAt 保留。
	firstCreated := fav["createdAt"]
	if status, _ := doJSON(t, ts, http.MethodPut, "/api/me/favorites", token, map[string]any{
		"sourceKey": "a", "vodId": "1", "title": "庆余年 第二季", "cover": "https://img.test/2.jpg",
	}); status != http.StatusOK {
		t.Fatalf("update favorite = %d", status)
	}
	_, data = doJSON(t, ts, http.MethodGet, "/api/me/favorites", token, nil)
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0]["title"] != "庆余年 第二季" {
		t.Fatalf("favorites = %v", list.Items)
	}
	if list.Items[0]["createdAt"] != firstCreated {
		t.Fatalf("createdAt should be preserved: %v vs %v", list.Items[0]["createdAt"], firstCreated)
	}

	status, data = doJSON(t, ts, http.MethodDelete, "/api/me/favorites/a:1", token, nil)
	if status != http.StatusOK || !bytes.Contains(data, []byte(`"deleted":true`)) {
		t.Fatalf("delete favorite = %d %s", status, data)
	}
	_, data = doJSON(t, ts, http.MethodGet, "/api/me/favorites", token, nil)
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("favorites after delete = %v", list.Items)
	}

	if status, _ := doJSON(t, ts, http.MethodPut, "/api/me/favorites", token, map[string]any{"sourceKey": "a"}); status != http.StatusBadRequest {
		t.Fatalf("missing vodId should be 400, got %d", status)
	}
}
