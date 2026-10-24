package api

import (
	"net/http"
	"time"

	"cilicili/internal/discover"
	"cilicili/internal/httpx"
	"cilicili/internal/sources"
)

// SourcesProbe 管理源探活：GET 返回状态，POST 修改间隔或立即触发。
func (a *API) SourcesProbe(w http.ResponseWriter, r *http.Request) {
	if a.deps.Prober == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "探活未启用")
		return
	}
	if r.Method == http.MethodGet {
		httpx.WriteJSON(w, http.StatusOK, a.deps.Prober.Status())
		return
	}
	var body struct {
		IntervalHours *int  `json:"intervalHours"`
		Run           *bool `json:"run"`
	}
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.IntervalHours != nil {
		hours := *body.IntervalHours
		if hours < 1 || hours > 168 {
			httpx.WriteError(w, http.StatusBadRequest, "探活间隔需在 1–168 小时之间")
			return
		}
		if err := a.deps.Prober.SetInterval(time.Duration(hours) * time.Hour); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "保存探活间隔失败")
			return
		}
	}
	if body.Run != nil && *body.Run {
		a.deps.Prober.Trigger()
	}
	httpx.WriteJSON(w, http.StatusOK, a.deps.Prober.Status())
}

// SourcesDiscover 扫描发现渠道，探测并返回候选源。
func (a *API) SourcesDiscover(w http.ResponseWriter, r *http.Request) {
	if a.deps.Discover == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "自动发现未启用")
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	result := a.deps.Discover.Run(r.Context(), refresh)
	httpx.WriteJSON(w, http.StatusOK, result)
}

// SourcesDiscoverApply 应用发现结果（只添加存活且不重复的源）。
func (a *API) SourcesDiscoverApply(w http.ResponseWriter, r *http.Request) {
	if a.deps.Discover == nil || a.deps.Sources == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "自动发现未启用")
		return
	}
	var body struct {
		Sources []discover.Candidate `json:"sources"`
	}
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(body.Sources) == 0 || len(body.Sources) > 200 {
		httpx.WriteError(w, http.StatusBadRequest, "候选列表无效")
		return
	}
	// 只接受最近一次发现结果里标记为存活的候选，避免把死链写进源库。
	alive := map[string]bool{}
	if cached := a.deps.Discover.Cached(); cached != nil {
		for _, c := range cached.Candidates {
			if c.Alive {
				alive[c.URL] = true
			}
		}
	}
	filtered := make([]discover.Candidate, 0, len(body.Sources))
	skippedDead := 0
	for _, c := range body.Sources {
		norm, err := sources.NormalizeURL(c.URL)
		if err != nil {
			skippedDead++
			continue
		}
		if !alive[norm] {
			skippedDead++
			continue
		}
		filtered = append(filtered, discover.Candidate{Name: c.Name, URL: norm})
	}
	added, skipped, err := a.deps.Discover.Apply(r.Context(), filtered)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "写入源失败")
		return
	}
	a.syncSources()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"added":       added,
		"skipped":     skipped + skippedDead,
		"skippedDead": skippedDead,
	})
}
