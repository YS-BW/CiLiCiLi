// Package watchparty 实现「一起看」的服务端权威时间轴与房间管理。
package watchparty

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"
)

var (
	// ErrForbidden 表示非房主尝试提交控制命令。
	ErrForbidden = errors.New("仅房主可以控制播放")
	// ErrUnknownOp 表示未知的控制命令。
	ErrUnknownOp = errors.New("未知控制命令")
)

// State 是房间唯一的权威时间轴状态。
type State struct {
	Seq             int64   `json:"seq"`
	Paused          bool    `json:"paused"`
	PositionMs      int64   `json:"positionMs"`
	ServerTimeMs    int64   `json:"serverTimeMs"`
	Rate            float64 `json:"rate"`
	Episode         int     `json:"episode"`
	URL             string  `json:"url"`
	ContentOffsetMs int64   `json:"contentOffsetMs"`
}

// Current 根据服务端时间推算当前播放位置。
func (s State) Current(now time.Time) int64 {
	if s.Paused {
		return s.PositionMs
	}
	rate := s.Rate
	if rate <= 0 {
		rate = 1
	}
	delta := now.UnixMilli() - s.ServerTimeMs
	if delta < 0 {
		delta = 0
	}
	pos := s.PositionMs + int64(float64(delta)*rate)
	if pos < 0 {
		return 0
	}
	return pos
}

// Member 是房间成员。Token 不参与 JSON 序列化。
type Member struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Host      bool   `json:"host"`
	Online    bool   `json:"online"`
	Buffering bool   `json:"-"`

	token string
}

// Command 是客户端提交的控制命令。
type Command struct {
	MemberID   string `json:"memberId,omitempty"`
	Op         string `json:"op"`
	PositionMs int64  `json:"positionMs"`
	Episode    int    `json:"episode"`
	URL        string `json:"url"`
}

type client struct {
	memberID string
	conn     *websocket.Conn
	send     chan []byte
	done     chan struct{}
	once     sync.Once
}

func newClient(memberID string, conn *websocket.Conn) *client {
	return &client{
		memberID: memberID,
		conn:     conn,
		send:     make(chan []byte, 64),
		done:     make(chan struct{}),
	}
}

func (c *client) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.CloseNow()
	})
}

func (c *client) enqueue(data []byte) {
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.send <- data:
	case <-c.done:
	default:
		// 慢客户端：丢弃本条消息，不阻塞房间广播。
	}
}

func (c *client) enqueueJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.enqueue(data)
}

func (c *client) writeLoop() {
	for {
		select {
		case data := <-c.send:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := c.conn.Write(ctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				c.close()
				return
			}
		case <-c.done:
			return
		}
	}
}

// Room 保存成员、在线连接与权威时间轴。
type Room struct {
	ID string

	mu         sync.Mutex
	members    map[string]*Member
	clients    map[string]*client
	hostID     string
	state      State
	lastActive time.Time
}

func newRoom(id string, now time.Time) *Room {
	return &Room{
		ID:         id,
		members:    make(map[string]*Member),
		clients:    make(map[string]*client),
		state:      State{Paused: true, Rate: 1, ServerTimeMs: now.UnixMilli()},
		lastActive: now,
	}
}

func (r *Room) addMember(name string, host bool, token string) *Member {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := &Member{
		ID:    newMemberID(),
		Name:  name,
		Host:  host,
		token: token,
	}
	r.members[m.ID] = m
	r.lastActive = time.Now()
	return m
}

// Authenticate 校验成员凭证。
func (r *Room) Authenticate(memberID, token string) (*Member, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.members[memberID]
	if m == nil || token == "" || len(token) != len(m.token) {
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(m.token)) != 1 {
		return nil, false
	}
	return m, true
}

// State 返回当前时间轴快照。
func (r *Room) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// Current 返回服务端推算的当前播放位置。
func (r *Room) Current(now time.Time) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.Current(now)
}

// Apply 应用一条控制命令。仅房主可调用，成功时 Seq 自增。
func (r *Room) Apply(cmd Command) (State, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if cmd.MemberID != r.hostID {
		return r.state, ErrForbidden
	}

	now := time.Now()
	switch cmd.Op {
	case "play":
		if cmd.PositionMs > 0 {
			r.state.PositionMs = cmd.PositionMs
		}
		r.state.Paused = false
		if r.state.Rate <= 0 {
			r.state.Rate = 1
		}
	case "pause":
		if cmd.PositionMs > 0 {
			r.state.PositionMs = cmd.PositionMs
		} else {
			r.state.PositionMs = r.state.Current(now)
		}
		r.state.Paused = true
	case "seek":
		r.state.PositionMs = cmd.PositionMs
		if r.state.PositionMs < 0 {
			r.state.PositionMs = 0
		}
	case "episode":
		r.state.Episode = cmd.Episode
		r.state.URL = cmd.URL
		r.state.PositionMs = cmd.PositionMs
		if r.state.PositionMs < 0 {
			r.state.PositionMs = 0
		}
	default:
		return r.state, ErrUnknownOp
	}

	r.state.Seq++
	r.state.ServerTimeMs = now.UnixMilli()
	r.lastActive = now
	return r.state, nil
}

// Members 返回成员快照，房主排在最前。
func (r *Room) Members() []Member {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.membersLocked()
}

func (r *Room) membersLocked() []Member {
	list := make([]Member, 0, len(r.members))
	for _, m := range r.members {
		list = append(list, *m)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Host != list[j].Host {
			return list[i].Host
		}
		return list[i].ID < list[j].ID
	})
	return list
}

// Touch 刷新房间活跃时间。
func (r *Room) Touch() {
	r.mu.Lock()
	r.lastActive = time.Now()
	r.mu.Unlock()
}

func (r *Room) lastActiveAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastActive
}

func (r *Room) setBuffering(memberID string, buffering bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.members[memberID]; m != nil {
		m.Buffering = buffering
	}
	r.lastActive = time.Now()
}

func (r *Room) addClient(c *client) (State, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.members[c.memberID]
	if m == nil {
		return r.state, false
	}
	if old := r.clients[c.memberID]; old != nil && old != c {
		old.close()
	}
	r.clients[c.memberID] = c
	m.Online = true
	r.lastActive = time.Now()
	return r.state, true
}

func (r *Room) removeClient(c *client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur := r.clients[c.memberID]; cur != c {
		return
	}
	delete(r.clients, c.memberID)
	if m := r.members[c.memberID]; m != nil {
		m.Online = false
	}
	r.lastActive = time.Now()
}

// Broadcast 向房间内所有在线连接广播 JSON 消息。
func (r *Room) Broadcast(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	r.mu.Lock()
	clients := make([]*client, 0, len(r.clients))
	for _, c := range r.clients {
		clients = append(clients, c)
	}
	r.mu.Unlock()
	for _, c := range clients {
		c.enqueue(data)
	}
}

func (r *Room) close() {
	r.mu.Lock()
	clients := make([]*client, 0, len(r.clients))
	for _, c := range r.clients {
		clients = append(clients, c)
	}
	r.clients = make(map[string]*client)
	r.mu.Unlock()
	for _, c := range clients {
		c.close()
	}
}
