package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cilicili/internal/httpx"
	"cilicili/internal/media"
	"cilicili/internal/model"
)

type mediaResolveRequest struct {
	SourceKey string                   `json:"sourceKey"`
	VodID     string                   `json:"vodId"`
	Items     []model.SearchResultItem `json:"items"`
}

type mediaMatchRequest struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"externalId"`
	Action     string `json:"action"`
}

type mediaCoverRequest struct {
	URL string `json:"url"`
}

// MediaResolve 批量把采集站条目聚合为媒体卡片；也支持单个 sourceKey+vodId。
func (a *API) MediaResolve(w http.ResponseWriter, r *http.Request) {
	if a.deps.Media == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "媒体库未启用")
		return
	}
	var body mediaResolveRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(body.Items) > 0 {
		items := body.Items
		if len(items) > 100 {
			items = items[:100]
		}
		cards, err := a.deps.Media.SearchCards(items, time.Now())
		if err != nil {
			a.deps.Log.Error("media resolve failed", "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, "聚合失败")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": cards})
		return
	}
	if strings.TrimSpace(body.SourceKey) == "" || strings.TrimSpace(body.VodID) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "缺少 sourceKey / vodId")
		return
	}
	m, err := a.deps.Media.Resolve(r.Context(), body.SourceKey, body.VodID)
	if err != nil {
		a.deps.Log.Error("media resolve failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "查询失败")
		return
	}
	if m == nil {
		httpx.WriteError(w, http.StatusNotFound, "媒体不存在")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

// MediaGet 返回统一媒体实体。
func (a *API) MediaGet(w http.ResponseWriter, r *http.Request) {
	if a.deps.Media == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "媒体库未启用")
		return
	}
	m, err := a.deps.Media.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.deps.Log.Error("media get failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "查询失败")
		return
	}
	if m == nil {
		httpx.WriteError(w, http.StatusNotFound, "媒体不存在")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

// MediaCandidates 返回候选；?refresh=1 重新排队刮削。
func (a *API) MediaCandidates(w http.ResponseWriter, r *http.Request) {
	if a.deps.Media == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "媒体库未启用")
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	cs, err := a.deps.Media.Candidates(r.Context(), r.PathValue("id"), refresh)
	if err != nil {
		a.deps.Log.Error("media candidates failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "读取候选失败")
		return
	}
	if cs == nil {
		cs = []media.Candidate{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": cs})
}

// MediaMatch 人工确认/拒绝候选。
func (a *API) MediaMatch(w http.ResponseWriter, r *http.Request) {
	if a.deps.Media == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "媒体库未启用")
		return
	}
	var body mediaMatchRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	action := strings.ToLower(strings.TrimSpace(body.Action))
	switch action {
	case "confirm", "reject", "clear":
	default:
		httpx.WriteError(w, http.StatusBadRequest, "action 必须是 confirm/reject/clear")
		return
	}
	if action == "confirm" && (body.Provider == "" || body.ExternalID == "") {
		httpx.WriteError(w, http.StatusBadRequest, "confirm 需要 provider + externalId")
		return
	}
	m, err := a.deps.Media.Match(r.Context(), r.PathValue("id"), body.Provider, body.ExternalID, action)
	if err != nil {
		switch {
		case errors.Is(err, media.ErrMediaNotFound):
			httpx.WriteError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, media.ErrCandidateNotFound):
			httpx.WriteError(w, http.StatusNotFound, err.Error())
		default:
			a.deps.Log.Error("media match failed", "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, "匹配失败")
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

// MediaSetCover 人工设置封面 URL（服务端下载缓存）。
func (a *API) MediaSetCover(w http.ResponseWriter, r *http.Request) {
	if a.deps.Media == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "媒体库未启用")
		return
	}
	var body mediaCoverRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	raw := strings.TrimSpace(body.URL)
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		httpx.WriteError(w, http.StatusBadRequest, "封面地址无效")
		return
	}
	m, err := a.deps.Media.SetCover(r.Context(), r.PathValue("id"), raw)
	if err != nil {
		switch {
		case errors.Is(err, media.ErrMediaNotFound):
			httpx.WriteError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, media.ErrCoverType), errors.Is(err, media.ErrCoverTooLarge):
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
		default:
			a.deps.Log.Debug("set cover failed", "err", err)
			httpx.WriteError(w, http.StatusBadGateway, "封面下载/校验失败")
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

// MediaCover 返回本地缓存的封面图片；没有缓存时 404（前端显示占位图）。
func (a *API) MediaCover(w http.ResponseWriter, r *http.Request) {
	if a.deps.Media == nil {
		http.NotFound(w, r)
		return
	}
	path, err := a.deps.Media.CoverPath(r.PathValue("id"))
	if err != nil || path == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

// MediaRefresh 重新排队刮削。
func (a *API) MediaRefresh(w http.ResponseWriter, r *http.Request) {
	if a.deps.Media == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "媒体库未启用")
		return
	}
	if err := a.deps.Media.Refresh(r.Context(), r.PathValue("id")); err != nil {
		a.deps.Log.Error("media refresh failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "刷新失败")
		return
	}
	m, err := a.deps.Media.Get(r.Context(), r.PathValue("id"))
	if err != nil || m == nil {
		httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"queued": true})
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"queued": true, "media": m})
}
