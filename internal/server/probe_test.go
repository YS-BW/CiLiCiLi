package server_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestSourceProbeStatusAndConfig(t *testing.T) {
	ts, _ := newTestServer(t)
	token := login(t, ts)

	status, data := doJSON(t, ts, http.MethodGet, "/api/sources/probe", token, nil)
	if status != http.StatusOK {
		t.Fatalf("probe status = %d %s", status, data)
	}
	var st struct {
		IntervalHours int  `json:"intervalHours"`
		Running       bool `json:"running"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.IntervalHours != 12 {
		t.Fatalf("default interval = %d", st.IntervalHours)
	}

	// 修改间隔
	status, data = doJSON(t, ts, http.MethodPost, "/api/sources/probe", token, map[string]any{"intervalHours": 6})
	if status != http.StatusOK {
		t.Fatalf("set interval = %d %s", status, data)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.IntervalHours != 6 {
		t.Fatalf("interval after set = %d", st.IntervalHours)
	}
	// 非法间隔
	if status, _ := doJSON(t, ts, http.MethodPost, "/api/sources/probe", token, map[string]any{"intervalHours": 0}); status != http.StatusBadRequest {
		t.Fatalf("invalid interval = %d", status)
	}

	// 立即探活（异步），轮询等待结果
	status, _ = doJSON(t, ts, http.MethodPost, "/api/sources/probe", token, map[string]any{"run": true})
	if status != http.StatusOK {
		t.Fatalf("trigger probe = %d", status)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, data = doJSON(t, ts, http.MethodGet, "/api/sources/probe", token, nil)
		var full struct {
			Last *struct {
				Total int `json:"total"`
				Alive int `json:"alive"`
			} `json:"last"`
		}
		if json.Unmarshal(data, &full) == nil && full.Last != nil && full.Last.Total > 0 {
			if full.Last.Alive == 0 {
				t.Fatalf("probe last = %s", data)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("probe did not finish: %s", data)
}
