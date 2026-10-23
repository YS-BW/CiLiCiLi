package api

import (
	"net/http"
	"regexp"
	"strings"

	"cilicili/internal/httpx"
	"cilicili/internal/model"
)

var qualityIDRe = regexp.MustCompile(`^[\w-]{1,64}$`)

type qualityRequest struct {
	Items []model.QualityProbeItem `json:"items"`
}

func validQualityItem(it model.QualityProbeItem) bool {
	u := strings.TrimSpace(it.Source.URL)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return false
	}
	return qualityIDRe.MatchString(strings.TrimSpace(it.ID))
}

// Quality 批量画质探测；?stream=1 时 NDJSON 逐条推送。
func (a *API) Quality(w http.ResponseWriter, r *http.Request) {
	if a.deps.Quality == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "画质探测尚未启用")
		return
	}
	var body qualityRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	items := make([]model.QualityProbeItem, 0, len(body.Items))
	for _, it := range body.Items {
		if validQualityItem(it) {
			items = append(items, it)
		}
		if len(items) >= 50 {
			break
		}
	}
	if len(items) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "条目格式无效")
		return
	}

	if r.URL.Query().Get("stream") == "1" {
		nw := httpx.NewNDJSONWriter(w)
		// 首字节心跳：客户端据此区分“请求被网关拦下”与“探测本身失败”。
		if err := nw.Write(map[string]bool{"ping": true}); err != nil {
			return
		}
		a.deps.Quality.ProbeMany(r.Context(), items, func(res model.QualityProbeResult) error {
			return nw.Write(res)
		})
		return
	}

	results := make([]model.QualityProbeResult, 0, len(items))
	a.deps.Quality.ProbeMany(r.Context(), items, func(res model.QualityProbeResult) error {
		results = append(results, res)
		return nil
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": results})
}
