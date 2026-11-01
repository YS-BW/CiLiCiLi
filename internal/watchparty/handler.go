package watchparty

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"cilicili/internal/httpx"
)

// Handler 提供一起看的 HTTP / WebSocket 接口。
type Handler struct {
	hub *Hub
}

// NewHandler 创建 Handler。hub 为空时使用默认 Hub。
func NewHandler(hub *Hub) *Handler {
	if hub == nil {
		hub = NewHub()
	}
	return &Handler{hub: hub}
}

// New 是 NewHandler 的简写。
func New(hub *Hub) *Handler { return NewHandler(hub) }

// Hub 返回底层 Hub。
func (h *Handler) Hub() *Hub { return h.hub }

type credentialsResponse struct {
	RoomID      string `json:"roomId"`
	MemberID    string `json:"memberId"`
	MemberToken string `json:"memberToken"`
	State       State  `json:"state"`
}

type clientMessage struct {
	Type       string `json:"type"`
	ClientTs   int64  `json:"clientTs,omitempty"`
	Op         string `json:"op,omitempty"`
	PositionMs int64  `json:"positionMs,omitempty"`
	Episode    int    `json:"episode,omitempty"`
	URL        string `json:"url,omitempty"`
	State      bool   `json:"state,omitempty"`
}

type stateMessage struct {
	Type  string `json:"type"`
	State State  `json:"state"`
}

type membersMessage struct {
	Type    string   `json:"type"`
	Members []Member `json:"members"`
}

type pongMessage struct {
	Type     string `json:"type"`
	ClientTs int64  `json:"clientTs"`
	ServerTs int64  `json:"serverTs"`
}

type errorMessage struct {
	Type string `json:"type"`
	Msg  string `json:"msg"`
}

// Create 处理 POST /api/watch/rooms。
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	name, err := decodeName(r, "房主")
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	room, member, token := h.hub.Create(name)
	if room == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "服务暂不可用")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, credentialsResponse{
		RoomID:      room.ID,
		MemberID:    member.ID,
		MemberToken: token,
		State:       room.State(),
	})
}

// Join 处理 POST /api/watch/rooms/{id}/join。
func (h *Handler) Join(w http.ResponseWriter, r *http.Request) {
	name, err := decodeName(r, "观众")
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	room, member, token, err := h.hub.Join(r.PathValue("id"), name)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "房间不存在或已过期")
		return
	}
	room.Broadcast(membersMessage{Type: "members", Members: room.Members()})
	httpx.WriteJSON(w, http.StatusOK, credentialsResponse{
		RoomID:      room.ID,
		MemberID:    member.ID,
		MemberToken: token,
		State:       room.State(),
	})
}

// WS 处理 GET /api/watch/rooms/{id}/ws?memberId=&token=。
func (h *Handler) WS(w http.ResponseWriter, r *http.Request) {
	room, ok := h.hub.Get(r.PathValue("id"))
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "房间不存在或已过期")
		return
	}
	memberID := r.URL.Query().Get("memberId")
	if _, ok := room.Authenticate(memberID, r.URL.Query().Get("token")); !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "成员凭证无效")
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c := newClient(memberID, conn)
	state, ok := room.addClient(c)
	if !ok {
		_ = conn.Close(websocket.StatusPolicyViolation, "member not found")
		return
	}

	go c.writeLoop()
	c.enqueueJSON(stateMessage{Type: "state", State: state})
	room.Broadcast(membersMessage{Type: "members", Members: room.Members()})

	defer func() {
		room.removeClient(c)
		c.close()
		room.Broadcast(membersMessage{Type: "members", Members: room.Members()})
	}()

	for {
		var msg clientMessage
		if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
			return
		}
		room.Touch()

		switch msg.Type {
		case "ping":
			c.enqueueJSON(pongMessage{
				Type:     "pong",
				ClientTs: msg.ClientTs,
				ServerTs: time.Now().UnixMilli(),
			})
		case "cmd":
			state, err := room.Apply(Command{
				MemberID:   memberID,
				Op:         msg.Op,
				PositionMs: msg.PositionMs,
				Episode:    msg.Episode,
				URL:        msg.URL,
			})
			if err != nil {
				c.enqueueJSON(errorMessage{Type: "error", Msg: err.Error()})
				continue
			}
			room.Broadcast(stateMessage{Type: "state", State: state})
		case "buffering":
			room.setBuffering(memberID, msg.State)
		default:
			c.enqueueJSON(errorMessage{Type: "error", Msg: "未知消息类型"})
		}
	}
}

func decodeName(r *http.Request, fallback string) (string, error) {
	if r.Body == nil {
		return fallback, nil
	}
	var body struct {
		Name string `json:"name"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, httpx.MaxJSONBody))
	if err := dec.Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return fallback, nil
		}
		return "", err
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = fallback
	}
	return name, nil
}
