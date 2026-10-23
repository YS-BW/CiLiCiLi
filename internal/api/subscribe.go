package api

import (
	"errors"
	"net/http"
	"strings"

	"cilicili/internal/httpx"
	"cilicili/internal/subscribe"
)

// SourceList 拉取并解析远程数据源订阅。
func (a *API) SourceList(w http.ResponseWriter, r *http.Request) {
	if a.deps.Subscribe == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "源订阅尚未启用")
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("url"))
	if raw == "" {
		httpx.WriteError(w, http.StatusBadRequest, "缺少 url 参数")
		return
	}
	payload, err := a.deps.Subscribe.Fetch(r.Context(), raw)
	if err != nil {
		switch {
		case errors.Is(err, subscribe.ErrInvalidURL), errors.Is(err, subscribe.ErrInvalidContent):
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
		default:
			a.deps.Log.Debug("source-list failed", "err", err)
			httpx.WriteError(w, http.StatusBadGateway, "订阅拉取失败")
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, payload)
}
