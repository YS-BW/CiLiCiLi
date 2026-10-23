package subscribe

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cilicili/internal/model"
)

// 订阅格式标识（写入 stats.format）。
const (
	formatLibreTV = "libretv"
	formatTVBOX   = "tvbox"
	formatCatalog = "source-catalog"
	// skippedSampleLimit 是 stats.skippedSamples 保留的样例条数（提示文案不宜过长）。
	skippedSampleLimit = 3
)

// ErrInvalidContent 表示订阅内容无法解析或没有可导入的条目；
// 接口层据此返回 400（而不是把「内容问题」当成 502 上游故障）。
var ErrInvalidContent = errors.New("订阅内容格式不正确")

// contentErrorf 构造带 ErrInvalidContent 哨兵的解析错误。
func contentErrorf(format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	return fmt.Errorf("%w：%s", ErrInvalidContent, message)
}

// Parse 解析订阅内容，自动识别三种格式：
//  1. TVBOX 配置：顶层出现 sites / lives 数组；
//  2. 接口目录：非空裸数组，且每个条目都带 baseUrl；
//  3. LibreTV-SourceList：{name?, sources, liveSources}、裸 sources 数组或老格式。
//
// 三种格式共同语义：URL 只接受 http(s)、归一化尾部空查询串/斜杠、按归一化 URL 去重、
// 去重后再按上限截断，并给每个条目生成稳定 key（sub_<8hex>）。
// 混用 baseUrl 与 url 的裸数组直接报错，避免静默丢掉一半条目。
func Parse(data []byte) (*model.SourceListPayload, error) {
	root, err := decodeJSON(data)
	if err != nil {
		return nil, err
	}
	return parsePayload(root)
}

// decodeJSON 宽容解析订阅 JSON，非法内容统一报「不是合法的 JSON」。
func decodeJSON(data []byte) (any, error) {
	text := strings.TrimPrefix(string(data), "\uFEFF")
	var root any
	if err := json.Unmarshal([]byte(stripJSONArtifacts(text)), &root); err != nil {
		return nil, contentErrorf("不是合法的 JSON")
	}
	return root, nil
}

// parsePayload 按顶层结构识别格式后分派。
func parsePayload(root any) (*model.SourceListPayload, error) {
	if object, ok := asObject(root); ok {
		if hasArray(object, "sites") || hasArray(object, "lives") {
			return parseTVBOX(object)
		}
		return parseLibreTV(root)
	}

	if array, ok := asArray(root); ok {
		withBaseURL := 0
		for _, entry := range array {
			entry, ok := asObject(entry)
			if !ok {
				continue
			}
			if _, has := entry["baseUrl"]; has {
				withBaseURL++
			}
		}
		switch {
		case len(array) > 0 && withBaseURL == len(array):
			return parseCatalog(array)
		case withBaseURL > 0:
			return nil, contentErrorf("混用了 baseUrl 与 url 两种条目，请统一为一种格式（接口目录用 baseUrl，LibreTV 源列表用 url）")
		}
		return parseLibreTV(root)
	}

	// 字符串/数字/null 等既不是对象也不是数组的内容，按 LibreTV 格式报格式错误。
	return parseLibreTV(root)
}

// parseLibreTV 解析 LibreTV-SourceList：完整格式、裸数组、只有 sources 的老格式、
// 只有 liveSources 的纯直播订阅都兼容。
//
// 该格式保持 LibreTV 既有行为：非法/重复/超限条目静默丢弃，stats 全 0；
// 跳过原因统计只对 TVBOX 与接口目录生效。
func parseLibreTV(root any) (*model.SourceListPayload, error) {
	var rawVod, rawLive []any
	var name string

	if object, ok := asObject(root); ok {
		name = optionalString(object["name"])
		rawVod, _ = asArray(object["sources"])
		rawLive, _ = asArray(object["liveSources"])
	} else if array, ok := asArray(root); ok {
		rawVod = array
	}

	if len(rawVod) == 0 && len(rawLive) == 0 {
		return nil, contentErrorf("缺少 sources / liveSources 数组")
	}

	return &model.SourceListPayload{
		Name:        name,
		Sources:     collectLibreTVVod(rawVod),
		LiveSources: collectLibreTVLive(rawLive),
		Stats:       newStats(formatLibreTV),
	}, nil
}

// collectLibreTVVod 收集 LibreTV 点播源：先按归一化 URL 去重，再按上限截断。
func collectLibreTVVod(rawVod []any) []model.SourceConfig {
	sources := make([]model.SourceConfig, 0, min(len(rawVod), MaxVodSources))
	seen := make(map[string]struct{}, len(rawVod))

	for _, raw := range rawVod {
		entry, ok := asObject(raw)
		if !ok {
			continue
		}
		if len(sources) >= MaxVodSources {
			break
		}
		target := NormalizeURL(optionalString(entry["url"]), true)
		if target == "" {
			continue
		}
		if _, duplicate := seen[target]; duplicate {
			continue
		}
		seen[target] = struct{}{}
		sources = append(sources, model.SourceConfig{
			Key:     sourceKey(target),
			Name:    displayName(optionalString(entry["name"]), target),
			URL:     target,
			Detail:  optionalString(entry["detail"]),
			IsAdult: boolTrue(entry["isAdult"]),
		})
	}
	return sources
}

