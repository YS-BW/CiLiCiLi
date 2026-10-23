package subscribe

import (
	"regexp"
	"strconv"
	"strings"

	"cilicili/internal/model"
)

// TVBOX 站点类型：0=XML 接口，1=JSON 接口（Apple CMS），3=Spider，4=JSON 外链。
const (
	siteTypeXML    = 0
	siteTypeJSON   = 1
	siteTypeSpider = 3
	siteTypeAPI    = 4
	// liveTypePlaylist 是 TVBOX 的普通播放列表（m3u/txt）；1 是单仓 JSON。
	liveTypePlaylist = 0
)

var (
	// spiderPattern 匹配 Spider 特征：csp_ 前缀，或 jar / js / py 资源（含查询串）。
	spiderPattern = regexp.MustCompile(`(?i)^csp_|\.(?:jar|js|py)(?:[?#].*)?$`)
	// localAssetPattern 匹配非 http(s) 的相对/本地资源（如 ./json/xxx.json）。
	localAssetPattern = regexp.MustCompile(`^\.{0,2}/`)
	// cmsVodPattern 匹配 Apple CMS 直连接口路径（负向断言 /at/xml 在 isAppleCMSAPI 里做）。
	cmsVodPattern = regexp.MustCompile(`(?i)/api\.php/provide/vod`)
	// m3uPattern 匹配 M3U 播放列表地址。
	m3uPattern = regexp.MustCompile(`(?i)\.m3u8?(?:[?#].*)?$`)
	// txtPattern 匹配纯文本频道列表地址（TVBOX 的 txt 直播源，本站无法解析）。
	txtPattern = regexp.MustCompile(`(?i)\.txt(?:[?#].*)?$`)
)

// parseTVBOX 解析 TVBOX 配置：sites / lives → 本站点播源与直播源。
// 只导入「直连类」条目（type=1 的 Apple CMS 接口与 M3U 直播），其余跳过并计入统计，
// 不因为个别条目不可用就丢掉整份配置。
func parseTVBOX(object map[string]any) (*model.SourceListPayload, error) {
	rawSites, _ := asArray(object["sites"])
	rawLives, _ := asArray(object["lives"])

	skipped := newSkipCounter()
	sources, vodTruncated := collectVodSources(rawSites, skipped)
	liveSources, liveTruncated := collectLiveSources(rawLives, skipped)

	if len(sources) == 0 && len(liveSources) == 0 {
		if skipped.count > 0 {
			return nil, contentErrorf("配置中没有可直接导入的源：已跳过 %d 个条目（%s）",
				skipped.count, describeReasons(skipped.byReason))
		}
		return nil, contentErrorf("配置中没有可用的站点（sites / lives 均为空）")
	}

	return &model.SourceListPayload{
		Name:        optionalString(object["name"]),
		Sources:     sources,
		LiveSources: liveSources,
		Stats:       skipped.fill(newStats(formatTVBOX), vodTruncated+liveTruncated),
	}, nil
}

