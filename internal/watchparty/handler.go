package watchparty

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"cilicili/internal/httpx"
)

const (
	// DefaultDanmakuRateLimit 是每个成员在默认时间窗口内允许发送的弹幕数。
	DefaultDanmakuRateLimit = 5
	// DefaultDanmakuRateWindow 是弹幕限流的默认时间窗口。
	DefaultDanmakuRateWindow = 2 * time.Second
)

// Handler 提供一起看的 HTTP / WebSocket 接口。
type Handler struct {
	hub            *Hub
	limiter        *rateLimiter
	danmakuLimiter *rateLimiter
}

// NewHandler 创建 Handler。hub 为空时使用默认 Hub。
func NewHandler(hub *Hub) *Handler {
	if hub == nil {
		hub = NewHub()
	}
	return &Handler{
		hub:            hub,
		limiter:        newRateLimiter(DefaultRateLimit, DefaultRateWindow),
		danmakuLimiter: newRateLimiter(DefaultDanmakuRateLimit, DefaultDanmakuRateWindow),
	}
}

// New 是 NewHandler 的简写。
func New(hub *Hub) *Handler { return NewHandler(hub) }

// SetRateLimit 调整 POST 接口的限流参数；limit <= 0 表示禁用限流。
// 建议在开始处理请求前调用。
func (h *Handler) SetRateLimit(limit int, window time.Duration) {
	if h.limiter != nil {
		h.limiter.close()
		h.limiter = nil
	}
	if limit > 0 {
		h.limiter = newRateLimiter(limit, window)
	}
}

// SetDanmakuRateLimit 调整成员弹幕发送的限流参数；limit <= 0 表示禁用限流。
// 建议在开始处理请求前调用。
func (h *Handler) SetDanmakuRateLimit(limit int, window time.Duration) {
	if h.danmakuLimiter != nil {
		h.danmakuLimiter.close()
		h.danmakuLimiter = nil
	}
	if limit > 0 {
		h.danmakuLimiter = newRateLimiter(limit, window)
	}
}

// DisableRateLimit 关闭 HTTP 与弹幕限流，主要用于测试。
func (h *Handler) DisableRateLimit() {
	h.SetRateLimit(0, 0)
	h.SetDanmakuRateLimit(0, 0)
}

// Hub 返回底层 Hub。
func (h *Handler) Hub() *Hub { return h.hub }

type credentialsResponse struct {
	RoomID      string `json:"roomId"`
	MemberID    string `json:"memberId"`
	MemberToken string `json:"memberToken"`
	Name        string `json:"name"`
	State       State  `json:"state"`
}

type clientMessage struct {
	Type       string    `json:"type"`
	ClientTs   int64     `json:"clientTs,omitempty"`
	Op         string    `json:"op,omitempty"`
	PositionMs int64     `json:"positionMs,omitempty"`
	Episode    int       `json:"episode,omitempty"`
	URL        string    `json:"url,omitempty"`
	Title      string    `json:"title,omitempty"`
	SourceKey  string    `json:"sourceKey,omitempty"`
	VodID      string    `json:"vodId,omitempty"`
	SourceName string    `json:"sourceName,omitempty"`
	Episodes   []Episode `json:"episodes,omitempty"`
	State      bool      `json:"state,omitempty"`
	Text       string    `json:"text,omitempty"`
	Color      string    `json:"color,omitempty"`
	Mode       int       `json:"mode,omitempty"`
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

type leaveRequest struct {
	MemberID string `json:"memberId"`
	Token    string `json:"token"`
}

// Create 处理 POST /api/watch/rooms。
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	if !h.allow(r, "create") {
		httpx.WriteError(w, http.StatusTooManyRequests, "操作太频繁，请稍后再试")
		return
	}
	name, err := decodeName(r)
	if err != nil {
		if errors.Is(err, errNameRequired) {
			httpx.WriteError(w, http.StatusBadRequest, "请输入昵称")
			return
		}
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
		Name:        member.Name,
		State:       room.State(),
	})
}

