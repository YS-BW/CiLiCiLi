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
	Name        string `json:"name"`
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
	handler.DisableRateLimit()

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

func TestEpisodeMetaVisibleToGuestAndJoinResponse(t *testing.T) {
	hub := NewHubWithIdle(time.Hour)
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

	writeJSON(t, ctx, hostConn, map[string]any{
		"type":       "cmd",
		"op":         "episode",
		"episode":    1,
		"url":        "https://cdn.example.com/ep2.m3u8",
		"positionMs": 12000,
		"title":      "示例剧集",
		"sourceKey":  "src-a",
		"vodId":      "vod-1",
		"sourceName": "线路甲",
		"episodes": []map[string]string{
			{"name": "第1集", "url": "https://cdn.example.com/ep1.m3u8"},
			{"name": "第2集", "url": "https://cdn.example.com/ep2.m3u8"},
		},
	})

	hostState := readState(t, ctx, hostConn)
	guestState := readState(t, ctx, guestConn)
	for name, state := range map[string]State{"host": hostState, "guest": guestState} {
		if state.Title != "示例剧集" || state.SourceKey != "src-a" || state.VodID != "vod-1" || state.SourceName != "线路甲" {
			t.Fatalf("%s meta state = %+v", name, state)
		}
		if state.Episode != 1 || state.URL != "https://cdn.example.com/ep2.m3u8" || state.PositionMs != 12000 {
			t.Fatalf("%s episode state = %+v", name, state)
		}
		if len(state.Episodes) != 2 || state.Episodes[1].Name != "第2集" || state.Episodes[1].URL != "https://cdn.example.com/ep2.m3u8" {
			t.Fatalf("%s episodes = %+v", name, state.Episodes)
		}
	}

	late := joinRoom(t, ts.URL, host.RoomID, "后来的人")
	if late.State.Title != "示例剧集" || late.State.SourceKey != "src-a" || late.State.VodID != "vod-1" {
		t.Fatalf("join response meta = %+v", late.State)
	}
	if late.State.SourceName != "线路甲" || late.State.Episode != 1 || late.State.URL != "https://cdn.example.com/ep2.m3u8" {
		t.Fatalf("join response episode = %+v", late.State)
	}
	if len(late.State.Episodes) != 2 || late.State.Episodes[0].Name != "第1集" {
		t.Fatalf("join response episodes = %+v", late.State.Episodes)
	}

	// 后续不带 meta 的选集命令应保留已存储的元信息。
	writeJSON(t, ctx, hostConn, map[string]any{
		"type":    "cmd",
		"op":      "episode",
		"episode": 0,
		"url":     "https://cdn.example.com/ep1.m3u8",
	})
	hostState = readState(t, ctx, hostConn)
	if hostState.Title != "示例剧集" || hostState.SourceKey != "src-a" || len(hostState.Episodes) != 2 {
		t.Fatalf("meta should be preserved: %+v", hostState)
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

func TestSanitizeName(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"  张三 \t 李四\n", "张三 李四"},
		{"a\x00b\x01c", "abc"},
		{strings.Repeat("好", 20), strings.Repeat("好", 16)},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := sanitizeName(tc.raw); got != tc.want {
			t.Fatalf("sanitizeName(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestCreateAndJoinRejectEmptyName(t *testing.T) {
	hub := NewHubWithIdle(time.Hour)
	t.Cleanup(hub.Close)
	ts := newTestServer(t, hub)

	status, body := postStatus(t, ts.URL+"/api/watch/rooms", map[string]any{"name": "   "})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "请输入昵称") {
		t.Fatalf("empty create = %d %s", status, body)
	}

	host := createRoom(t, ts.URL, "房主")
	status, body = postStatus(t, ts.URL+"/api/watch/rooms/"+url.PathEscape(host.RoomID)+"/join", map[string]any{"name": "\x00\t "})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "请输入昵称") {
		t.Fatalf("empty join = %d %s", status, body)
	}
}

func TestNicknameDedupeCaseInsensitive(t *testing.T) {
	hub := NewHubWithIdle(time.Hour)
	t.Cleanup(hub.Close)
	ts := newTestServer(t, hub)

	host := createRoom(t, ts.URL, "Alice")
	if host.Name != "Alice" {
		t.Fatalf("host name = %q", host.Name)
	}
	guest1 := joinRoom(t, ts.URL, host.RoomID, "alice")
	if guest1.Name != "alice#2" {
		t.Fatalf("guest1 name = %q, want alice#2", guest1.Name)
	}
	guest2 := joinRoom(t, ts.URL, host.RoomID, "ALICE")
	if guest2.Name != "ALICE#3" {
		t.Fatalf("guest2 name = %q, want ALICE#3", guest2.Name)
	}

	room, ok := hub.Get(host.RoomID)
	if !ok {
		t.Fatal("room missing")
	}
	members := room.Members()
	if len(members) != 3 {
		t.Fatalf("members = %+v", members)
	}
	want := []string{"Alice", "alice#2", "ALICE#3"}
	for i, name := range want {
		if members[i].Name != name || members[i].ID == "" {
			t.Fatalf("member[%d] = %+v, want name %q", i, members[i], name)
		}
	}
	if !members[0].Host || members[1].Host || members[2].Host {
		t.Fatalf("host order/flag wrong: %+v", members)
	}
	if members[1].JoinedAt == 0 || members[2].JoinedAt == 0 {
		t.Fatalf("joinedAt missing: %+v", members)
	}
}

func TestNicknameDedupeRespectsLimit(t *testing.T) {
	hub := NewHubWithIdle(time.Hour)
	t.Cleanup(hub.Close)
	ts := newTestServer(t, hub)

	base := strings.Repeat("a", 16)
	host := createRoom(t, ts.URL, base)
	guest1 := joinRoom(t, ts.URL, host.RoomID, base)
	guest2 := joinRoom(t, ts.URL, host.RoomID, base)
	want1 := strings.Repeat("a", 14) + "#2"
	want2 := strings.Repeat("a", 14) + "#3"
	if guest1.Name != want1 || guest2.Name != want2 {
		t.Fatalf("dedupe names = %q, %q; want %q, %q", guest1.Name, guest2.Name, want1, want2)
	}
	for _, name := range []string{guest1.Name, guest2.Name} {
		if len([]rune(name)) != maxNicknameRunes {
			t.Fatalf("name %q length = %d", name, len([]rune(name)))
		}
	}
}

func TestGuestLeaveBroadcastsMembersToRemainingGuest(t *testing.T) {
	hub := NewHubWithOptions(time.Hour, time.Hour)
	t.Cleanup(hub.Close)
	ts := newTestServer(t, hub)

	host := createRoom(t, ts.URL, "房主")
	guest1 := joinRoom(t, ts.URL, host.RoomID, "观众一")
	guest2 := joinRoom(t, ts.URL, host.RoomID, "观众二")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	guest1Conn := dial(t, ctx, ts.URL, guest1)
	defer guest1Conn.Close(websocket.StatusNormalClosure, "")
	readUntilType(t, ctx, guest1Conn, "state")
	readUntilType(t, ctx, guest1Conn, "members")

	guest2Conn := dial(t, ctx, ts.URL, guest2)
	defer guest2Conn.Close(websocket.StatusNormalClosure, "")
	readUntilType(t, ctx, guest2Conn, "state")
	readUntilType(t, ctx, guest2Conn, "members")

	members := readMembersUntil(t, ctx, guest1Conn, func(list []Member) bool {
		for _, member := range list {
			if member.ID == guest2.MemberID && member.Online {
				return true
			}
		}
		return false
	})
	if len(members) != 3 {
		t.Fatalf("members after guest2 connect = %+v", members)
	}

	leaveRoom(t, ts.URL, guest2)

	members = readMembersWithout(t, ctx, guest1Conn, guest2.MemberID)
	if len(members) != 2 {
		t.Fatalf("members after guest2 leave = %+v", members)
	}
	for _, member := range members {
		if member.ID == guest2.MemberID {
			t.Fatalf("guest2 still present: %+v", members)
		}
	}
}

func TestGuestDisconnectBroadcastsOfflineToOthers(t *testing.T) {
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

	guestConn := dial(t, ctx, ts.URL, guest)
	defer guestConn.Close(websocket.StatusNormalClosure, "")
	readUntilType(t, ctx, guestConn, "state")
	readUntilType(t, ctx, guestConn, "members")
	readMembersUntil(t, ctx, hostConn, func(list []Member) bool {
		for _, member := range list {
			if member.ID == guest.MemberID && member.Online {
				return true
			}
		}
		return false
	})

	guestConn.Close(websocket.StatusNormalClosure, "")

	members := readMembersUntil(t, ctx, hostConn, func(list []Member) bool {
		for _, member := range list {
			if member.ID == guest.MemberID {
				return !member.Online
			}
		}
		return false
	})
	if len(members) != 2 {
		t.Fatalf("members after guest disconnect = %+v", members)
	}
}

func TestRateLimitJoin(t *testing.T) {
	hub := NewHubWithIdle(time.Hour)
	t.Cleanup(hub.Close)
	handler := NewHandler(hub)
	handler.SetRateLimit(2, time.Minute)
	t.Cleanup(handler.DisableRateLimit)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/watch/rooms", handler.Create)
	mux.HandleFunc("POST /api/watch/rooms/{id}/join", handler.Join)
	mux.HandleFunc("POST /api/watch/rooms/{id}/leave", handler.Leave)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	host := createRoom(t, ts.URL, "房主")
	join := ts.URL + "/api/watch/rooms/" + url.PathEscape(host.RoomID) + "/join"
	if status, body := postStatus(t, join, map[string]any{"name": "观众一"}); status != http.StatusOK {
		t.Fatalf("join1 = %d %s", status, body)
	}
	if status, body := postStatus(t, join, map[string]any{"name": "观众二"}); status != http.StatusOK {
		t.Fatalf("join2 = %d %s", status, body)
	}
	status, body := postStatus(t, join, map[string]any{"name": "观众三"})
	if status != http.StatusTooManyRequests || !strings.Contains(string(body), "操作太频繁") {
		t.Fatalf("join3 = %d %s, want 429", status, body)
	}
}

func postStatus(t *testing.T, endpoint string, body any) (int, []byte) {
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
	return res.StatusCode, data
}

func newTestServer(t *testing.T, hub *Hub) *httptest.Server {
	t.Helper()
	handler := NewHandler(hub)
	handler.DisableRateLimit()
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

func readMembersUntil(t *testing.T, ctx context.Context, conn *websocket.Conn, pred func([]Member) bool) []Member {
	t.Helper()
	for {
		var msg wsMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			t.Fatalf("read members: %v", err)
		}
		if msg.Type == "members" && pred(msg.Members) {
			return msg.Members
		}
	}
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