// collectVodSources 收集点播源：只接受 Apple CMS 直连接口，Spider / XML / 不可搜索
// / 非法地址分别计数跳过。返回导入的源与被截断的条目数。
func collectVodSources(rawSites []any, skipped *skipCounter) ([]model.SourceConfig, int) {
	sources := make([]model.SourceConfig, 0, min(len(rawSites), MaxVodSources))
	seen := make(map[string]struct{}, len(rawSites))
	truncated := 0

	for _, raw := range rawSites {
		site, ok := asObject(raw)
		if !ok {
			continue
		}
		name := optionalString(site["name"])
		api := optionalString(site["api"])
		siteType, hasType := jsonInt(site["type"])

		if api == "" {
			skipped.mark(model.SkipInvalidURL, name)
			continue
		}
		// Spider 类与本地资源需要 TVBOX 引擎才能运行，服务端无法直接采集。
		if (hasType && siteType == siteTypeSpider) || spiderPattern.MatchString(api) {
			skipped.mark(model.SkipSpider, name)
			continue
		}
		if !hasHTTPPrefix(api) {
			if localAssetPattern.MatchString(api) {
				skipped.mark(model.SkipSpider, name)
			} else {
				skipped.mark(model.SkipInvalidURL, name)
			}
			continue
		}
		// 站点自身声明不可搜索：本站只有搜索入口，导入后无从使用。
		if isUnsearchable(site["searchable"]) {
			skipped.mark(model.SkipUnsearchable, name)
			continue
		}

		// 可导入判定：显式 JSON 接口，或类型缺失/写成 XML、外链但地址命中 Apple CMS 特征
		// （共享配置里 type 省略或写错的很多，直接丢弃会白丢可用源）。
		importable := hasType && siteType == siteTypeJSON
		if !importable && (!hasType || siteType == siteTypeXML || siteType == siteTypeAPI) {
			importable = isAppleCMSAPI(api)
		}
		if !importable {
			if hasType && siteType == siteTypeAPI {
				skipped.mark(model.SkipSpider, name)
			} else {
				skipped.mark(model.SkipXML, name)
			}
			continue
		}

		target := NormalizeURL(api, true)
		if target == "" {
			skipped.mark(model.SkipInvalidURL, name)
			continue
		}
		if _, duplicate := seen[target]; duplicate {
			continue
		}
		if len(sources) >= MaxVodSources {
			truncated++
			continue
		}
		seen[target] = struct{}{}
		sources = append(sources, model.SourceConfig{
			Key:  sourceKey(target),
			Name: displayName(name, target),
			URL:  target,
		})
	}

	return sources, truncated
}

// collectLiveSources 收集直播源：只接受 M3U 播放列表。
// 单仓 JSON 订阅本站不支持，txt 频道列表无法被 M3U 解析器识别，均计入 nonM3uLive。
func collectLiveSources(rawLives []any, skipped *skipCounter) ([]model.LiveSourceConfig, int) {
	liveSources := make([]model.LiveSourceConfig, 0, min(len(rawLives), MaxLiveSources))
	seen := make(map[string]struct{}, len(rawLives))
	truncated := 0

	for _, raw := range rawLives {
		live, ok := asObject(raw)
		if !ok {
			continue
		}
		name := optionalString(live["name"])
		target := NormalizeURL(optionalString(live["url"]), false)
		if target == "" {
			skipped.mark(model.SkipInvalidURL, name)
			continue
		}
		liveType, hasType := jsonInt(live["type"])
		if (hasType && liveType != liveTypePlaylist) || (!m3uPattern.MatchString(target) && txtPattern.MatchString(target)) {
			skipped.mark(model.SkipNonM3U, name)
			continue
		}
		if _, duplicate := seen[target]; duplicate {
			continue
		}
		if len(liveSources) >= MaxLiveSources {
			truncated++
			continue
		}
		seen[target] = struct{}{}
		liveSources = append(liveSources, model.LiveSourceConfig{
			Key:  sourceKey(target),
			Name: displayName(name, target),
			URL:  target,
			EPG:  NormalizeURL(optionalString(live["epg"]), false),
		})
	}

	return liveSources, truncated
}

// isAppleCMSAPI 判断地址是否命中 Apple CMS 直连接口特征。
//
// 等价于 `/api\.php/provide/vod(?![^?#]*/at/xml)(?:[/?#]|$)`：
// RE2 不支持负向先行断言，这里手写「路径段内不含 /at/xml」的检查——
// /at/xml 是 XML 通道，本服务解析不了。
func isAppleCMSAPI(api string) bool {
	location := cmsVodPattern.FindStringIndex(api)
	if location == nil {
		return false
	}
	rest := api[location[1]:]
	pathPart := rest
	if index := strings.IndexAny(rest, "?#"); index >= 0 {
		pathPart = rest[:index]
	}
	if strings.Contains(strings.ToLower(pathPart), "/at/xml") {
		return false
	}
	if rest == "" {
		return true
	}
	return rest[0] == '/' || rest[0] == '?' || rest[0] == '#'
}

// isUnsearchable 判断 TVBOX 站点是否声明了不可搜索（searchable: 0 或 "0"）。
func isUnsearchable(value any) bool {
	switch typed := value.(type) {
	case float64:
		return typed == 0
	case string:
		return strings.TrimSpace(typed) == "0"
	default:
		return false
	}
}

// jsonInt 把 JSON 数字或数字字符串转成 int；其余类型返回 false。
func jsonInt(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.Atoi(trimmed)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}
