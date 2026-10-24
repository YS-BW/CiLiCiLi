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
		// 只带 sourceKey+vodId 的条目（收藏/历史）不重新建媒体，直接查已有聚合，
		// 避免客户端为了换源把整条目录再搜一遍。
		lookupOnly := true
		for _, it := range items {
			if strings.TrimSpace(it.Name) != "" {
				lookupOnly = false
				break
			}
		}
		if lookupOnly {
			cards := make([]media.SearchMedia, 0, len(items))
			for _, it := range items {
				m, err := a.deps.Media.Resolve(r.Context(), it.SourceKey, it.VodID)
				if err != nil || m == nil {
					continue
				}
				cover := m.Cover.URL
				if cover == "" {
					cover = m.Metadata.Cover
				}
				cards = append(cards, media.SearchMedia{
					ID: m.ID, Kind: m.Kind, Title: m.Metadata.Title, Year: m.Metadata.Year,
					Cover: cover, SourceCount: m.SourceCount, MatchStatus: m.MatchStatus, Sources: m.Sources,
				})
			}
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": cards})
			return
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
	id := r.PathValue("id")
	path, err := a.deps.Media.CoverPath(id)
	if err != nil || path == "" {
		http.NotFound(w, r)
		return
	}
	// 封面按 hash 做 ETag：前端可以长缓存，重复访问直接 304，不再回源。
	if ci, _ := a.deps.Media.CoverInfo(id); ci != nil && ci.Hash != "" {
		etag := `"` + ci.Hash + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
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
	if ci, _ := a.deps.Media.CoverInfo(id); ci != nil && ci.Hash != "" {
		// URL 带 ?v=<hash>，内容变化时 URL 会变，可以安全地 immutable。
		w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
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
