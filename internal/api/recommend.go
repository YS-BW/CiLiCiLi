package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"cilicili/internal/httpx"
	"cilicili/internal/recommend"
)

// Douban 返回豆瓣推荐。
func (a *API) Douban(w http.ResponseWriter, r *http.Request) {
	if a.deps.Recommend == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "推荐服务尚未启用")
		return
	}
	q := r.URL.Query()
	typ := "movie"
	if q.Get("type") == "tv" {
		typ = "tv"
	}
	tag := strings.TrimSpace(q.Get("tag"))
	if tag == "" {
		tag = "热门"
	}
	if len([]rune(tag)) > 20 {
		tag = string([]rune(tag)[:20])
	}
	pageStart := 0
	if n, err := strconv.Atoi(q.Get("pageStart")); err == nil && n > 0 {
		pageStart = n
	}
	pageSize := 25
	if n, err := strconv.Atoi(q.Get("pageSize")); err == nil && n > 0 {
		pageSize = n
	}
	if pageSize > 50 {
		pageSize = 50
	}
	items, err := a.deps.Recommend.Douban(r.Context(), typ, tag, pageStart, pageSize)
	if err != nil {
		a.deps.Log.Debug("douban failed", "err", err)
		httpx.WriteError(w, http.StatusBadGateway, "豆瓣推荐获取失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// HotList 返回影视榜单。
func (a *API) HotList(w http.ResponseWriter, r *http.Request) {
	if a.deps.Recommend == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "推荐服务尚未启用")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	items, err := a.deps.Recommend.HotList(r.Context(), id)
	if err != nil {
		if errors.Is(err, recommend.ErrUnknownHotList) {
			httpx.WriteError(w, http.StatusBadRequest, "未知榜单")
			return
		}
		a.deps.Log.Debug("hot list failed", "err", err)
		httpx.WriteError(w, http.StatusBadGateway, "榜单获取失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