// Join 处理 POST /api/watch/rooms/{id}/join。
func (h *Handler) Join(w http.ResponseWriter, r *http.Request) {
	if !h.allow(r, "join") {
		httpx.WriteError(w, http.StatusTooManyRequests, "操作太频繁，请稍后再试")
		return
	}
	name, err := decodeName(r)
	if err != nil {
		if errors.Is(err, errNameRequired) {
			httpx.WriteError(w, http.StatusBadRequest, "请输入昵称")
			return
		}
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
		Name:        member.Name,
		State:       room.State(),
	})
}

// Leave 处理 POST /api/watch/rooms/{id}/leave。
// 房主离开会解散房间，普通成员离开仅移除自己。
func (h *Handler) Leave(w http.ResponseWriter, r *http.Request) {
	if !h.allow(r, "leave") {
		httpx.WriteError(w, http.StatusTooManyRequests, "操作太频繁，请稍后再试")
		return
	}
	room, ok := h.hub.Get(r.PathValue("id"))
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "房间不存在或已过期")
		return
	}
	var body leaveRequest
	if r.Body != nil {
		dec := json.NewDecoder(io.LimitReader(r.Body, httpx.MaxJSONBody))
		if err := dec.Decode(&body); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
			return
		}
	}
	member, ok := room.Authenticate(body.MemberID, body.Token)
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "成员凭证无效")
		return
	}
	if member.Host {
		h.hub.dissolve(room.ID, "host_left")
	} else if !h.hub.removeGuest(room.ID, member.ID) {
		httpx.WriteError(w, http.StatusNotFound, "房间不存在或已过期")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
	go c.writeLoop()
	state, ok := room.addClient(c)
	if !ok {
		c.close()
		_ = conn.Close(websocket.StatusPolicyViolation, "member not found")
		return
	}
	if room.IsHost(memberID) {
		h.hub.cancelHostGrace(room.ID)
	}

	c.enqueueJSON(stateMessage{Type: "state", State: state})
	room.Broadcast(membersMessage{Type: "members", Members: room.Members()})

	defer func() {
		room.removeClient(c)
		c.close()
		if room.IsHost(memberID) && !room.HostOnline() {
			h.hub.scheduleHostGrace(room.ID)
		}
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
				Title:      msg.Title,
				SourceKey:  msg.SourceKey,
				VodID:      msg.VodID,
				SourceName: msg.SourceName,
				Episodes:   msg.Episodes,
			})
			if err != nil {
				c.enqueueJSON(errorMessage{Type: "error", Msg: err.Error()})
				continue
			}
			room.Broadcast(stateMessage{Type: "state", State: state})
		case "buffering":
			room.setBuffering(memberID, msg.State)
		case "danmaku":
			text, err := sanitizeDanmaku(msg.Text)
			if err != nil {
				c.enqueueJSON(errorMessage{Type: "error", Msg: err.Error()})
				continue
			}
			if !h.allowDanmaku(room.ID, memberID) {
				c.enqueueJSON(errorMessage{Type: "error", Msg: "弹幕发送太频繁"})
				continue
			}
			room.Broadcast(Danmaku{
				Type:  "danmaku",
				Text:  text,
				Color: sanitizeDanmakuColor(msg.Color),
				Mode:  sanitizeDanmakuMode(msg.Mode),
				ID:    newDanmakuID(),
				AtMs:  time.Now().UnixMilli(),
			})
		default:
			c.enqueueJSON(errorMessage{Type: "error", Msg: "未知消息类型"})
		}
	}
}

var errNameRequired = errors.New("请输入昵称")

func decodeName(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", errNameRequired
	}
	var body struct {
		Name string `json:"name"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, httpx.MaxJSONBody))
	if err := dec.Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return "", errNameRequired
		}
		return "", err
	}
	name := sanitizeName(body.Name)
	if name == "" {
		return "", errNameRequired
	}
	return name, nil
}

func (h *Handler) allow(r *http.Request, kind string) bool {
	if h.limiter == nil {
		return true
	}
	return h.limiter.allow(kind + "|" + remoteIP(r))
}

func (h *Handler) allowDanmaku(roomID, memberID string) bool {
	if h.danmakuLimiter == nil {
		return true
	}
	return h.danmakuLimiter.allow(roomID + "|" + memberID)
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
