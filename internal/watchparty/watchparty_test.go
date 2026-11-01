package watchparty

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type credentials struct {
	RoomID      string `json:"roomId"`
	MemberID    string `json:"memberId"`
	MemberToken string `json:"memberToken"`
	State       State  `json:"state"`
}

type wsMessage struct {
	Type     string   `json:"type"`
	ClientTs int64    `json:"clientTs"`
	ServerTs int64    `json:"serverTs"`
	State    *State   `json:"state"`
	Members  []Member `json:"members"`
	Msg      string   `json:"msg"`
}

func TestWatchPartyTwoClients(t *testing.T) {
	hub := NewHubWithIdle(time.Hour)
	t.Cleanup(hub.Close)
	handler := NewHandler(hub)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/watch/rooms", handler.Create)
	mux.HandleFunc("POST /api/watch/rooms/{id}/join", handler.Join)
	mux.HandleFunc("GET /api/watch/rooms/{id}/ws", handler.WS)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	host := createRoom(t, ts.URL, "房主")
	guest := joinRoom(t, ts.URL, host.RoomID, "观众")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hostConn := dial(t, ctx, ts.URL, host)
	defer hostConn.Close(websocket.StatusNormalClosure, "")
	if msg := readUntilType(t, ctx, hostConn, "state"); msg.State == nil || msg.State.Seq != 0 {
		t.Fatalf("host initial state = %+v", msg.State)
	}
	readUntilType(t, ctx, hostConn, "members")

	guestConn := dial(t, ctx, ts.URL, guest)
	defer guestConn.Close(websocket.StatusNormalClosure, "")
	if msg := readUntilType(t, ctx, guestConn, "state"); msg.State == nil || msg.State.Seq != 0 {
		t.Fatalf("guest initial state = %+v", msg.State)
	}
	readUntilType(t, ctx, guestConn, "members")

	writeJSON(t, ctx, hostConn, map[string]any{"type": "cmd", "op": "play"})
	hostState := readState(t, ctx, hostConn)
	guestState := readState(t, ctx, guestConn)
	if hostState.Seq != 1 || guestState.Seq != 1 {
		t.Fatalf("after play seq host=%d guest=%d", hostState.Seq, guestState.Seq)
	}
	if hostState.Paused || guestState.Paused {
		t.Fatalf("after play paused host=%v guest=%v", hostState.Paused, guestState.Paused)
	}

	writeJSON(t, ctx, hostConn, map[string]any{"type": "cmd", "op": "seek", "positionMs": 90000})
	hostState = readState(t, ctx, hostConn)
	guestState = readState(t, ctx, guestConn)
	if hostState.Seq != 2 || guestState.Seq != 2 || guestState.PositionMs != 90000 {
		t.Fatalf("after seek host=%+v guest=%+v", hostState, guestState)
	}

	writeJSON(t, ctx, hostConn, map[string]any{"type": "cmd", "op": "pause"})
	hostState = readState(t, ctx, hostConn)
	guestState = readState(t, ctx, guestConn)
	if hostState.Seq != 3 || guestState.Seq != 3 {
		t.Fatalf("after pause seq host=%d guest=%d", hostState.Seq, guestState.Seq)
	}
	if !hostState.Paused || !guestState.Paused || guestState.PositionMs < 90000 {
		t.Fatalf("after pause host=%+v guest=%+v", hostState, guestState)
	}

	writeJSON(t, ctx, guestConn, map[string]any{"type": "cmd", "op": "play"})
	if msg := readUntilType(t, ctx, guestConn, "error"); msg.Msg == "" {
		t.Fatalf("guest command should be rejected, got %+v", msg)
	}

	writeJSON(t, ctx, guestConn, map[string]any{"type": "ping", "clientTs": 42})
	if msg := readUntilType(t, ctx, guestConn, "pong"); msg.ClientTs != 42 || msg.ServerTs == 0 {
		t.Fatalf("pong = %+v", msg)
	}
}

func createRoom(t *testing.T, baseURL, name string) credentials {
	t.Helper()
	return postCredentials(t, baseURL+"/api/watch/rooms", map[string]any{"name": name})
}

func joinRoom(t *testing.T, baseURL, roomID, name string) credentials {
	t.Helper()
	return postCredentials(t, baseURL+"/api/watch/rooms/"+url.PathEscape(roomID)+"/join", map[string]any{"name": name})
}

func postCredentials(t *testing.T, endpoint string, body any) credentials {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(endpoint, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		t.Fatalf("POST %s = %d %s", endpoint, res.StatusCode, data)
	}
	var out credentials
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode credentials: %v (%s)", err, data)
	}
	if out.RoomID == "" || out.MemberID == "" || out.MemberToken == "" {
		t.Fatalf("incomplete credentials: %+v", out)
	}
	return out
}

func dial(t *testing.T, ctx context.Context, baseURL string, c credentials) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/api/watch/rooms/" +
		url.PathEscape(c.RoomID) + "/ws?memberId=" + url.QueryEscape(c.MemberID) +
		"&token=" + url.QueryEscape(c.MemberToken)
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func writeJSON(t *testing.T, ctx context.Context, conn *websocket.Conn, v any) {
	t.Helper()
	if err := wsjson.Write(ctx, conn, v); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readUntilType(t *testing.T, ctx context.Context, conn *websocket.Conn, typ string) wsMessage {
	t.Helper()
	for {
		var msg wsMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			t.Fatalf("read %s: %v", typ, err)
		}
		if msg.Type == typ {
			return msg
		}
	}
}

func readState(t *testing.T, ctx context.Context, conn *websocket.Conn) State {
	t.Helper()
	msg := readUntilType(t, ctx, conn, "state")
	if msg.State == nil {
		t.Fatal("state message missing state")
	}
	return *msg.State
}
