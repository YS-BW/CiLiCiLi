package recommend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cilicili/internal/model"
)

const (
	// hotListCacheKeyPrefix 是影视榜单的缓存键前缀。
	hotListCacheKeyPrefix = "recommend:hotlist:"
	// hotListCacheTTL 是榜单的新鲜期：60s 侧的榜单本身按天/周更新，1 小时足够。
	hotListCacheTTL = time.Hour
	// sixtyDoubanWeeklyPathPrefix 是豆瓣周榜接口路径前缀。
	// 实测 2026-10：
	//
	//	GET {base}/v2/douban/weekly/movie  →  {"code":200,"data":[{"id":"37322586","title":"生日快乐","rating":8.9,"cover":"https://img9.doubanio.com/...","cover_proxy":"https://doubanio.viki.moe/..."}]}
	//
	// 注意：旧文档里的 /v2/douban_weekly 已不存在（实测 404，接口要求 /v2 版本前缀 + /douban/weekly/<类别>）。
	sixtyDoubanWeeklyPathPrefix = "/v2/douban/weekly/"
	// sixtyBaiduTeleplayPath 是百度热播剧榜接口路径。
	// 实测 2026-10：
	//
	//	GET {base}/v2/baidu/teleplay → {"code":200,"data":[{"rank":1,"title":"九门","cover":"https://b.bdstatic.com/...","score":"95471"}]}
	//
	// 百度榜没有稳定 id，用榜单内序号合成。
	sixtyBaiduTeleplayPath = "/v2/baidu/teleplay"
	// baiduTeleplayID 是百度热播剧榜的榜单标识。
	baiduTeleplayID = "baidu_teleplay"
	// maxHotListIDRunes 限制回显的榜单 id 长度，避免把超长用户输入原样带回错误。
	maxHotListIDRunes = 30
)

// ErrUnknownHotList 表示榜单标识不受支持；接口层据此返回 400 而不是 502。
var ErrUnknownHotList = errors.New("未知榜单")

// hotListCategories 是支持的榜单标识 → 60s 豆瓣周榜类别（顺序即 HotListIDs 的返回顺序）。
// 映射语义与 LibreTV 的 douban-weekly.ts 一致：五个豆瓣周榜 + 百度热播剧榜。
var hotListCategories = map[string]string{
	"douban_movie_weekly": "movie",
	"douban_tv_chinese":   "tv_chinese",
	"douban_tv_global":    "tv_global",
	"douban_show_chinese": "show_chinese",
	"douban_show_global":  "show_global",
}

// hotListIDs 是榜单标识的稳定展示顺序（豆瓣电影榜在最前）。
var hotListIDs = []string{
	"douban_movie_weekly",
	"douban_tv_chinese",
	"douban_tv_global",
	"douban_show_chinese",
	"douban_show_global",
	baiduTeleplayID,
}

// HotListIDs 返回支持的榜单标识（顺序固定，供接口层枚举/校验）。
func HotListIDs() []string {
	return append([]string(nil), hotListIDs...)
}

// IsHotListID 判断榜单标识是否受支持。
func IsHotListID(id string) bool {
	id = strings.TrimSpace(id)
	if _, ok := hotListCategories[id]; ok {
		return true
	}
	return id == baiduTeleplayID
}

// sixtyEnvelope 是 60s API 的统一响应外壳：{code, message, data}。
// code 只用于日志提示（不同实例有 200/0 两种写法），data 才是有意义的部分。
type sixtyEnvelope struct {
	Code int             `json:"code"`
	Data json.RawMessage `json:"data"`
}

// doubanWeeklyRaw 是豆瓣周榜条目（只取用到的字段）。
type doubanWeeklyRaw struct {
	ID         stringOrNumber `json:"id"`
	Title      string         `json:"title"`
	Rating     float64        `json:"rating"`
	Cover      string         `json:"cover"`
	CoverProxy string         `json:"cover_proxy"`
}

// baiduTeleplayRaw 是百度热播剧榜条目。
type baiduTeleplayRaw struct {
	Title string `json:"title"`
	Cover string `json:"cover"`
}

// stringOrNumber 兼容 JSON 里可能写成字符串或数字的字段（60s 的 id 两种都会出现）。
type stringOrNumber string

// UnmarshalJSON 同时接受 "123" 与 123。
func (s *stringOrNumber) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		*s = ""
		return nil
	}
	if data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*s = stringOrNumber(text)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	*s = stringOrNumber(number.String())
	return nil
}

