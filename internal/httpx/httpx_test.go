package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, map[string]string{"ok": "1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != "1" {
		t.Fatalf("body = %v", body)
	}
}

func TestReadJSONRejectsUnknownField(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1,"b":2}`))
	var dst struct {
		A int `json:"a"`
	}
	if err := ReadJSON(r, &dst); err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestReadJSONTooLarge(t *testing.T) {
	big := `{"a":"` + strings.Repeat("x", MaxJSONBody) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(big))
	var dst struct {
		A string `json:"a"`
	}
	if err := ReadJSON(r, &dst); err == nil {
		t.Fatal("expected size limit error")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	if got := ClientIP(r); got != "10.0.0.1" {
		t.Fatalf("remote ip = %q", got)
	}
	r.Header.Set("X-Real-IP", "1.2.3.4")
	if got := ClientIP(r); got != "1.2.3.4" {
		t.Fatalf("x-real-ip = %q", got)
	}
}

func TestNDJSONWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	nw := NewNDJSONWriter(rec)
	if err := nw.Write(map[string]any{"type": "source", "ok": true}); err != nil {
		t.Fatal(err)
	}
	if err := nw.Write(map[string]any{"type": "done"}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d: %q", len(lines), rec.Body.String())
	}
	if !strings.Contains(lines[0], `"type":"source"`) {
		t.Fatalf("line0 = %q", lines[0])
	}
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatal("missing X-Accel-Buffering")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/x-ndjson") {
		t.Fatalf("content-type = %q", ct)
	}
}
