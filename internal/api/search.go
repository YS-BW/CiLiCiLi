package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cilicili/internal/detail"
	"cilicili/internal/httpx"
	"cilicili/internal/media"
	"cilicili/internal/model"
)

type searchRequest struct {
	WD          string               `json:"wd"`
	Sources     []model.SourceConfig `json:"sources"`
	FilterAdult *bool                `json:"filterAdult"`
	// Fast 默认 true：只查服务端健康度最快的 N 个源；false 查全部源。
	Fast *bool `json:"fast"`
	// IncludeMedia 为 true 时额外返回聚合后的媒体卡片（media 字段）。
	IncludeMedia bool `json:"includeMedia"`
}

type searchResponseWithMedia struct {
	model.SearchResponse
	Media []media.SearchMedia `json:"media,omitempty"`
}

type doneEventWithMedia struct {
	model.DoneEvent
	Media []media.SearchMedia `json:"media,omitempty"`
}

// Search 处理聚合搜索；?stream=1 时返回 NDJSON。
func (a *API) Search(w http.ResponseWriter, r *http.Request) {
	var body searchRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	wd := strings.TrimSpace(body.WD)
	if wd == "" || len([]rune(wd)) > 100 {
		httpx.WriteError(w, http.StatusBadRequest, "搜索关键词无效")
		return
	}
	sources := a.deps.Registry.Resolve(body.Sources)
	if len(sources) == 0 {
		sources = a.deps.Registry.List()
	}
	valid := make([]model.SourceConfig, 0, len(sources))
	for _, s := range sources {
		if strings.HasPrefix(s.URL, "http://") || strings.HasPrefix(s.URL, "https://") {
			valid = append(valid, s)
		}
	}
	if len(valid) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "请至少选择一个点播源")
		return
	}
	if len(valid) > 50 {
		httpx.WriteError(w, http.StatusBadRequest, "点播源数量过多")
		return
	}
	filterAdult := true
	if body.FilterAdult != nil {
		filterAdult = *body.FilterAdult
	}
	fast := true
	if body.Fast != nil {
		fast = *body.Fast
	}
	total := len(valid)
	if fast {
		valid = a.deps.Searcher.SelectFast(valid, a.deps.Cfg.SearchFastSources)
	}
	w.Header().Set("X-Search-Sources", fmt.Sprintf("%d/%d", len(valid), total))

	if r.URL.Query().Get("stream") != "1" {
		resp, err := a.deps.Searcher.Search(r.Context(), wd, valid, filterAdult)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if body.IncludeMedia && a.deps.Media != nil {
			if cards, mErr := a.deps.Media.SearchCards(resp.List, time.Now()); mErr == nil {
				httpx.WriteJSON(w, http.StatusOK, searchResponseWithMedia{SearchResponse: resp, Media: cards})
				return
			} else {
				a.deps.Log.Warn("media aggregate failed", "err", mErr)
			}
		}
		httpx.WriteJSON(w, http.StatusOK, resp)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	nw := httpx.NewNDJSONWriter(w)
	emit := func(ev any) error {
		if body.IncludeMedia && a.deps.Media != nil {
			if done, ok := ev.(model.DoneEvent); ok {
				if cards, mErr := a.deps.Media.SearchCards(done.List, time.Now()); mErr == nil {
					return nw.Write(doneEventWithMedia{DoneEvent: done, Media: cards})
				} else {
					a.deps.Log.Warn("media aggregate failed", "err", mErr)
				}
			}
		}
		return nw.Write(ev)
	}
	if _, err := a.deps.Searcher.Stream(r.Context(), wd, valid, filterAdult, emit); err != nil && !errors.Is(err, r.Context().Err()) {
		a.deps.Log.Debug("search stream ended", "error", err)
	}
}

// Detail 处理影片详情。
func (a *API) Detail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := strings.TrimSpace(q.Get("id"))
	src, ok := a.parseSource(q.Get("source"), q.Get("sourceKey"))
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, detail.ErrInvalidSource.Error())
		return
	}
	d, err := a.deps.Detail.Resolve(r.Context(), id, src)
	if err != nil {
		switch {
		case errors.Is(err, detail.ErrInvalidID), errors.Is(err, detail.ErrInvalidSource):
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, detail.ErrNotFound):
			httpx.WriteError(w, http.StatusNotFound, err.Error())
		default:
			// 不把上游 URL / 内网 IP 等细节回显给客户端。
			a.deps.Log.Debug("detail resolve failed", "id", id, "source", src.Key, "err", err)
			httpx.WriteError(w, http.StatusBadGateway, "详情获取失败，请稍后重试")
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (a *API) parseSource(raw, key string) (model.SourceConfig, bool) {
	var src model.SourceConfig
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &src); err != nil {
			return model.SourceConfig{}, false
		}
	}
	if key != "" {
		if canonical, ok := a.deps.Registry.Get(key); ok {
			src = canonical
		}
	}
	if src.URL == "" || (!strings.HasPrefix(src.URL, "http://") && !strings.HasPrefix(src.URL, "https://")) {
		return model.SourceConfig{}, false
	}
	if src.Key == "" {
		src.Key = src.URL
	}
	if src.Name == "" {
		src.Name = src.Key
	}
	return src, true
}
