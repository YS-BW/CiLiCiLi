package api

import (
	"net/http"
	"time"

	"cilicili/internal/httpx"
)

// SourcesProbe 管理源探活：GET 返回状态，POST 修改间隔或立即触发。
func (a *API) SourcesProbe(w http.ResponseWriter, r *http.Request) {
	if a.deps.Prober == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "探活未启用")
		return
	}
	if r.Method == http.MethodGet {
		payload := map[string]any{}
		st := a.deps.Prober.Status()
		payload["intervalHours"] = st.IntervalHours
		payload["running"] = st.Running
		payload["last"] = st.Last
		if a.deps.Discover != nil {
			payload["discover"] = a.deps.Discover.Status()
		}
		httpx.WriteJSON(w, http.StatusOK, payload)
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
