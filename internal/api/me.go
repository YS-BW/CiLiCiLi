package api

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cilicili/internal/httpx"
	"cilicili/internal/userdata"
)

const (
	maxKeyLen      = 128
	maxTextLen     = 512
	maxCoverLen    = 2048
	maxEpisodeName = 256
	maxHistory     = 200
)

type progressRequest struct {
	SourceKey    string  `json:"sourceKey"`
	SourceName   string  `json:"sourceName"`
	VodID        string  `json:"vodId"`
	Title        string  `json:"title"`
	Cover        string  `json:"cover"`
	LineID       string  `json:"lineId"`
	LineName     string  `json:"lineName"`
	EpisodeID    string  `json:"episodeId"`
	EpisodeName  string  `json:"episodeName"`
	EpisodeIndex int     `json:"episodeIndex"`
	Position     float64 `json:"position"`
	Duration     float64 `json:"duration"`
	Completed    bool    `json:"completed"`
}

type favoriteRequest struct {
	SourceKey  string `json:"sourceKey"`
	SourceName string `json:"sourceName"`
	VodID      string `json:"vodId"`
	Title      string `json:"title"`
	Cover      string `json:"cover"`
}

// PutProgress 保存观看进度。影片身份由 sourceKey + vodId 决定；同一影片覆盖旧进度。
func (a *API) PutProgress(w http.ResponseWriter, r *http.Request) {
	if a.deps.UserData == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "用户数据存储未启用")
		return
	}
	var body progressRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if !validKey(body.SourceKey) || !validKey(body.VodID) || !validKey(body.EpisodeID) {
		httpx.WriteError(w, http.StatusBadRequest, "sourceKey / vodId / episodeId 无效")
		return
	}
	if math.IsNaN(body.Position) || math.IsInf(body.Position, 0) ||
		math.IsNaN(body.Duration) || math.IsInf(body.Duration, 0) ||
		body.Position < 0 || body.Duration < 0 {
		httpx.WriteError(w, http.StatusBadRequest, "position / duration 无效")
		return
	}
	saved, err := a.deps.UserData.PutProgress(userdata.Progress{
		SourceKey:    strings.TrimSpace(body.SourceKey),
		SourceName:   trimText(body.SourceName, maxKeyLen),
		VodID:        strings.TrimSpace(body.VodID),
		Title:        trimText(body.Title, maxTextLen),
		Cover:        trimText(body.Cover, maxCoverLen),
		LineID:       trimText(body.LineID, maxKeyLen),
		LineName:     trimText(body.LineName, maxKeyLen),
		EpisodeID:    strings.TrimSpace(body.EpisodeID),
		EpisodeName:  trimText(body.EpisodeName, maxEpisodeName),
		EpisodeIndex: body.EpisodeIndex,
		Position:     body.Position,
		Duration:     body.Duration,
		Completed:    body.Completed,
	}, time.Now())
	if err != nil {
		a.deps.Log.Error("save progress failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "保存进度失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, saved)
}

// History 返回观看历史；?unfinished=1 只看未看完的，?limit=50 限制条数。
func (a *API) History(w http.ResponseWriter, r *http.Request) {
	if a.deps.UserData == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "用户数据存储未启用")
		return
	}
	q := r.URL.Query()
	unfinished := q.Get("unfinished") == "1" || strings.EqualFold(q.Get("unfinished"), "true")
	limit := parseLimit(q.Get("limit"), 50, maxHistory)
	items, err := a.deps.UserData.History(unfinished, limit)
	if err != nil {
		a.deps.Log.Error("list history failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "读取历史失败")
		return
	}
	if items == nil {
		items = []userdata.Progress{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// DeleteHistory 删除一条观看历史。
func (a *API) DeleteHistory(w http.ResponseWriter, r *http.Request) {
	if a.deps.UserData == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "用户数据存储未启用")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 2*maxKeyLen+1 {
		httpx.WriteError(w, http.StatusBadRequest, "无效的 id")
		return
	}
	deleted, err := a.deps.UserData.DeleteProgress(id)
	if err != nil {
		a.deps.Log.Error("delete history failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "删除历史失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": deleted})
}

// Favorites 返回收藏列表。
func (a *API) Favorites(w http.ResponseWriter, r *http.Request) {
	if a.deps.UserData == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "用户数据存储未启用")
		return
	}
	limit := parseLimit(r.URL.Query().Get("limit"), 100, maxHistory)
	items, err := a.deps.UserData.Favorites(limit)
	if err != nil {
		a.deps.Log.Error("list favorites failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "读取收藏失败")
		return
	}
	if items == nil {
		items = []userdata.Favorite{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// PutFavorite 收藏影片（保存标题/封面快照）。
func (a *API) PutFavorite(w http.ResponseWriter, r *http.Request) {
	if a.deps.UserData == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "用户数据存储未启用")
		return
	}
	var body favoriteRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if !validKey(body.SourceKey) || !validKey(body.VodID) {
		httpx.WriteError(w, http.StatusBadRequest, "sourceKey / vodId 无效")
		return
	}
	saved, err := a.deps.UserData.PutFavorite(userdata.Favorite{
		SourceKey:  strings.TrimSpace(body.SourceKey),
		SourceName: trimText(body.SourceName, maxKeyLen),
		VodID:      strings.TrimSpace(body.VodID),
		Title:      trimText(body.Title, maxTextLen),
		Cover:      trimText(body.Cover, maxCoverLen),
	}, time.Now())
	if err != nil {
		a.deps.Log.Error("save favorite failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "保存收藏失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, saved)
}

// DeleteFavorite 取消收藏。
func (a *API) DeleteFavorite(w http.ResponseWriter, r *http.Request) {
	if a.deps.UserData == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "用户数据存储未启用")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 2*maxKeyLen+1 {
		httpx.WriteError(w, http.StatusBadRequest, "无效的 id")
		return
	}
	deleted, err := a.deps.UserData.DeleteFavorite(id)
	if err != nil {
		a.deps.Log.Error("delete favorite failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "取消收藏失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": deleted})
}

func validKey(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxKeyLen {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func trimText(s string, max int) string {
	s = strings.TrimSpace(s)
	if max > 0 && len([]rune(s)) > max {
		s = string([]rune(s)[:max])
	}
	return s
}

func parseLimit(raw string, def, max int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return def
	}
	if max > 0 && n > max {
		return max
	}
	return n
}
