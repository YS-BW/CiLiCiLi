package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"cilicili/internal/httpx"
	"cilicili/internal/model"
	"cilicili/internal/sources"
)

// syncSources 把持久化里启用中的源热替换进 Registry（无需重启）。
func (a *API) syncSources() {
	if a.deps.Sources == nil || a.deps.Registry == nil {
		return
	}
	list, err := a.deps.Sources.ListEnabled()
	if err != nil {
		a.deps.Log.Error("同步源配置失败", "err", err)
		return
	}
	a.deps.Registry.Replace(list)
}

func writeSourceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sources.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "源不存在")
	case errors.Is(err, sources.ErrConflict):
		httpx.WriteError(w, http.StatusConflict, "源已存在（key 或地址重复）")
	case errors.Is(err, sources.ErrInvalidURL):
		httpx.WriteError(w, http.StatusBadRequest, "无效的源地址")
	default:
		httpx.WriteError(w, http.StatusInternalServerError, "源配置操作失败")
	}
}

type sourceCreateRequest struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Detail  string `json:"detail"`
	IsAdult bool   `json:"isAdult"`
	Enabled *bool  `json:"enabled"`
}

// SourceCreate 新增点播源。
func (a *API) SourceCreate(w http.ResponseWriter, r *http.Request) {
	if a.deps.Sources == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "源管理未启用")
		return
	}
	var body sourceCreateRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len([]rune(name)) > 64 {
		httpx.WriteError(w, http.StatusBadRequest, "源名称无效")
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	src, err := a.deps.Sources.CreateWithEnabled(sources.Source{
		SourceConfig: model.SourceConfig{
			Key:     strings.TrimSpace(body.Key),
			Name:    name,
			URL:     body.URL,
			Detail:  body.Detail,
			IsAdult: body.IsAdult,
		},
	}, enabled)
	if err != nil {
		writeSourceError(w, err)
		return
	}
	a.syncSources()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"source": src})
}

type sourceUpdateRequest struct {
	Name    *string `json:"name"`
	URL     *string `json:"url"`
	Detail  *string `json:"detail"`
	IsAdult *bool   `json:"isAdult"`
	Enabled *bool   `json:"enabled"`
}

// SourceUpdate 更新点播源。
func (a *API) SourceUpdate(w http.ResponseWriter, r *http.Request) {
	if a.deps.Sources == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "源管理未启用")
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		httpx.WriteError(w, http.StatusBadRequest, "缺少 key")
		return
	}
	var body sourceUpdateRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" || len([]rune(name)) > 64 {
			httpx.WriteError(w, http.StatusBadRequest, "源名称无效")
			return
		}
		body.Name = &name
	}
	src, err := a.deps.Sources.Update(key, sources.SourcePatch{
		Name: body.Name, URL: body.URL, Detail: body.Detail, IsAdult: body.IsAdult, Enabled: body.Enabled,
	})
	if err != nil {
		writeSourceError(w, err)
		return
	}
	a.syncSources()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"source": src})
}

// SourceDelete 删除点播源。
func (a *API) SourceDelete(w http.ResponseWriter, r *http.Request) {
	if a.deps.Sources == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "源管理未启用")
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	if err := a.deps.Sources.Delete(key); err != nil {
		writeSourceError(w, err)
		return
	}
	a.syncSources()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}

type sourceReorderRequest struct {
	Keys []string `json:"keys"`
}

// SourceReorder 调整源顺序。
func (a *API) SourceReorder(w http.ResponseWriter, r *http.Request) {
	if a.deps.Sources == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "源管理未启用")
		return
	}
	var body sourceReorderRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(body.Keys) == 0 || len(body.Keys) > 200 {
		httpx.WriteError(w, http.StatusBadRequest, "排序列表无效")
		return
	}
	if err := a.deps.Sources.Reorder(body.Keys); err != nil {
		writeSourceError(w, err)
		return
	}
	a.syncSources()
	list, _ := a.deps.Sources.List()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sources": list})
}

// ---- 订阅 ----

// Subscriptions 返回全部订阅。
func (a *API) Subscriptions(w http.ResponseWriter, r *http.Request) {
	if a.deps.Sources == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "源管理未启用")
		return
	}
	list, err := a.deps.Sources.ListSubscriptions()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "读取订阅失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": list})
}

type subscriptionCreateRequest struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// SubscriptionCreate 新增订阅。
func (a *API) SubscriptionCreate(w http.ResponseWriter, r *http.Request) {
	if a.deps.Sources == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "源管理未启用")
		return
	}
	var body subscriptionCreateRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	sub, err := a.deps.Sources.CreateSubscription(body.Name, body.URL)
	if err != nil {
		writeSourceError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"subscription": sub})
}

