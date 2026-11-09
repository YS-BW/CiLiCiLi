// Package watchparty 实现「一起看」的服务端权威时间轴与房间管理。
package watchparty

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const (
	maxEpisodeCount    = 2000
	maxMetaFieldRunes  = 200
	maxEpisodeNameRune = 200
	maxEpisodeURLRunes = 4096
	maxNicknameRunes   = 16
	maxDanmakuRunes    = 80
)

// Episode 是房间内的剧集条目，仅保留播放所需的名称与地址。
type Episode struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

var (
	// ErrForbidden 表示非房主尝试提交控制命令。
	ErrForbidden = errors.New("仅房主可以控制播放")
	// ErrUnknownOp 表示未知的控制命令。
	ErrUnknownOp = errors.New("未知控制命令")
	// ErrDanmakuEmpty 表示弹幕内容为空。
	ErrDanmakuEmpty = errors.New("弹幕内容不能为空")
	// ErrDanmakuTooLong 表示弹幕超过长度限制。
	ErrDanmakuTooLong = errors.New("弹幕内容太长")
)

// State 是房间唯一的权威时间轴状态。
type State struct {
	Seq             int64     `json:"seq"`
	Paused          bool      `json:"paused"`
	PositionMs      int64     `json:"positionMs"`
	ServerTimeMs    int64     `json:"serverTimeMs"`
	Rate            float64   `json:"rate"`
	Episode         int       `json:"episode"`
	URL             string    `json:"url"`
	ContentOffsetMs int64     `json:"contentOffsetMs"`
	Title           string    `json:"title,omitempty"`
	SourceKey       string    `json:"sourceKey,omitempty"`
	VodID           string    `json:"vodId,omitempty"`
	SourceName      string    `json:"sourceName,omitempty"`
	Episodes        []Episode `json:"episodes,omitempty"`
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
// memberId 是成员的稳定身份，未来的弹幕等功能应通过 memberId 归属发言，
// 而不是依赖可变的昵称。
type Member struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Host      bool   `json:"host"`
	Online    bool   `json:"online"`
	JoinedAt  int64  `json:"joinedAt"`
	Buffering bool   `json:"-"`

	token   string
	joinSeq int64
}

// Command 是客户端提交的控制命令。
type Command struct {
	MemberID   string    `json:"memberId,omitempty"`
	Op         string    `json:"op"`
	PositionMs int64     `json:"positionMs"`
	Episode    int       `json:"episode"`
	URL        string    `json:"url"`
	Title      string    `json:"title,omitempty"`
	SourceKey  string    `json:"sourceKey,omitempty"`
	VodID      string    `json:"vodId,omitempty"`
	SourceName string    `json:"sourceName,omitempty"`
	Episodes   []Episode `json:"episodes,omitempty"`
}

// Danmaku 是实时广播的弹幕消息。AtMs 是服务端广播时间，不作为视频时间轴使用。
type Danmaku struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Color string `json:"color,omitempty"`
	Mode  int    `json:"mode"`
	ID    string `json:"id"`
	AtMs  int64  `json:"atMs"`
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

type closedMessage struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
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
	joinSeq    int64
	closed     bool
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
	r.joinSeq++
	m := &Member{
		ID:       newMemberID(),
		Name:     r.uniqueNameLocked(name),
		Host:     host,
		JoinedAt: time.Now().UnixMilli(),
		token:    token,
		joinSeq:  r.joinSeq,
	}
	r.members[m.ID] = m
	r.lastActive = time.Now()
	return m
}

