// Package server 组装路由、中间件与鉴权守卫。
package server

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"cilicili/internal/api"
	"cilicili/internal/auth"
	"cilicili/internal/config"
	"cilicili/internal/httpx"
)

// Deps 是 Server 的依赖。
type Deps struct {
	API  *api.API
	Cfg  *config.Config
	Log  *slog.Logger
	Auth *auth.Manager
}

// New 返回完整的 HTTP handler。
func New(d Deps) http.Handler {
	mux := http.NewServeMux()

	// 公开接口
	mux.HandleFunc("GET /api/health", d.API.Health)
	mux.HandleFunc("GET /api/status", d.API.Status)
	mux.HandleFunc("POST /api/auth", d.API.Login)
	mux.HandleFunc("GET /api/auth", d.API.AuthStatus)
	mux.HandleFunc("DELETE /api/auth", d.API.Logout)

	// 需要登录的接口
	guard := func(h http.HandlerFunc) http.HandlerFunc { return requireAuth(d.Auth, h) }
	mux.HandleFunc("POST /api/search", guard(d.API.Search))
	mux.HandleFunc("GET /api/detail", guard(d.API.Detail))
	mux.HandleFunc("GET /api/sources", guard(d.API.Sources))
	mux.HandleFunc("POST /api/sources/test", guard(d.API.SourceTest))
	mux.HandleFunc("GET /api/metrics", guard(d.API.Metrics))

	// 代理自己处理鉴权（匿名图片域），这里不再套 guard。
	mux.HandleFunc("GET /api/proxy", d.API.Proxy)
	mux.HandleFunc("GET /api/proxy/{target...}", d.API.Proxy)

	// 下一阶段接口：先占位，保证 Web/App 的契约固定。
	mux.HandleFunc("POST /api/quality", guard(d.API.NotImplemented("画质探测")))
	mux.HandleFunc("GET /api/source-list", guard(d.API.NotImplemented("源订阅")))
	mux.HandleFunc("POST /api/publish", guard(d.API.NotImplemented("源发布")))
	mux.HandleFunc("GET /api/douban", guard(d.API.NotImplemented("豆瓣推荐")))
	mux.HandleFunc("GET /api/bangumi/calendar", guard(d.API.NotImplemented("Bangumi 放送表")))
	mux.HandleFunc("GET /api/hot-list", guard(d.API.NotImplemented("影视榜单")))
	mux.HandleFunc("GET /api/live/playlist", guard(d.API.NotImplemented("直播列表")))
	mux.HandleFunc("GET /api/live/epg", guard(d.API.NotImplemented("EPG")))
	mux.HandleFunc("POST /api/live/probe", guard(d.API.NotImplemented("直播测活")))
	mux.HandleFunc("GET /api/live/stream", d.API.LiveStream)
	mux.HandleFunc("GET /api/live/stream/{target...}", d.API.LiveStream)

	// 根路径：后续 Web 前端由 nginx 静态托管；这里只给一个服务标识。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			httpx.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"service": "cilici-core",
			"version": d.Cfg.Version,
			"api":     "/api",
		})
	})

	h := http.Handler(mux)
	h = withCORS(d.Cfg.CorsOrigins, h)
	h = withSecurityHeaders(h)
	h = withLogging(d.Log, h)
	h = withRecover(d.Log, h)
	return h
}

func requireAuth(m *auth.Manager, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m == nil || !m.Configured() {
			httpx.WriteError(w, http.StatusServiceUnavailable, "服务器未设置 PASSWORD 环境变量")
			return
		}
		if !m.FromRequest(r) {
			httpx.WriteError(w, http.StatusUnauthorized, "未登录")
			return
		}
		next(w, r)
	}
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

func withCORS(origins []string, next http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(origins))
	any := false
	for _, o := range origins {
		if o == "*" {
			any = true
			continue
		}
		allowed[strings.TrimRight(o, "/")] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimRight(r.Header.Get("Origin"), "/")
		if origin != "" {
			_, ok := allowed[origin]
			if ok || any {
				if any {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				}
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Range")
				w.Header().Set("Access-Control-Expose-Headers", "Content-Range, Accept-Ranges, ETag, X-Search-Sources")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wrote {
		return
	}
	s.wrote = true
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func withLogging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/api/health" {
			return
		}
		log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"durationMs", time.Since(start).Milliseconds(),
			"ip", httpx.ClientIP(r),
		)
	})
}

func withRecover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("panic", "error", rec, "path", r.URL.Path, "stack", string(debug.Stack()))
				httpx.WriteError(w, http.StatusInternalServerError, "服务器内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
