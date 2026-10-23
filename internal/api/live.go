package api

import (
	"errors"
	"net/http"
	"strings"

	"cilicili/internal/httpx"
	"cilicili/internal/live"
	"cilicili/internal/model"
)

// LivePlaylist 拉取并解析 M3U 直播订阅。
func (a *API) LivePlaylist(w http.ResponseWriter, r *http.Request) {
	if a.deps.Live == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "直播服务尚未启用")
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("url"))
	if raw == "" {
		httpx.WriteError(w, http.StatusBadRequest, "缺少 url 参数")
		return
	}
	force := r.URL.Query().Get("force") == "1"
	res, err := a.deps.Live.Playlist(r.Context(), raw, force)
	if err != nil {
		if errors.Is(err, live.ErrInvalidURL) || errors.Is(err, live.ErrInvalidPlaylist) {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.deps.Log.Debug("live playlist failed", "err", err)
		httpx.WriteError(w, http.StatusBadGateway, "直播列表拉取失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// LiveEPG 查询单频道节目单。
func (a *API) LiveEPG(w http.ResponseWriter, r *http.Request) {
	if a.deps.Live == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "直播服务尚未启用")
		return
	}
	q := r.URL.Query()
	raw := strings.TrimSpace(q.Get("url"))
	channel := strings.TrimSpace(q.Get("channel"))
	if raw == "" {
		httpx.WriteError(w, http.StatusBadRequest, "缺少 url 参数")
		return
	}
	if channel == "" {
		httpx.WriteError(w, http.StatusBadRequest, "缺少 channel 参数")
		return
	}
	force := q.Get("force") == "1"
	res, err := a.deps.Live.EPG(r.Context(), raw, channel, force)
	if err != nil {
		if errors.Is(err, live.ErrInvalidURL) || errors.Is(err, live.ErrInvalidXMLTV) {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.deps.Log.Debug("live epg failed", "err", err)
		httpx.WriteError(w, http.StatusBadGateway, "节目单拉取失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

type liveProbeRequest struct {
	URLs []string `json:"urls"`
}

// LiveProbe 批量测活；?stream=1 时 NDJSON 逐条推送。
func (a *API) LiveProbe(w http.ResponseWriter, r *http.Request) {
	if a.deps.Live == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "直播服务尚未启用")
		return
	}
	var body liveProbeRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	seen := make(map[string]struct{}, len(body.URLs))
	urls := make([]string, 0, len(body.URLs))
	for _, u := range body.URLs {
		u = strings.TrimSpace(u)
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
		if len(urls) >= 50 {
			break
		}
	}
	if len(urls) == 0 {
		if r.URL.Query().Get("stream") == "1" {
			httpx.NewNDJSONWriter(w)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": []model.LiveProbeResult{}})
		return
	}

	if r.URL.Query().Get("stream") == "1" {
		nw := httpx.NewNDJSONWriter(w)
		a.deps.Live.Probe(r.Context(), urls, func(res model.LiveProbeResult) error {
			return nw.Write(res)
		})
		return
	}

	byURL := make(map[string]model.LiveProbeResult, len(urls))
	a.deps.Live.Probe(r.Context(), urls, func(res model.LiveProbeResult) error {
		byURL[res.URL] = res
		return nil
	})
	results := make([]model.LiveProbeResult, 0, len(urls))
	for _, u := range urls {
		results = append(results, byURL[u])
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": results})
}
