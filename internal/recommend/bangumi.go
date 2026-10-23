package recommend

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"cilicili/internal/model"
)

const (
	// bangumiCalendarURL 是 Bangumi 每日放送接口（完全公开、免 key）。
	// 实测 2026-10：
	//
	//	GET https://api.bgm.tv/calendar
	//	→ [{"weekday":{"id":1,"en":"Mon","cn":"星期一"},"items":[
	//	      {"id":390200,"name":"列女战纪…","name_cn":"列女战纪…",
	//	       "images":{"large":"http://lain.bgm.tv/...","common":"…"},
	//	       "rating":{"score":3.6}}]}]
	//
	// api.bgm.tv 要求带可识别的 UA（浏览器 UA 亦可），空 UA / 默认库 UA 会被拒绝。
	bangumiCalendarURL = "https://api.bgm.tv/calendar"
	// bangumiCacheKey 是放送表的缓存键（全量数据，与星期无关）。
	bangumiCacheKey = "recommend:bangumi:calendar"
	// bangumiCacheTTL 是放送表的新鲜期：新番表按周更新，30 分钟足够。
	bangumiCacheTTL = 30 * time.Minute
	// bangumiWeekdayMin / bangumiWeekdayMax 是合法的星期 id（1=周一 … 7=周日）。
	bangumiWeekdayMin = 1
	bangumiWeekdayMax = 7
)

// bangumiSubject 是放送表里的单个作品。
type bangumiSubject struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	NameCN string `json:"name_cn"`
	Images struct {
		Large  string `json:"large"`
		Common string `json:"common"`
		Medium string `json:"medium"`
	} `json:"images"`
	Rating struct {
		Score float64 `json:"score"`
	} `json:"rating"`
}

// bangumiCalendarDay 是放送表一天的原始结构。
type bangumiCalendarDay struct {
	Weekday struct {
		ID int `json:"id"`
	} `json:"weekday"`
	Items []bangumiSubject `json:"items"`
}

// BangumiCalendar 拉取 Bangumi 每日放送表，按星期 1..7 升序返回；结果缓存 30 分钟。
// 星期 id 非法的分组（上游偶发返回 8 等值）与缺标题/封面的条目都会被丢弃。
func (f *Fetcher) BangumiCalendar(ctx context.Context) ([]model.BangumiCalendarDay, error) {
	if err := f.ready(); err != nil {
		return nil, err
	}

	return cached(f.cache, f.log, bangumiCacheKey, window(bangumiCacheTTL), "Bangumi 放送表", "/calendar",
		func() ([]model.BangumiCalendarDay, error) {
			return f.fetchBangumiCalendar(ctx)
		})
}

// fetchBangumiCalendar 发起一次放送表请求并归并为按天分组的结果。
func (f *Fetcher) fetchBangumiCalendar(ctx context.Context) ([]model.BangumiCalendarDay, error) {
	body, err := f.client.GetJSON(ctx, bangumiCalendarURL, f.requestOptions(f.jsonHeaders()))
	if err != nil {
		return nil, err
	}

	var raw []bangumiCalendarDay
	if err := json.Unmarshal(body, &raw); err != nil {
		f.log.Debug("Bangumi 放送表响应无法解析", "path", "/calendar", "err", redactText(errorText(err)))
		return nil, badData("Bangumi 放送表数据异常")
	}

	grouped := make(map[int][]model.DoubanItem, bangumiWeekdayMax)
	for _, day := range raw {
		weekday := day.Weekday.ID
		if weekday < bangumiWeekdayMin || weekday > bangumiWeekdayMax {
			continue
		}
		// 上游偶发把同一天拆成多个分组，这里合并而不是后者覆盖前者。
		if _, seen := grouped[weekday]; !seen {
			grouped[weekday] = make([]model.DoubanItem, 0, len(day.Items))
		}
		for _, subject := range day.Items {
			if item, ok := bangumiItem(subject); ok {
				grouped[weekday] = append(grouped[weekday], item)
			}
		}
	}
	if len(grouped) == 0 {
		return nil, badData("Bangumi 放送表数据为空")
	}

	weekdays := make([]int, 0, len(grouped))
	for weekday := range grouped {
		weekdays = append(weekdays, weekday)
	}
	sort.Ints(weekdays)

	days := make([]model.BangumiCalendarDay, 0, len(weekdays))
	for _, weekday := range weekdays {
		days = append(days, model.BangumiCalendarDay{Weekday: weekday, Items: grouped[weekday]})
	}
	return days, nil
}

// bangumiItem 把 Bangumi subject 转成首页卡片；缺 id/标题/封面的脏数据返回 false。
// 标题优先中文名（name_cn），封面优先大图；评分保留一位小数（无效评分省略）。
func bangumiItem(subject bangumiSubject) (model.DoubanItem, bool) {
	title := strings.TrimSpace(subject.NameCN)
	if title == "" {
		title = strings.TrimSpace(subject.Name)
	}
	cover := firstNonEmpty(subject.Images.Large, subject.Images.Common, subject.Images.Medium)
	if subject.ID <= 0 || title == "" || cover == "" {
		return model.DoubanItem{}, false
	}
	item := model.DoubanItem{
		ID:    strconv.Itoa(subject.ID),
		Title: title,
		Cover: cover,
		IsTV:  true,
	}
	if subject.Rating.Score > 0 {
		item.Rating = strconv.FormatFloat(subject.Rating.Score, 'f', 1, 64)
	}
	return item, true
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