type subscriptionUpdateRequest struct {
	Name    *string `json:"name"`
	URL     *string `json:"url"`
	Enabled *bool   `json:"enabled"`
}

// SubscriptionUpdate 更新订阅。
func (a *API) SubscriptionUpdate(w http.ResponseWriter, r *http.Request) {
	if a.deps.Sources == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "源管理未启用")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "无效的订阅 ID")
		return
	}
	var body subscriptionUpdateRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	sub, err := a.deps.Sources.UpdateSubscription(id, body.Name, body.URL, body.Enabled)
	if err != nil {
		writeSourceError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"subscription": sub})
}

// SubscriptionDelete 删除订阅。
func (a *API) SubscriptionDelete(w http.ResponseWriter, r *http.Request) {
	if a.deps.Sources == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "源管理未启用")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "无效的订阅 ID")
		return
	}
	if err := a.deps.Sources.DeleteSubscription(id); err != nil {
		writeSourceError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}

// SubscriptionRefresh 拉取并解析订阅，返回预览（不写入源）。
func (a *API) SubscriptionRefresh(w http.ResponseWriter, r *http.Request) {
	if a.deps.Sources == nil || a.deps.Subscribe == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "订阅未启用")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "无效的订阅 ID")
		return
	}
	sub, err := a.deps.Sources.GetSubscription(id)
	if err != nil {
		writeSourceError(w, err)
		return
	}
	payload, err := a.deps.Subscribe.Fetch(r.Context(), sub.URL)
	if err != nil {
		_ = a.deps.Sources.SetSubscriptionResult(id, "failed", err.Error(), 0)
		a.deps.Log.Debug("subscription refresh failed", "id", id, "err", err)
		httpx.WriteError(w, http.StatusBadGateway, "订阅拉取失败")
		return
	}
	_ = a.deps.Sources.SetSubscriptionResult(id, "ok", "", len(payload.Sources))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"subscription": sub,
		"preview":      payload,
	})
}

type subscriptionApplyRequest struct {
	Sources []model.SourceConfig `json:"sources"`
}

// SubscriptionApply 把预览里的源合并进源列表（按 URL 去重）。
func (a *API) SubscriptionApply(w http.ResponseWriter, r *http.Request) {
	if a.deps.Sources == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "源管理未启用")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "无效的订阅 ID")
		return
	}
	sub, err := a.deps.Sources.GetSubscription(id)
	if err != nil {
		writeSourceError(w, err)
		return
	}
	var body subscriptionApplyRequest
	// apply 允许空 body（直接应用整份订阅），空体不是错误。
	if err := httpx.ReadJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	items := body.Sources
	if len(items) == 0 {
		if a.deps.Subscribe == nil {
			httpx.WriteError(w, http.StatusBadRequest, "没有可导入的源")
			return
		}
		payload, err := a.deps.Subscribe.Fetch(r.Context(), sub.URL)
		if err != nil {
			_ = a.deps.Sources.SetSubscriptionResult(id, "failed", err.Error(), 0)
			httpx.WriteError(w, http.StatusBadGateway, "订阅拉取失败")
			return
		}
		items = payload.Sources
		_ = a.deps.Sources.SetSubscriptionResult(id, "ok", "", len(payload.Sources))
	}
	if len(items) > 300 {
		items = items[:300]
	}

	existing, err := a.deps.Sources.List()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "读取源失败")
		return
	}
	byURL := make(map[string]bool, len(existing))
	for _, src := range existing {
		byURL[src.URL] = true
	}

	added, skipped := 0, 0
	applied := make([]sources.Source, 0, len(items))
	for _, item := range items {
		norm, err := sources.NormalizeURL(item.URL)
		if err != nil {
			skipped++
			continue
		}
		if byURL[norm] {
			skipped++
			continue
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = norm
		}
		key := strings.TrimSpace(item.Key)
		if key != "" {
			if _, getErr := a.deps.Sources.Get(key); getErr == nil {
				key = "" // key 冲突时让 store 按 URL 生成
			}
		}
		src, err := a.deps.Sources.Create(sources.Source{
			SourceConfig: model.SourceConfig{Key: key, Name: name, URL: norm, Detail: item.Detail, IsAdult: item.IsAdult},
		})
		if err != nil {
			skipped++
			continue
		}
		byURL[src.URL] = true
		added++
		applied = append(applied, *src)
	}
	a.syncSources()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"added":   added,
		"skipped": skipped,
		"items":   applied,
	})
}
