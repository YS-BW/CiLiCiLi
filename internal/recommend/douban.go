package recommend

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cilicili/internal/model"
)

const (
	// doubanSearchURL 是豆瓣「选电影 / 选剧集」的搜索引擎接口。
	// 实测 2026-10（服务端直连，必须带浏览器 UA + 站内 Referer）：
	//
	//	GET https://movie.douban.com/j/search_subjects?type=movie|tv&tag=%E7%83%AD%E9%97%A8&sort=recommend&page_limit=25&page_start=0
	//	→ {"subjects":[{"id":"36675942","title":"小城良方","cover":"https://img2.doubanio.com/...","rate":"","is_new":true,...}]}
	//
	// 返回体中的 is_new / playable / episodes_info 等字段 model 未暴露，这里只取
	// id/title/cover/rate；是否剧集由请求的 type 决定（响应里没有 isTv 标记）。
	doubanSearchURL = "https://movie.douban.com/j/search_subjects"
	doubanReferer   = "https://movie.douban.com/"
	// doubanCacheKeyPrefix 是豆瓣推荐缓存键前缀；type/tag/分页都进键。
	doubanCacheKeyPrefix = "recommend:douban:"
	// defaultDoubanTag 是未指定标签时的默认值（与 LibreTV 前端一致）。
	defaultDoubanTag = "热门"
	// maxDoubanTagRunes 限制标签长度，避免超长 tag 直接打到上游。
	maxDoubanTagRunes = 20
	// defaultDoubanPageSize / maxDoubanPageSize 是分页默认值与上限（与接口层约束一致）。
	defaultDoubanPageSize = 25
	maxDoubanPageSize     = 50
	// doubanCacheTTL 是豆瓣推荐的新鲜期。
	doubanCacheTTL = 10 * time.Minute
)

// doubanSubject 是 /j/search_subjects 的单个条目。
type doubanSubject struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Cover string `json:"cover"`
	Rate  string `json:"rate"`
}

// doubanSearchResponse 用指针区分「字段缺失」与「空数组」：
// 上游被拦截时返回的是 HTML 或 {"msg":...}，缺字段要报错；空数组是合法结果。
type doubanSearchResponse struct {
	Subjects *[]doubanSubject `json:"subjects"`
}

// Douban 拉取豆瓣推荐（电影 / 剧集）。typ 仅 "tv" 视为剧集，其余按电影处理；
// tag 为空回退「热门」；分页参数会被裁剪到安全范围。结果缓存 10 分钟。
func (f *Fetcher) Douban(ctx context.Context, typ, tag string, pageStart, pageSize int) ([]model.DoubanItem, error) {
	if err := f.ready(); err != nil {
		return nil, err
	}

	typ = normalizeDoubanType(typ)
	tag = normalizeDoubanTag(tag)
	if pageStart < 0 {
		pageStart = 0
	}
	if pageSize <= 0 {
		pageSize = defaultDoubanPageSize
	}
	if pageSize > maxDoubanPageSize {
		pageSize = maxDoubanPageSize
	}

	query := url.Values{}
	query.Set("type", typ)
	query.Set("tag", tag)
	query.Set("sort", "recommend")
	query.Set("page_limit", strconv.Itoa(pageSize))
	query.Set("page_start", strconv.Itoa(pageStart))
	target := doubanSearchURL + "?" + query.Encode()

	// 缓存键用归一化后的参数，保证同一请求不会因大小写/空白产生多条缓存。
	key := doubanCacheKeyPrefix + typ + ":" + tag + ":" + strconv.Itoa(pageStart) + ":" + strconv.Itoa(pageSize)

	return cached(f.cache, f.log, key, window(doubanCacheTTL), "豆瓣推荐", "/j/search_subjects",
		func() ([]model.DoubanItem, error) {
			return f.fetchDouban(ctx, target, typ)
		})
}

// fetchDouban 发起一次豆瓣推荐请求并归一化为卡片模型。
func (f *Fetcher) fetchDouban(ctx context.Context, target, typ string) ([]model.DoubanItem, error) {
	body, err := f.client.GetJSON(ctx, target, f.requestOptions(f.doubanHeaders()))
	if err != nil {
		return nil, err
	}

	var resp doubanSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil || resp.Subjects == nil {
		f.log.Debug("豆瓣推荐响应无法解析", "path", "/j/search_subjects", "err", redactText(errorText(err)))
		return nil, badData("豆瓣推荐响应异常")
	}

	isTV := typ == "tv"
	items := make([]model.DoubanItem, 0, len(*resp.Subjects))
	for _, subject := range *resp.Subjects {
		id := strings.TrimSpace(subject.ID)
		title := strings.TrimSpace(subject.Title)
		cover := strings.TrimSpace(subject.Cover)
		// 缺 id / 标题 / 封面的条目视为脏数据丢弃（前端无法渲染卡片）。
		if id == "" || title == "" || cover == "" {
			continue
		}
		items = append(items, model.DoubanItem{
			ID:     id,
			Title:  title,
			Cover:  cover,
			Rating: doubanRating(subject.Rate),
			IsTV:   isTV,
		})
	}
	return items, nil
}

// doubanRating 归一化豆瓣评分为展示串：0 或空表示暂无评分，直接省略。
func doubanRating(rate string) string {
	rate = strings.TrimSpace(rate)
	if rate == "" || rate == "0" || rate == "0.0" {
		return ""
	}
	return rate
}

// normalizeDoubanType 只接受 "tv"，其余一律按电影处理（与 LibreTV 前端一致）。
func normalizeDoubanType(typ string) string {
	if strings.EqualFold(strings.TrimSpace(typ), "tv") {
		return "tv"
	}
	return "movie"
}

// normalizeDoubanTag 去空白、限制长度，空值回退默认标签。
func normalizeDoubanTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return defaultDoubanTag
	}
	return truncateRunes(tag, maxDoubanTagRunes)
}

// errorText 把可能为 nil 的解析错误转成可打日志的字符串。
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
