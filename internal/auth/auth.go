package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// SessionCookie 是 Web 端会话 cookie 名。
	SessionCookie = "cilici_session"
	// BearerPrefix 是 App 端 Authorization 头前缀。
	BearerPrefix = "Bearer "
)

// Manager 负责密码校验、会话 token 签发/验证与登录限速。
type Manager struct {
	password string
	secret   []byte
	ttl      time.Duration

	mu       sync.Mutex
	attempts map[string]attempt
}

type attempt struct {
	count   int
	resetAt time.Time
}

// New 创建鉴权管理器。secret 为空时从 password 派生。
func New(password, secret string, ttl time.Duration) *Manager {
	if ttl <= 0 {
		ttl = 90 * 24 * time.Hour
	}
	if secret == "" {
		secret = "derived:" + password
	}
	return &Manager{
		password: password,
		secret:   []byte(secret),
		ttl:      ttl,
		attempts: make(map[string]attempt),
	}
}

// Configured 表示部署者是否设置了 PASSWORD。
func (m *Manager) Configured() bool { return m.password != "" }

// CheckPassword 做恒定时间比较。
func (m *Manager) CheckPassword(input string) bool {
	a := sha256.Sum256([]byte(input))
	b := sha256.Sum256([]byte(m.password))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// Sign 生成会话 token，格式：v1.<base64url(expUnix)>.<hex hmac>。
func (m *Manager) Sign() (token string, expiresAt time.Time) {
	expiresAt = time.Now().Add(m.ttl)
	payload := "v1." + base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(expiresAt.Unix(), 10)))
	return payload + "." + m.sign(payload), expiresAt
}

// Verify 校验 token 的签名与有效期。
func (m *Manager) Verify(token string) bool {
	token = strings.TrimSpace(token)
	lastDot := strings.LastIndexByte(token, '.')
	if lastDot <= 0 || lastDot == len(token)-1 {
		return false
	}
	payload, sig := token[:lastDot], token[lastDot+1:]
	want := m.sign(payload)
	if len(sig) != len(want) || subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return false
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 2 || parts[0] != "v1" {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	exp, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < exp
}

// FromRequest 从 Authorization: Bearer 或 cookie 中取会话并校验。
func (m *Manager) FromRequest(r *http.Request) bool {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, BearerPrefix) {
		if m.Verify(strings.TrimSpace(strings.TrimPrefix(h, BearerPrefix))) {
			return true
		}
	}
	if c, err := r.Cookie(SessionCookie); err == nil && m.Verify(c.Value) {
		return true
	}
	// 播放器/流媒体代理拿不到自定义请求头，允许 query 传 token（仅用于 /api/proxy 等流地址）。
	if q := r.URL.Query(); q.Get("token") != "" {
		if m.Verify(strings.TrimSpace(q.Get("token"))) {
			return true
		}
	}
	return false
}

// SetCookie 写入 Web 端会话 cookie。
func (m *Manager) SetCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   IsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
	})
}

// ClearCookie 删除会话 cookie。
func (m *Manager) ClearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   IsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// IsHTTPS 根据反代头或 TLS 状态判断请求是否走 HTTPS。
func IsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

const (
	loginMaxAttempts = 10
	loginWindow      = 10 * time.Minute
)

// AllowLogin 返回该 IP 是否还能尝试登录。
func (m *Manager) AllowLogin(ip string) bool {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.attempts[ip]
	if !ok || now.After(a.resetAt) {
		m.attempts[ip] = attempt{count: 1, resetAt: now.Add(loginWindow)}
		return true
	}
	if a.count >= loginMaxAttempts {
		return false
	}
	a.count++
	m.attempts[ip] = a
	return true
}

// ResetLogin 在登录成功后清空该 IP 的失败计数。
func (m *Manager) ResetLogin(ip string) {
	m.mu.Lock()
	delete(m.attempts, ip)
	m.mu.Unlock()
}

// GC 清理过期的登录限速记录。
func (m *Manager) GC(now time.Time) {
	m.mu.Lock()
	for ip, a := range m.attempts {
		if now.After(a.resetAt) {
			delete(m.attempts, ip)
		}
	}
	m.mu.Unlock()
}

func (m *Manager) sign(payload string) string {
	h := hmac.New(sha256.New, m.secret)
	_, _ = h.Write([]byte(payload))
	return hex.EncodeToString(h.Sum(nil))
}
