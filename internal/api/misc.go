package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"cilicili/internal/cms"
	"cilicili/internal/httpx"
	"cilicili/internal/model"
	"cilicili/internal/upstream"
)

type sourceTestRequest struct {
	URL string `json:"url"`
}

// SourceTest 对单个点播源做一次轻量探活（搜索关键词 test）。
func (a *API) SourceTest(w http.ResponseWriter, r *http.Request) {
	var body sourceTestRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	raw := strings.TrimRight(strings.TrimSpace(body.URL), "/")
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		httpx.WriteError(w, http.StatusBadRequest, "无效的源地址")
		return
	}
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	target := raw + sep + "ac=videolist&wd=test"
	src := model.SourceConfig{Key: "test", Name: "test", URL: raw}

	start := time.Now()
	res, err := a.deps.Client.Do(r.Context(), target, upstream.Options{
		Headers:       cms.RequestHeaders(a.deps.Cfg.UserAgent),
		Timeout:       6 * time.Second,
		MaxBytes:      8 << 20,
		Retries:       1,
		SafeRedirects: true,
	})
	ms := time.Since(start).Milliseconds()
	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": false, "ms": ms, "error": err.Error()})
		return
	}
	defer res.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"ok": false, "ms": ms, "error": fmt.Sprintf("HTTP %d", res.StatusCode),
		})
		return
	}
	data, err := res.ReadAll(8 << 20)
	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": false, "ms": ms, "error": err.Error()})
		return
	}
	list, _, err := cms.ParseSearchList(data, src)
	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": false, "ms": ms, "error": err.Error()})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "ms": ms, "count": len(list)})
}

// Proxy 转交流式代理（鉴权由 proxy handler 自身处理，以支持匿名图片域）。
func (a *API) Proxy(w http.ResponseWriter, r *http.Request) {
	if a.deps.Proxy == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "proxy 尚未启用")
		return
	}
	a.deps.Proxy.ServeHTTP(w, r)
}

// LiveStream 转发直播长连接代理。
func (a *API) LiveStream(w http.ResponseWriter, r *http.Request) {
	if a.deps.LiveProxy == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "live stream 尚未启用")
		return
	}
	a.deps.LiveProxy.ServeHTTP(w, r)
}

// NotImplemented 为下一阶段接口占位，返回明确的 501。
func (a *API) NotImplemented(feature string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, http.StatusNotImplemented, feature+" 尚未实现（下一阶段）")
	}
}