// collectLibreTVLive 收集 LibreTV 直播源；EPG 地址非法时只丢弃该字段，不丢整条源。
func collectLibreTVLive(rawLive []any) []model.LiveSourceConfig {
	liveSources := make([]model.LiveSourceConfig, 0, min(len(rawLive), MaxLiveSources))
	seen := make(map[string]struct{}, len(rawLive))

	for _, raw := range rawLive {
		entry, ok := asObject(raw)
		if !ok {
			continue
		}
		if len(liveSources) >= MaxLiveSources {
			break
		}
		target := NormalizeURL(optionalString(entry["url"]), false)
		if target == "" {
			continue
		}
		if _, duplicate := seen[target]; duplicate {
			continue
		}
		seen[target] = struct{}{}
		liveSources = append(liveSources, model.LiveSourceConfig{
			Key:  sourceKey(target),
			Name: displayName(optionalString(entry["name"]), target),
			URL:  target,
			EPG:  NormalizeURL(optionalString(entry["epg"]), false),
		})
	}
	return liveSources
}

// skipCounter 累计「条目不可导入」的统计（TVBOX / 接口目录共用）。
type skipCounter struct {
	count    int
	byReason map[model.SubscriptionSkipReason]int
	samples  []string
}

// newSkipCounter 返回空的跳过计数器。
func newSkipCounter() *skipCounter {
	return &skipCounter{byReason: make(map[model.SubscriptionSkipReason]int)}
}

// mark 记一次跳过：样例只收非空名称、去重、最多 skippedSampleLimit 个。
func (c *skipCounter) mark(reason model.SubscriptionSkipReason, name string) {
	c.count++
	c.byReason[reason]++
	if name == "" || len(c.samples) >= skippedSampleLimit {
		return
	}
	for _, sample := range c.samples {
		if sample == name {
			return
		}
	}
	c.samples = append(c.samples, name)
}

// fill 把计数写进解析统计。
func (c *skipCounter) fill(stats *model.SubscriptionParseStats, truncated int) *model.SubscriptionParseStats {
	stats.Skipped = c.count
	stats.SkippedByReason = c.byReason
	stats.SkippedSamples = c.samples
	stats.Truncated = truncated
	return stats
}

// newStats 返回指定格式的空统计（非 nil 的 reason map，形态确定）。
func newStats(format string) *model.SubscriptionParseStats {
	return &model.SubscriptionParseStats{
		Format:          format,
		SkippedByReason: make(map[model.SubscriptionSkipReason]int),
	}
}

// skipReasonOrder 是跳过原因的固定展示顺序，避免同类统计在不同配置下文案抖动。
var skipReasonOrder = []model.SubscriptionSkipReason{
	model.SkipSpider,
	model.SkipXML,
	model.SkipUnsearchable,
	model.SkipNonM3U,
	model.SkipDisabled,
	model.SkipInvalidURL,
}

// skipReasonLabels 是跳过原因的中文说明。
var skipReasonLabels = map[model.SubscriptionSkipReason]string{
	model.SkipSpider:       "Spider 引擎",
	model.SkipXML:          "XML 接口",
	model.SkipUnsearchable: "不支持搜索",
	model.SkipNonM3U:       "非 M3U 直播",
	model.SkipDisabled:     "已停用",
	model.SkipInvalidURL:   "地址不可用",
}

// describeReasons 按固定顺序拼装原因明细，如「Spider 引擎 4、XML 接口 1」。
func describeReasons(byReason map[model.SubscriptionSkipReason]int) string {
	parts := make([]string, 0, len(byReason))
	for _, reason := range skipReasonOrder {
		if count := byReason[reason]; count > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", skipReasonLabels[reason], count))
		}
	}
	return strings.Join(parts, "、")
}

// asObject 把 JSON 对象转成 map；其余类型返回 false。
func asObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

// asArray 把 JSON 数组转成 []any；其余类型返回 false。
func asArray(value any) ([]any, bool) {
	array, ok := value.([]any)
	return array, ok
}

// hasArray 判断对象里某个 key 是否为数组（TVBOX 判别用）。
func hasArray(object map[string]any, key string) bool {
	_, ok := asArray(object[key])
	return ok
}

// optionalString 返回去空白后的非空字符串；非字符串或空白返回空串。
func optionalString(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

// boolTrue 只接受 JSON 布尔 true（字符串 "true" 不算，避免误判成人内容标记）。
func boolTrue(value any) bool {
	flag, ok := value.(bool)
	return ok && flag
}
