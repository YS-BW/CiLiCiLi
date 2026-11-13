package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"cilicili/internal/httpx"
	"cilicili/internal/latency"
	"cilicili/internal/model"
)

const (
	maxLatencyItems    = 20
	latencyConcurrency = 4
	// latencyItemTimeout 覆盖「解析片源详情 + 测一次播放列表」的总预算。
	latencyItemTimeout = 5 * time.Second
)

type latencyRequestItem struct {
	Key       string `json:"key"`
	URL       string `json:"url"`
	SourceKey string `json:"sourceKey"`
	VodID     string `json:"vodId"`
}

type latencyRequest struct {
	Items []latencyRequestItem `json:"items"`
}

type latencyResult struct {
	Key string `json:"key"`
	latency.Measurement
}

// Latency 批量测量播放列表延迟。
//
// items 可以直接给播放地址（url），也可以给 sourceKey+vodId：
// 后者会先解析该片源的详情，再选第一条可用 m3u8 地址测量。所有条目并发执行，
// 单条有独立超时，结果按请求顺序返回。
func (a *API) Latency(w http.ResponseWriter, r *http.Request) {
	if a.deps.Latency == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "延迟探测尚未启用")
		return
	}
	var body latencyRequest
	if err := httpx.ReadJSON(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(body.Items) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "缺少探测条目")
		return
	}
	if len(body.Items) > maxLatencyItems {
		body.Items = body.Items[:maxLatencyItems]
	}

	results := make([]latencyResult, len(body.Items))
	sem := make(chan struct{}, latencyConcurrency)
	var wg sync.WaitGroup
	for i, item := range body.Items {
		wg.Add(1)
		go func(i int, item latencyRequestItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(r.Context(), latencyItemTimeout)
			defer cancel()

			result := latencyResult{Key: strings.TrimSpace(item.Key)}
			raw := strings.TrimSpace(item.URL)
			if raw == "" {
				var resolveErr string
				raw, resolveErr = a.resolveLatencyURL(ctx, item.SourceKey, item.VodID)
				if raw == "" {
					result.Error = resolveErr
					result.TimedOut = resolveErr == "详情获取超时"
					results[i] = result
					return
				}
			}
			result.Measurement = a.deps.Latency.Measure(ctx, raw)
			results[i] = result
		}(i, item)
	}
	wg.Wait()

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": results})
}

// resolveLatencyURL 把 sourceKey+vodId 解析成一条可测的播放地址。
// 上游详情解析可能较慢，用 goroutine + ctx 兜住等待时间；解析本身仍会继续，
// 后续请求可命中详情缓存。
func (a *API) resolveLatencyURL(ctx context.Context, sourceKey, vodID string) (string, string) {
	sourceKey = strings.TrimSpace(sourceKey)
	vodID = strings.TrimSpace(vodID)
	if sourceKey == "" || vodID == "" {
		return "", "缺少播放地址"
	}
	if a.deps.Registry == nil || a.deps.Detail == nil {
		return "", "延迟探测未配置"
	}
	src, ok := a.deps.Registry.Get(sourceKey)
	if !ok {
		return "", "片源不存在"
	}

	type resolved struct {
		url string
		err error
	}
	ch := make(chan resolved, 1)
	go func() {
		d, err := a.deps.Detail.Resolve(ctx, vodID, src)
		if err != nil {
			ch <- resolved{err: err}
			return
		}
		ch <- resolved{url: firstPlaylistURL(d)}
	}()

	select {
	case <-ctx.Done():
		return "", "详情获取超时"
	case res := <-ch:
		if res.err != nil {
			return "", "详情获取失败"
		}
		if res.url == "" {
			return "", "未找到播放地址"
		}
		return res.url, ""
	}
}

// firstPlaylistURL 优先选推荐线路里的 m3u8，其次任意线路的 m3u8，最后退回首集。
func firstPlaylistURL(d *model.VideoDetail) string {
	if d == nil || len(d.PlayLines) == 0 {
		return ""
	}
	ordered := make([]model.PlayLine, 0, len(d.PlayLines))
	for _, line := range d.PlayLines {
		if line.Preferred {
			ordered = append(ordered, line)
		}
	}
	for _, line := range d.PlayLines {
		if !line.Preferred {
			ordered = append(ordered, line)
		}
	}
	for _, line := range ordered {
		for _, ep := range line.Episodes {
			if isM3U8(ep.URL) {
				return ep.URL
			}
		}
	}
	for _, line := range ordered {
		if len(line.Episodes) > 0 {
			return line.Episodes[0].URL
		}
	}
	return ""
}

func isM3U8(raw string) bool {
	return strings.Contains(strings.ToLower(raw), ".m3u8")
}
