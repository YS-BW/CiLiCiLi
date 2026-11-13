// Package server 组装路由、中间件与鉴权守卫。
package server

import (
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"cilicili/internal/api"
	"cilicili/internal/auth"
	"cilicili/internal/config"
	"cilicili/internal/httpx"
	"cilicili/internal/watchparty"
)

// Deps 是 Server 的依赖。
type Deps struct {
	API        *api.API
	Cfg        *config.Config
	Log        *slog.Logger
	Auth       *auth.Manager
	WatchParty *watchparty.Handler
}

// New 返回完整的 HTTP handler。
// spaHandler 托管 Flutter Web 静态文件，未命中的路径回退 index.html（SPA 路由）。
func spaHandler(root string) http.Handler {
	files := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		clean := path.Clean("/" + r.URL.Path)
		full := filepath.Join(root, filepath.FromSlash(clean))
		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			switch filepath.Ext(full) {
			case ".js", ".wasm", ".ttf", ".otf", ".woff2", ".png", ".jpg", ".svg", ".json":
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			default:
				w.Header().Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(root, "index.html"))
	})
}

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

	// 一起看：建房需要登录，加入房间与 WebSocket 用成员 token 自鉴权。
	watchParty := d.WatchParty
	if watchParty == nil {
		watchParty = watchparty.NewHandler(watchparty.NewHub())
	}
	mux.HandleFunc("POST /api/watch/rooms", guard(watchParty.Create))
	mux.HandleFunc("POST /api/watch/rooms/{id}/join", watchParty.Join)
	mux.HandleFunc("POST /api/watch/rooms/{id}/leave", watchParty.Leave)
	mux.HandleFunc("GET /api/watch/rooms/{id}/ws", watchParty.WS)

	mux.HandleFunc("POST /api/search", guard(d.API.Search))
	mux.HandleFunc("GET /api/detail", guard(d.API.Detail))
	mux.HandleFunc("POST /api/latency", guard(d.API.Latency))
	mux.HandleFunc("GET /api/sources", guard(d.API.Sources))
	mux.HandleFunc("POST /api/sources", guard(d.API.SourceCreate))
	mux.HandleFunc("POST /api/sources/test", guard(d.API.SourceTest))
	mux.HandleFunc("POST /api/sources/reset", guard(d.API.ResetSource))
	mux.HandleFunc("POST /api/sources/reorder", guard(d.API.SourceReorder))
	mux.HandleFunc("GET /api/sources/probe", guard(d.API.SourcesProbe))
	mux.HandleFunc("POST /api/sources/probe", guard(d.API.SourcesProbe))
	mux.HandleFunc("PUT /api/sources/{key}", guard(d.API.SourceUpdate))
	mux.HandleFunc("DELETE /api/sources/{key}", guard(d.API.SourceDelete))

	mux.HandleFunc("GET /api/subscriptions", guard(d.API.Subscriptions))
	mux.HandleFunc("POST /api/subscriptions", guard(d.API.SubscriptionCreate))
	mux.HandleFunc("PUT /api/subscriptions/{id}", guard(d.API.SubscriptionUpdate))
	mux.HandleFunc("DELETE /api/subscriptions/{id}", guard(d.API.SubscriptionDelete))
	mux.HandleFunc("POST /api/subscriptions/{id}/refresh", guard(d.API.SubscriptionRefresh))
	mux.HandleFunc("POST /api/subscriptions/{id}/apply", guard(d.API.SubscriptionApply))
	mux.HandleFunc("GET /api/metrics", guard(d.API.Metrics))

	// 观看进度与收藏（第一版默认用户）
	mux.HandleFunc("PUT /api/me/progress", guard(d.API.PutProgress))
	mux.HandleFunc("GET /api/me/history", guard(d.API.History))
	mux.HandleFunc("DELETE /api/me/history/{id}", guard(d.API.DeleteHistory))
	mux.HandleFunc("GET /api/me/favorites", guard(d.API.Favorites))
	mux.HandleFunc("PUT /api/me/favorites", guard(d.API.PutFavorite))
	mux.HandleFunc("DELETE /api/me/favorites/{id}", guard(d.API.DeleteFavorite))

	// 媒体聚合与第三方元数据（Fladder 统一封面/简介/匹配）
	mux.HandleFunc("POST /api/media/resolve", guard(d.API.MediaResolve))
	mux.HandleFunc("GET /api/media/{id}", guard(d.API.MediaGet))
	mux.HandleFunc("GET /api/media/{id}/metadata-candidates", guard(d.API.MediaCandidates))
	mux.HandleFunc("POST /api/media/{id}/match", guard(d.API.MediaMatch))
	mux.HandleFunc("POST /api/media/{id}/cover", guard(d.API.MediaSetCover))
	mux.HandleFunc("GET /api/media/{id}/cover", d.API.MediaCover)
	mux.HandleFunc("GET /api/cover", guard(d.API.Cover))
	mux.HandleFunc("POST /api/media/{id}/refresh-metadata", guard(d.API.MediaRefresh))

	// 代理自己处理鉴权（匿名图片域），这里不再套 guard。
	mux.HandleFunc("GET /api/proxy", d.API.Proxy)
	mux.HandleFunc("GET /api/proxy/{target...}", d.API.Proxy)

	// 下一阶段接口：先占位，保证 Web/App 的契约固定。
	mux.HandleFunc("POST /api/quality", guard(d.API.Quality))
	mux.HandleFunc("GET /api/source-list", guard(d.API.SourceList))
	mux.HandleFunc("POST /api/publish", guard(d.API.NotImplemented("源发布")))
	mux.HandleFunc("GET /api/douban", guard(d.API.Douban))
	mux.HandleFunc("GET /api/hot-list", guard(d.API.HotList))
	mux.HandleFunc("GET /api/backdrop", guard(d.API.Backdrop))
	mux.HandleFunc("GET /api/live/playlist", guard(d.API.LivePlaylist))
	mux.HandleFunc("GET /api/live/epg", guard(d.API.LiveEPG))
	mux.HandleFunc("POST /api/live/probe", guard(d.API.LiveProbe))
	mux.HandleFunc("GET /api/live/stream", d.API.LiveStream)
	mux.HandleFunc("GET /api/live/stream/{target...}", d.API.LiveStream)

	// 可选：同源托管 Flutter Web（WEB_DIR），容器部署时前后端一个进程。
	if dir := strings.TrimSpace(d.Cfg.WebDir); dir != "" {
		mux.Handle("GET /", spaHandler(dir))
	}

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
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
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
