package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"cilicili/internal/httpx"
	"cilicili/internal/media"
	"cilicili/internal/model"
	"cilicili/internal/recommend"
)

// Backdrop 按标题查第三方横版剧照（TMDB），返回 {backdrop}。
// 供 Web 首页主推大图使用；无 provider / 查不到时 backdrop 为空串（前端回退竖版海报）。
func (a *API) Backdrop(w http.ResponseWriter, r *http.Request) {
	if a.deps.Media == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"backdrop": ""})
		return
	}
	title := strings.TrimSpace(r.URL.Query().Get("title"))
	if title == "" {
		httpx.WriteError(w, http.StatusBadRequest, "缺少 title")
		return
	}
	kind := media.KindMovie
	if r.URL.Query().Get("type") == "tv" {
		kind = media.KindTV
	}
	year := strings.TrimSpace(r.URL.Query().Get("year"))
	url, err := a.deps.Media.LookupBackdrop(r.Context(), title, year, kind)
	if err != nil {
		a.deps.Log.Debug("backdrop lookup failed", "err", err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"backdrop": url})
}

// Douban 返回豆瓣推荐。
func (a *API) Douban(w http.ResponseWriter, r *http.Request) {
	if a.deps.Recommend == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "推荐服务尚未启用")
		return
	}
	q := r.URL.Query()
	typ := "movie"
	if q.Get("type") == "tv" {
		typ = "tv"
	}
	tag := strings.TrimSpace(q.Get("tag"))
	if tag == "" {
		tag = "热门"
	}
	if len([]rune(tag)) > 20 {
		tag = string([]rune(tag)[:20])
	}
	pageStart := 0
	if n, err := strconv.Atoi(q.Get("pageStart")); err == nil && n > 0 {
		pageStart = n
	}
	pageSize := 25
	if n, err := strconv.Atoi(q.Get("pageSize")); err == nil && n > 0 {
		pageSize = n
	}
	if pageSize > 50 {
		pageSize = 50
	}
	items, err := a.deps.Recommend.Douban(r.Context(), typ, tag, pageStart, pageSize)
	if err != nil {
		a.deps.Log.Debug("douban failed", "err", err)
		httpx.WriteError(w, http.StatusBadGateway, "豆瓣推荐获取失败")
		return
	}
	// 对每个豆瓣榜单条目走一遍搜索聚合，返回与搜索页一致的 media 卡片（自带 sources/vodId），
	// 这样首页点卡片可直接进详情页，而不是跳搜索。
	cards := a.enrichDoubanMedia(r.Context(), typ, tag, pageStart, items)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": cards})
}

// doubanMediaCache 缓存“豆瓣榜单 → 聚合卡片”结果，避免每次首页都重复跑几十次搜索。
type doubanMediaEntry struct {
	items []media.SearchMedia
	exp   int64
}

var doubanMediaCache sync.Map // key -> doubanMediaEntry

// enrichDoubanMedia 把豆瓣榜单条目按标题并发搜索聚合成 media 卡片。
func (a *API) enrichDoubanMedia(ctx context.Context, typ, tag string, pageStart int, items []model.DoubanItem) []media.SearchMedia {
	mk := "douban_media:" + typ + ":" + tag + ":" + strconv.Itoa(pageStart)
	if v, ok := doubanMediaCache.Load(mk); ok {
		e := v.(doubanMediaEntry)
		if time.Now().UnixMilli() < e.exp {
			return e.items
		}
	}
	empty := []media.SearchMedia{}
	if a.deps.Media == nil || a.deps.Searcher == nil || a.deps.Registry == nil {
		return empty
	}
	// 选源：与搜索一致的 fast + 存活过滤。
	valid := make([]model.SourceConfig, 0)
	for _, s := range a.deps.Registry.List() {
		if strings.HasPrefix(s.URL, "http://") || strings.HasPrefix(s.URL, "https://") {
			valid = append(valid, s)
		}
	}
	fast := a.deps.Searcher.SelectFast(valid, a.deps.Cfg.SearchFastSources)
	now := time.Now()
	alive := make([]model.SourceConfig, 0, len(fast))
	for _, s := range fast {
		if !a.deps.Registry.Health().IsOpen(s.Key, now) {
			alive = append(alive, s)
		}
	}
	if len(alive) > 0 {
		fast = alive
	}

	out := make([]media.SearchMedia, len(items))
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup
	for i := range items {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			it := items[i]
			card := media.SearchMedia{
				ID: "douban:" + it.ID, Title: it.Title, Cover: it.Cover, Rating: it.Rating,
				Kind: media.KindMovie, MatchStatus: media.MatchPending,
			}
			if it.IsTV {
				card.Kind = media.KindTV
			}
			resp, serr := a.deps.Searcher.SearchQuiet(ctx, it.Title, fast, true)
			if serr == nil && len(resp.List) > 0 {
				if cards, cerr := a.deps.Media.SearchCards(resp.List, now); cerr == nil && len(cards) > 0 {
					best := pickCard(cards, it.Title)
					if best != nil {
						best.Title = it.Title
						best.Rating = it.Rating
						if best.Cover == "" {
							best.Cover = it.Cover
						}
						out[i] = *best
						return
					}
				}
			}
			out[i] = card // 没搜到源：保留豆瓣封面，前端点击回退搜索
		}(i)
	}
	wg.Wait()
	doubanMediaCache.Store(mk, doubanMediaEntry{items: out, exp: time.Now().Add(10 * time.Minute).UnixMilli()})
	return out
}

// pickCard 从聚合卡片里选标题最匹配的一张（无匹配则取第一张有源的）。
func pickCard(cards []media.SearchMedia, title string) *media.SearchMedia {
	want, _ := media.NormalizeTitle(title)
	for i := range cards {
		got, _ := media.NormalizeTitle(cards[i].Title)
		if want != "" && got == want {
			return &cards[i]
		}
	}
	for i := range cards {
		if len(cards[i].Sources) > 0 {
			return &cards[i]
		}
	}
	if len(cards) > 0 {
		return &cards[0]
	}
	return nil
}

// HotList 返回影视榜单。
func (a *API) HotList(w http.ResponseWriter, r *http.Request) {
	if a.deps.Recommend == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "推荐服务尚未启用")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	items, err := a.deps.Recommend.HotList(r.Context(), id)
	if err != nil {
		if errors.Is(err, recommend.ErrUnknownHotList) {
			httpx.WriteError(w, http.StatusBadRequest, "未知榜单")
			return
		}
		a.deps.Log.Debug("hot list failed", "err", err)
		httpx.WriteError(w, http.StatusBadGateway, "榜单获取失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
