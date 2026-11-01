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
	Reason   string   `json:"reason"`
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

func TestHostLeaveDissolvesRoom(t *testing.T) {
	hub := NewHubWithOptions(time.Hour, time.Hour)
	t.Cleanup(hub.Close)
	ts := newTestServer(t, hub)

	host := createRoom(t, ts.URL, "房主")
	guest := joinRoom(t, ts.URL, host.RoomID, "观众")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	guestConn := dial(t, ctx, ts.URL, guest)
	defer guestConn.Close(websocket.StatusNormalClosure, "")
	readUntilType(t, ctx, guestConn, "state")
	readUntilType(t, ctx, guestConn, "members")

	leaveRoom(t, ts.URL, host)

	msg := readUntilType(t, ctx, guestConn, "closed")
	if msg.Reason != "host_left" {
		t.Fatalf("closed reason = %q, want host_left", msg.Reason)
	}
	if status := joinStatus(t, ts.URL, host.RoomID, "late"); status != http.StatusNotFound {
		t.Fatalf("join after dissolve = %d, want 404", status)
	}
}

func TestGuestLeaveRemovesMemberAndInvalidatesToken(t *testing.T) {
	hub := NewHubWithOptions(time.Hour, time.Hour)
	t.Cleanup(hub.Close)
	ts := newTestServer(t, hub)

	host := createRoom(t, ts.URL, "房主")
	guest := joinRoom(t, ts.URL, host.RoomID, "观众")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hostConn := dial(t, ctx, ts.URL, host)
	defer hostConn.Close(websocket.StatusNormalClosure, "")
	readUntilType(t, ctx, hostConn, "state")
	readUntilType(t, ctx, hostConn, "members")

	leaveRoom(t, ts.URL, guest)

	members := readMembersWithout(t, ctx, hostConn, guest.MemberID)
	if len(members) != 1 || members[0].ID != host.MemberID {
		t.Fatalf("members after guest leave = %+v", members)
	}

	room, ok := hub.Get(host.RoomID)
	if !ok {
		t.Fatal("room should still exist after guest leave")
	}
	if _, ok := room.Authenticate(guest.MemberID, guest.MemberToken); ok {
		t.Fatal("guest token should be invalid after leaving")
	}
	if status := wsStatus(t, ctx, ts.URL, guest); status != http.StatusUnauthorized {
		t.Fatalf("guest ws after leave = %d, want 401", status)
	}
}

func TestHostDisconnectGraceDissolves(t *testing.T) {
	hub := NewHubWithOptions(time.Hour, 100*time.Millisecond)
	t.Cleanup(hub.Close)
	ts := newTestServer(t, hub)

	host := createRoom(t, ts.URL, "房主")
	guest := joinRoom(t, ts.URL, host.RoomID, "观众")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hostConn := dial(t, ctx, ts.URL, host)
	defer hostConn.Close(websocket.StatusNormalClosure, "")
	readUntilType(t, ctx, hostConn, "state")
	readUntilType(t, ctx, hostConn, "members")

	guestConn := dial(t, ctx, ts.URL, guest)
	defer guestConn.Close(websocket.StatusNormalClosure, "")
	readUntilType(t, ctx, guestConn, "state")
	readUntilType(t, ctx, guestConn, "members")

	hostConn.Close(websocket.StatusNormalClosure, "")

	msg := readUntilType(t, ctx, guestConn, "closed")
	if msg.Reason != "host_disconnected" {
		t.Fatalf("closed reason = %q, want host_disconnected", msg.Reason)
	}
	if status := joinStatus(t, ts.URL, host.RoomID, "late"); status != http.StatusNotFound {
		t.Fatalf("join after grace = %d, want 404", status)
	}
}

func TestHostReconnectWithinGraceKeepsRoom(t *testing.T) {
	grace := 250 * time.Millisecond
	hub := NewHubWithOptions(time.Hour, grace)
	t.Cleanup(hub.Close)
	ts := newTestServer(t, hub)

	host := createRoom(t, ts.URL, "房主")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first := dial(t, ctx, ts.URL, host)
	readUntilType(t, ctx, first, "state")
	readUntilType(t, ctx, first, "members")
	first.Close(websocket.StatusNormalClosure, "")

	second := dial(t, ctx, ts.URL, host)
	defer second.Close(websocket.StatusNormalClosure, "")
	readUntilType(t, ctx, second, "state")

	time.Sleep(grace * 2)
	if status := joinStatus(t, ts.URL, host.RoomID, "late"); status != http.StatusOK {
		t.Fatalf("join after reconnect = %d, want 200", status)
	}
}

func newTestServer(t *testing.T, hub *Hub) *httptest.Server {
	t.Helper()
	handler := NewHandler(hub)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/watch/rooms", handler.Create)
	mux.HandleFunc("POST /api/watch/rooms/{id}/join", handler.Join)
	mux.HandleFunc("POST /api/watch/rooms/{id}/leave", handler.Leave)
	mux.HandleFunc("GET /api/watch/rooms/{id}/ws", handler.WS)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func leaveRoom(t *testing.T, baseURL string, c credentials) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"memberId": c.MemberID, "token": c.MemberToken})
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(baseURL+"/api/watch/rooms/"+url.PathEscape(c.RoomID)+"/leave", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("leave = %d %s", res.StatusCode, data)
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(data, &out); err != nil || !out.OK {
		t.Fatalf("leave response = %s (%v)", data, err)
	}
}

func joinStatus(t *testing.T, baseURL, roomID, name string) int {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(baseURL+"/api/watch/rooms/"+url.PathEscape(roomID)+"/join", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

func wsStatus(t *testing.T, ctx context.Context, baseURL string, c credentials) int {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/api/watch/rooms/" +
		url.PathEscape(c.RoomID) + "/ws?memberId=" + url.QueryEscape(c.MemberID) +
		"&token=" + url.QueryEscape(c.MemberToken)
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		return http.StatusOK
	}
	if resp == nil {
		t.Fatalf("dial: %v", err)
	}
	return resp.StatusCode
}

func readMembersWithout(t *testing.T, ctx context.Context, conn *websocket.Conn, memberID string) []Member {
	t.Helper()
	for {
		var msg wsMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			t.Fatalf("read members: %v", err)
		}
		if msg.Type != "members" {
			continue
		}
		found := false
		for _, m := range msg.Members {
			if m.ID == memberID {
				found = true
				break
			}
		}
		if !found {
			return msg.Members
		}
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
