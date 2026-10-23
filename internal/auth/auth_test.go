package auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	m := New("pw", "secret", time.Hour)
	token, exp := m.Sign()
	if token == "" || !exp.After(time.Now()) {
		t.Fatalf("bad token/exp: %q %v", token, exp)
	}
	if !m.Verify(token) {
		t.Fatal("token should verify")
	}
	if m.Verify(token + "x") {
		t.Fatal("tampered token should not verify")
	}
	if New("pw", "other-secret", time.Hour).Verify(token) {
		t.Fatal("token should not verify with another secret")
	}
}

func TestExpiredToken(t *testing.T) {
	m := New("pw", "secret", -time.Second)
	token, _ := m.Sign()
	// Sign 内部会把 <=0 的 ttl 兜底为 90 天，这里直接构造一个过期 payload。
	exp := base64.RawURLEncoding.EncodeToString([]byte("1"))
	payload := "v1." + exp + "." + m.sign("v1."+exp)
	if m.Verify(payload) {
		t.Fatal("expired token should not verify")
	}
	_ = token
}

func TestFromRequest(t *testing.T) {
	m := New("pw", "s", time.Hour)
	token, _ := m.Sign()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if !m.FromRequest(r) {
		t.Fatal("bearer should verify")
	}

	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	if !m.FromRequest(r) {
		t.Fatal("cookie should verify")
	}

	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "bad"})
	if m.FromRequest(r) {
		t.Fatal("bad cookie should not verify")
	}
}

func TestCheckPassword(t *testing.T) {
	m := New("s3cret", "s", time.Hour)
	if !m.CheckPassword("s3cret") {
		t.Fatal("correct password rejected")
	}
	if m.CheckPassword("S3cret") {
		t.Fatal("wrong password accepted")
	}
	if New("", "s", time.Hour).Configured() {
		t.Fatal("empty password should not be configured")
	}
}

func TestRateLimit(t *testing.T) {
	m := New("pw", "s", time.Hour)
	for i := 0; i < loginMaxAttempts; i++ {
		if !m.AllowLogin("1.2.3.4") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if m.AllowLogin("1.2.3.4") {
		t.Fatal("should be rate limited")
	}
	m.ResetLogin("1.2.3.4")
	if !m.AllowLogin("1.2.3.4") {
		t.Fatal("reset should allow again")
	}
}

func TestIsHTTPS(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if IsHTTPS(r) {
		t.Fatal("plain request should not be https")
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if !IsHTTPS(r) {
		t.Fatal("x-forwarded-proto https should be https")
	}
}

func TestGC(t *testing.T) {
	m := New("pw", "s", time.Hour)
	m.AllowLogin("ip")
	m.mu.Lock()
	a := m.attempts["ip"]
	a.resetAt = time.Now().Add(-time.Minute)
	m.attempts["ip"] = a
	m.mu.Unlock()
	m.GC(time.Now())
	if _, ok := m.attempts["ip"]; ok {
		t.Fatal("expired attempt should be removed")
	}
}