func (r *Room) uniqueNameLocked(name string) string {
	name = truncateRunes(name, maxNicknameRunes)
	taken := make(map[string]bool, len(r.members))
	for _, member := range r.members {
		taken[strings.ToLower(member.Name)] = true
	}
	if !taken[strings.ToLower(name)] {
		return name
	}
	for n := 2; ; n++ {
		suffix := "#" + strconv.Itoa(n)
		baseLimit := maxNicknameRunes - utf8.RuneCountInString(suffix)
		if baseLimit < 1 {
			return truncateRunes(name, maxNicknameRunes)
		}
		candidate := truncateRunes(name, baseLimit) + suffix
		if !taken[strings.ToLower(candidate)] {
			return candidate
		}
	}
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
		if title := strings.TrimSpace(cmd.Title); title != "" {
			r.state.Title = truncateRunes(title, maxMetaFieldRunes)
		}
		if sourceKey := strings.TrimSpace(cmd.SourceKey); sourceKey != "" {
			r.state.SourceKey = truncateRunes(sourceKey, maxMetaFieldRunes)
		}
		if vodID := strings.TrimSpace(cmd.VodID); vodID != "" {
			r.state.VodID = truncateRunes(vodID, maxMetaFieldRunes)
		}
		if sourceName := strings.TrimSpace(cmd.SourceName); sourceName != "" {
			r.state.SourceName = truncateRunes(sourceName, maxMetaFieldRunes)
		}
		if len(cmd.Episodes) > 0 {
			r.state.Episodes = sanitizeEpisodes(cmd.Episodes)
		}
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

// Members 返回成员快照，房主排在最前，其余按加入顺序排列。
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
		if list[i].JoinedAt != list[j].JoinedAt {
			return list[i].JoinedAt < list[j].JoinedAt
		}
		if list[i].joinSeq != list[j].joinSeq {
			return list[i].joinSeq < list[j].joinSeq
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
	if r.closed {
		return r.state, false
	}
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

// IsHost 判断成员是否为房主。
func (r *Room) IsHost(memberID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return memberID != "" && memberID == r.hostID
}

// HostOnline 返回房主当前是否有在线连接。
func (r *Room) HostOnline() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hostOnlineLocked()
}

func (r *Room) hostOnlineLocked() bool {
	return r.clients[r.hostID] != nil
}

// removeMember 移除成员及其连接。返回是否确实移除了成员。
func (r *Room) removeMember(memberID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.members[memberID] == nil {
		return false
	}
	delete(r.members, memberID)
	if c := r.clients[memberID]; c != nil {
		delete(r.clients, memberID)
		c.close()
	}
	r.lastActive = time.Now()
	return true
}

// beginDissolve 标记房间已解散并取走全部连接。
// requireHostOffline 为真时，若房主仍在线则不做任何修改。
func (r *Room) beginDissolve(requireHostOffline bool) ([]*client, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false
	}
	if requireHostOffline && r.hostOnlineLocked() {
		return nil, false
	}
	r.closed = true
	clients := make([]*client, 0, len(r.clients))
	for _, c := range r.clients {
		clients = append(clients, c)
	}
	r.clients = make(map[string]*client)
	return clients, true
}

const closedFlushDelay = 100 * time.Millisecond

// finishDissolve 广播 closed 事件并关闭所有连接。
func (r *Room) finishDissolve(clients []*client, reason string) {
	if data, err := json.Marshal(closedMessage{Type: "closed", Reason: reason}); err == nil {
		for _, c := range clients {
			c.enqueue(data)
		}
	}
	if len(clients) > 0 {
		// 给 writeLoop 一点时间把 closed 事件刷出去，再断开连接。
		time.Sleep(closedFlushDelay)
	}
	for _, c := range clients {
		c.close()
	}
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
	clients, ok := r.beginDissolve(false)
	if !ok {
		return
	}
	for _, c := range clients {
		c.close()
	}
}

func sanitizeText(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if unicode.IsControl(r) {
			if unicode.IsSpace(r) {
				b.WriteRune(' ')
			}
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func sanitizeName(raw string) string {
	return truncateRunes(sanitizeText(raw), maxNicknameRunes)
}

func sanitizeDanmaku(raw string) (string, error) {
	text := sanitizeText(raw)
	if text == "" {
		return "", ErrDanmakuEmpty
	}
	if utf8.RuneCountInString(text) > maxDanmakuRunes {
		return "", ErrDanmakuTooLong
	}
	return text, nil
}

func sanitizeDanmakuColor(raw string) string {
	color := strings.TrimSpace(raw)
	if len(color) != 4 && len(color) != 7 {
		return ""
	}
	if color[0] != '#' {
		return ""
	}
	for _, r := range color[1:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return ""
		}
	}
	return strings.ToLower(color)
}

func sanitizeDanmakuMode(mode int) int {
	if mode < 0 || mode > 2 {
		return 0
	}
	return mode
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

func sanitizeEpisodes(list []Episode) []Episode {
	if len(list) > maxEpisodeCount {
		list = list[:maxEpisodeCount]
	}
	out := make([]Episode, 0, len(list))
	for _, ep := range list {
		out = append(out, Episode{
			Name: truncateRunes(strings.TrimSpace(ep.Name), maxEpisodeNameRune),
			URL:  truncateRunes(strings.TrimSpace(ep.URL), maxEpisodeURLRunes),
		})
	}
	return out
}