// HotList 拉取影视热榜。id 必须是 HotListIDs 中的一个，未知 id 返回 ErrUnknownHotList；
// 结果缓存 1 小时。主实例失败时回退 60s 官方公共实例。
func (f *Fetcher) HotList(ctx context.Context, id string) ([]model.DoubanItem, error) {
	// 先校验榜单标识：未知 id 是调用方错误（400），与上游故障严格区分。
	id = strings.TrimSpace(id)
	category, isDoubanWeekly := hotListCategories[id]
	isBaidu := id == baiduTeleplayID
	if !isDoubanWeekly && !isBaidu {
		return nil, fmt.Errorf("%w: %s", ErrUnknownHotList, truncateRunes(id, maxHotListIDRunes))
	}
	if err := f.ready(); err != nil {
		return nil, err
	}

	// 除豆瓣电影周榜外，其余榜单都是剧集/综艺（前端卡片据此选角标）。
	isTV := id != "douban_movie_weekly"
	path := sixtyBaiduTeleplayPath
	if isDoubanWeekly {
		path = sixtyDoubanWeeklyPathPrefix + category
	}

	return cached(f.cache, f.log, hotListCacheKeyPrefix+id, window(hotListCacheTTL), "影视榜单", path,
		func() ([]model.DoubanItem, error) {
			data, err := f.getSixtyData(ctx, path)
			if err != nil {
				return nil, err
			}
			var items []model.DoubanItem
			if isDoubanWeekly {
				items, err = decodeDoubanWeekly(data, isTV)
			} else {
				items, err = decodeBaiduTeleplay(data)
			}
			if err != nil {
				return nil, err
			}
			if len(items) == 0 {
				return nil, badData("榜单数据为空")
			}
			return items, nil
		})
}

// sixtyBases 返回按优先级排列的 60s 实例地址：主实例在前，主实例不是官方实例时
// 追加官方公共实例兜底（官方实例每日额度有限，只做备用）。
func (f *Fetcher) sixtyBases() []string {
	if f.sixtyBase == officialSixtyAPIBase {
		return []string{f.sixtyBase}
	}
	return []string{f.sixtyBase, officialSixtyAPIBase}
}

// getSixtyData 依次尝试各 60s 实例，返回响应里的 data 片段。
// 调用方 context 结束后不再换实例（避免无谓的额外请求）。
func (f *Fetcher) getSixtyData(ctx context.Context, path string) ([]byte, error) {
	var lastErr error
	for _, base := range f.sixtyBases() {
		body, err := f.client.GetJSON(ctx, base+path, f.requestOptions(f.jsonHeaders()))
		if err != nil {
			lastErr = err
			f.log.Debug("60s 实例请求失败", "base", base, "path", path, "err", redactText(err.Error()))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		var envelope sixtyEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			// 实例返回了非 JSON（被限流时常给 HTML），换下一个实例再试。
			f.log.Debug("60s 实例响应不是合法 JSON", "base", base, "path", path, "err", redactText(err.Error()))
			lastErr = badData("榜单数据异常")
			continue
		}
		if envelope.Code != 200 {
			f.log.Debug("60s 实例返回非 200 业务码", "base", base, "path", path, "code", envelope.Code)
		}
		if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			lastErr = badData("榜单数据异常")
			continue
		}
		return envelope.Data, nil
	}
	if lastErr == nil {
		lastErr = badData("榜单数据异常")
	}
	return nil, lastErr
}

// decodeDoubanWeekly 把豆瓣周榜条目转成卡片；缺标题/封面的脏数据丢弃。
func decodeDoubanWeekly(data []byte, isTV bool) ([]model.DoubanItem, error) {
	var raws []doubanWeeklyRaw
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, badData("榜单数据异常")
	}
	items := make([]model.DoubanItem, 0, len(raws))
	for _, raw := range raws {
		// cover 是 img*.doubanio.com 原生图（命中图片代理白名单），cover_proxy 仅作兜底。
		cover := strings.TrimSpace(raw.Cover)
		if cover == "" {
			cover = strings.TrimSpace(raw.CoverProxy)
		}
		id := strings.TrimSpace(string(raw.ID))
		title := strings.TrimSpace(raw.Title)
		if id == "" || title == "" || cover == "" {
			continue
		}
		item := model.DoubanItem{ID: id, Title: title, Cover: cover, IsTV: isTV}
		if raw.Rating > 0 {
			item.Rating = strconv.FormatFloat(raw.Rating, 'f', -1, 64)
		}
		items = append(items, item)
	}
	return items, nil
}

// decodeBaiduTeleplay 把百度热播剧榜转成卡片；榜单没有稳定 id，用序号合成。
func decodeBaiduTeleplay(data []byte) ([]model.DoubanItem, error) {
	var raws []baiduTeleplayRaw
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, badData("榜单数据异常")
	}
	items := make([]model.DoubanItem, 0, len(raws))
	for index, raw := range raws {
		title := strings.TrimSpace(raw.Title)
		cover := strings.TrimSpace(raw.Cover)
		if title == "" || cover == "" {
			continue
		}
		items = append(items, model.DoubanItem{
			ID:    "baidu_" + strconv.Itoa(index),
			Title: title,
			Cover: cover,
			IsTV:  true,
		})
	}
	return items, nil
}
