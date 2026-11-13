package server_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestLatencyEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	token := login(t, ts)

	// 未登录应被守卫拦截。
	if status, _ := doJSON(t, ts, http.MethodPost, "/api/latency", "", map[string]any{
		"items": []map[string]any{{"key": "u1", "url": "https://cdn.test/1.m3u8"}},
	}); status != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d", status)
	}

	status, data := doJSON(t, ts, http.MethodPost, "/api/latency", token, map[string]any{
		"items": []map[string]any{
			{"key": "u1", "url": "https://cdn.test/1.m3u8"},
			{"key": "s1", "sourceKey": "a", "vodId": "1"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("latency status = %d %s", status, data)
	}
	var resp struct {
		Results []struct {
			Key      string `json:"key"`
			OK       bool   `json:"ok"`
			TTFBMs   int64  `json:"ttfbMs"`
			MS       int64  `json:"ms"`
			Status   int    `json:"status"`
			Error    string `json:"error"`
			TimedOut bool   `json:"timedOut"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %d %s", len(resp.Results), data)
	}
	for _, res := range resp.Results {
		if !res.OK || res.Error != "" || res.Status != 200 {
			t.Fatalf("bad result: %+v", res)
		}
		if res.MS < res.TTFBMs {
			t.Fatalf("ms < ttfb: %+v", res)
		}
	}
}

func TestLatencyEndpointRejectsEmpty(t *testing.T) {
	ts, _ := newTestServer(t)
	token := login(t, ts)
	if status, _ := doJSON(t, ts, http.MethodPost, "/api/latency", token, map[string]any{}); status != http.StatusBadRequest {
		t.Fatalf("empty items status = %d", status)
	}
}
