package watchparty

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// DefaultIdleTimeout 是房间空闲回收时间。
const DefaultIdleTimeout = 30 * time.Minute

// DefaultHostGrace 是房主掉线后保留房间的默认宽限时间。
const DefaultHostGrace = 30 * time.Second

// ErrRoomNotFound 表示房间不存在或已过期。
var ErrRoomNotFound = errors.New("房间不存在或已过期")

// Hub 管理内存中的全部房间。
type Hub struct {
	mu              sync.Mutex
	rooms           map[string]*Room
	hostGraceTimers map[string]*time.Timer
	idle            time.Duration
	hostGrace       time.Duration
	now             func() time.Time
	reaperStarted   bool
	closed          bool
	stop            chan struct{}
	done            chan struct{}
	closeOnce       sync.Once
}

// NewHub 创建使用默认空闲时间与房主宽限时间的 Hub。
func NewHub() *Hub {
	return NewHubWithIdle(DefaultIdleTimeout)
}

// NewHubWithIdle 创建指定空闲时间的 Hub。回收协程在第一个房间创建时启动。
func NewHubWithIdle(idle time.Duration) *Hub {
	return NewHubWithOptions(idle, DefaultHostGrace)
}

// NewHubWithOptions 创建指定空闲时间与房主掉线宽限时间的 Hub。
func NewHubWithOptions(idle, hostGrace time.Duration) *Hub {
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}
	if hostGrace <= 0 {
		hostGrace = DefaultHostGrace
	}
	return &Hub{
		rooms:           make(map[string]*Room),
		hostGraceTimers: make(map[string]*time.Timer),
		idle:            idle,
		hostGrace:       hostGrace,
		now:             time.Now,
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
	}
}

// SetHostGrace 设置房主掉线宽限时间，主要用于测试。建议在创建房间前调用。
func (h *Hub) SetHostGrace(d time.Duration) {
	if d <= 0 {
		d = DefaultHostGrace
	}
	h.mu.Lock()
	h.hostGrace = d
	h.mu.Unlock()
}

// Create 创建房间并返回房主成员与成员凭证。
func (h *Hub) Create(name string) (*Room, *Member, string) {
	h.startReaper()

	now := h.now()
	room := newRoom(newRoomID(), now)
	token := newToken()
	member := room.addMember(name, true, token)
	room.hostID = member.ID

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		room.close()
		return nil, nil, ""
	}
	for h.rooms[room.ID] != nil {
		room.ID = newRoomID()
	}
	h.rooms[room.ID] = room
	h.mu.Unlock()
	return room, member, token
}

// Join 向已有房间添加一名普通成员。
func (h *Hub) Join(roomID, name string) (*Room, *Member, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, "", ErrRoomNotFound
	}
	room := h.rooms[roomID]
	if room == nil {
		return nil, nil, "", ErrRoomNotFound
	}
	token := newToken()
	member := room.addMember(name, false, token)
	return room, member, token, nil
}

// Get 查找房间。
func (h *Hub) Get(roomID string) (*Room, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room, ok := h.rooms[roomID]
	return room, ok
}

// dissolve 从 Hub 移除房间并广播 closed 事件。重复调用安全。
func (h *Hub) dissolve(roomID, reason string) {
	h.mu.Lock()
	room := h.rooms[roomID]
	if room == nil {
		h.mu.Unlock()
		return
	}
	delete(h.rooms, roomID)
	if t := h.hostGraceTimers[roomID]; t != nil {
		t.Stop()
		delete(h.hostGraceTimers, roomID)
	}
	clients, ok := room.beginDissolve(false)
	h.mu.Unlock()
	if !ok {
		return
	}
	room.finishDissolve(clients, reason)
}

// removeGuest 移除普通成员，返回是否成功。
func (h *Hub) removeGuest(roomID, memberID string) bool {
	h.mu.Lock()
	room := h.rooms[roomID]
	if room == nil {
		h.mu.Unlock()
		return false
	}
	if !room.removeMember(memberID) {
		h.mu.Unlock()
		return false
	}
	members := room.Members()
	h.mu.Unlock()
	room.Broadcast(membersMessage{Type: "members", Members: members})
	return true
}

func (h *Hub) scheduleHostGrace(roomID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.rooms[roomID] == nil {
		return
	}
	if t := h.hostGraceTimers[roomID]; t != nil {
		t.Stop()
	}
	grace := h.hostGrace
	h.hostGraceTimers[roomID] = time.AfterFunc(grace, func() {
		h.hostGraceFired(roomID)
	})
}

func (h *Hub) cancelHostGrace(roomID string) {
	h.mu.Lock()
	if t := h.hostGraceTimers[roomID]; t != nil {
		t.Stop()
		delete(h.hostGraceTimers, roomID)
	}
	h.mu.Unlock()
}

func (h *Hub) hostGraceFired(roomID string) {
	h.mu.Lock()
	room := h.rooms[roomID]
	if room == nil {
		delete(h.hostGraceTimers, roomID)
		h.mu.Unlock()
		return
	}
	delete(h.hostGraceTimers, roomID)
	clients, ok := room.beginDissolve(true)
	if !ok {
		h.mu.Unlock()
		return
	}
	delete(h.rooms, roomID)
	h.mu.Unlock()
	room.finishDissolve(clients, "host_disconnected")
}

// Close 停止回收协程并关闭所有房间连接。
func (h *Hub) Close() {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		started := h.reaperStarted
		rooms := h.rooms
		h.rooms = make(map[string]*Room)
		for id, t := range h.hostGraceTimers {
			t.Stop()
			delete(h.hostGraceTimers, id)
		}
		h.mu.Unlock()

		if started {
			close(h.stop)
			<-h.done
		}
		for _, room := range rooms {
			room.close()
		}
	})
}

func (h *Hub) startReaper() {
	h.mu.Lock()
	if h.reaperStarted || h.closed {
		h.mu.Unlock()
		return
	}
	h.reaperStarted = true
	h.mu.Unlock()
	go h.reapLoop()
}

func (h *Hub) reapLoop() {
	defer close(h.done)
	interval := h.idle / 2
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.reap()
		case <-h.stop:
			return
		}
	}
}

func (h *Hub) reap() {
	now := h.now()
	var expired []*Room
	h.mu.Lock()
	for id, room := range h.rooms {
		if now.Sub(room.lastActiveAt()) >= h.idle {
			delete(h.rooms, id)
			if t := h.hostGraceTimers[id]; t != nil {
				t.Stop()
				delete(h.hostGraceTimers, id)
			}
			expired = append(expired, room)
		}
	}
	h.mu.Unlock()
	for _, room := range expired {
		room.close()
	}
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("watchparty: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

func newRoomID() string    { return randomHex(8) }
func newMemberID() string  { return "m_" + randomHex(8) }
func newDanmakuID() string { return "d_" + randomHex(8) }
func newToken() string     { return randomHex(24) }
