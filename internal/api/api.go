// Package api 实现 HTTP 接口层。
package api

import (
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"time"

	"cilicili/internal/auth"
	"cilicili/internal/cache"
	"cilicili/internal/config"
	"cilicili/internal/detail"
	"cilicili/internal/httpx"
	"cilicili/internal/live"
	"cilicili/internal/media"
	"cilicili/internal/model"
	"cilicili/internal/quality"
	"cilicili/internal/recommend"
	"cilicili/internal/search"
	"cilicili/internal/source"
	"cilicili/internal/subscribe"
	"cilicili/internal/upstream"
	"cilicili/internal/userdata"
)

// Deps 是 API 层的依赖。
type Deps struct {
	Cfg       *config.Config
	Log       *slog.Logger
	Auth      *auth.Manager
	Searcher  *search.Searcher
	Detail    *detail.Resolver
	Registry  *source.Registry
	Cache     cache.Store
	Client    upstream.Doer
	Proxy     http.Handler
	LiveProxy http.Handler
	Quality   *quality.Prober
	Live      *live.Service
	Recommend *recommend.Fetcher
	Subscribe *subscribe.Fetcher
	UserData  *userdata.Store
	Media     *media.Manager
	Version   string
}

// API 是所有 handler 的集合。
type API struct {
	deps  Deps
	start time.Time
}

// New 创建 API。
func New(d Deps) *API {
	return &API{deps: d, start: time.Now()}
}

// Health 是无鉴权健康检查。
func (a *API) Health(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"version":  a.deps.Version,
		"uptimeMs": time.Since(a.start).Milliseconds(),
	})
}

// Status 返回站点状态、鉴权状态与预置源。
func (a *API) Status(w http.ResponseWriter, r *http.Request) {
	verified := a.deps.Auth != nil && a.deps.Auth.FromRequest(r)
	httpx.WriteJSON(w, http.StatusOK, model.AuthStatusResponse{
		PasswordRequired:       a.deps.Auth != nil && a.deps.Auth.Configured(),
		Verified:               verified,
		Version:                a.deps.Version,
		DefaultSources:         a.deps.Registry.List(),
		DefaultLiveSources:     a.deps.Cfg.DefaultLiveSources,
		DefaultSubscriptions:   a.deps.Cfg.DefaultSubscriptions,
		DefaultRecommendSource: a.deps.Cfg.DefaultRecommendSource,
		DefaultImageMode:       a.deps.Cfg.DefaultImageMode,
		Capabilities: []string{
			"search.stream",
			"search.soft_deadline",
			"search.server_health",
			"search.fast_mode",
			"detail",
			"detail.playlines",
			"proxy",
			"sources",
			"sources.subscribe",
			"me.progress",
			"me.favorites",
			"quality",
			"live",
			"recommend",
			"media",
			"media.cover",
			"auth.bearer",
		},
	})
}

// AuthStatus 返回当前会话状态。
func (a *API) AuthStatus(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"verified": a.deps.Auth.FromRequest(r),
	})
}

type loginRequest struct {
	Password string `json:"password"`
}

// Login 校验密码并写入 cookie，同时返回 Bearer token 供 App 使用。
func (a *API) Login(w http.ResponseWriter, r *http.Request) {
	if a.deps.Auth == nil || !a.deps.Auth.Configured() {
		httpx.WriteError(w, http.StatusServiceUnavailable, "服务器未设置 PASSWORD 环境变量")
		return
	}
	var body loginRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	ip := httpx.ClientIP(r)
	if !a.deps.Auth.AllowLogin(ip) {
		httpx.WriteError(w, http.StatusTooManyRequests, "尝试次数过多，请 10 分钟后再试")
		return
	}
	if !a.deps.Auth.CheckPassword(body.Password) {
		httpx.WriteError(w, http.StatusUnauthorized, "密码错误")
		return
	}
	a.deps.Auth.ResetLogin(ip)
	token, expiresAt := a.deps.Auth.Sign()
	a.deps.Auth.SetCookie(w, r, token, expiresAt)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"token":     token,
		"expiresAt": expiresAt.UnixMilli(),
	})
}

// Logout 清除会话 cookie。
func (a *API) Logout(w http.ResponseWriter, r *http.Request) {
	a.deps.Auth.ClearCookie(w, r)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}

// Sources 返回源列表与服务端健康快照。
func (a *API) Sources(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"sources": a.deps.Registry.List(),
		"health":  a.deps.Registry.Health().Snapshot(now),
	})
}

type sourceResetRequest struct {
	Key string `json:"key"`
}

// ResetSource 清除单个源的熔断/停用状态（源管理页「一键恢复」）。
func (a *API) ResetSource(w http.ResponseWriter, r *http.Request) {
	var body sourceResetRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	key := strings.TrimSpace(body.Key)
	if key == "" {
		httpx.WriteError(w, http.StatusBadRequest, "缺少 key")
		return
	}
	if _, ok := a.deps.Registry.Get(key); !ok {
		httpx.WriteError(w, http.StatusNotFound, "源不存在")
		return
	}
	a.deps.Registry.Health().Reset(key)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}

// Metrics 返回运行指标（轻量 JSON；后续可替换为 Prometheus）。
func (a *API) Metrics(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{
		"version":    a.deps.Version,
		"uptimeMs":   time.Since(a.start).Milliseconds(),
		"goroutines": runtime.NumGoroutine(),
		"cache":      a.deps.Cache.Stats(),
		"health":     a.deps.Registry.Health().Snapshot(time.Now()),
	}
	if a.deps.Media != nil {
		if stats, err := a.deps.Media.Store().Stats(); err == nil {
			payload["media"] = stats
		}
	}
	httpx.WriteJSON(w, http.StatusOK, payload)
}
