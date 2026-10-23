package subscribe

import (
	"fmt"
	"hash/fnv"
	"net/url"
	"regexp"
	"strings"
)

const (
	// MaxVodSources 是单次订阅导入的点播源上限。
	MaxVodSources = 100
	// MaxLiveSources 是单次订阅导入的直播源上限。
	MaxLiveSources = 50
)

var (
	// trailingEmptyQuery 匹配结尾处连续的空查询串 / 锚点（`?`、`#`、`?#`）。
	trailingEmptyQuery = regexp.MustCompile(`[?#]+$`)
	// trailingSlashes 匹配结尾处连续的斜杠。
	trailingSlashes = regexp.MustCompile(`/+$`)
)

// NormalizeURL 把订阅条目地址归一成 http(s) 形态：trim、剥掉结尾处连续的空 ? / #，
// trimSlash 为 true 时再剥掉结尾斜杠（点播接口去、直播播放列表保留——尾斜杠对
// 部分直播服务有语义）。只接受 http(s)：空串、其他协议、无法解析的地址返回空串。
//
// 归一化必须在导入前完成：`.../vod`、`.../vod/`、`.../vod?` 是同一个接口，
// 不归一会被当成多个源重复导入。
func NormalizeURL(raw string, trimSlash bool) string {
	trimmed := strings.TrimSpace(raw)
	if !hasHTTPPrefix(trimmed) {
		return ""
	}
	trimmed = trailingEmptyQuery.ReplaceAllString(trimmed, "")
	if !validHTTPURL(trimmed) {
		return ""
	}
	if trimSlash {
		trimmed = trailingSlashes.ReplaceAllString(trimmed, "")
	}
	return trimmed
}

// NormalizeSubscriptionURL 归一化「订阅地址」本身：trim + 剥结尾空 ? / # 与斜杠。
// 手动输入与预置订阅必须走同一形态，否则同一地址带不带尾斜杠会被存成两条订阅。
func NormalizeSubscriptionURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	trimmed = trailingEmptyQuery.ReplaceAllString(trimmed, "")
	return trailingSlashes.ReplaceAllString(trimmed, "")
}

// hasHTTPPrefix 大小写不敏感地判断是否以 http:// 或 https:// 开头。
func hasHTTPPrefix(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// validHTTPURL 判断 s 是否是带主机名的 http(s) 地址（不负责归一化）。
func validHTTPURL(raw string) bool {
	if !hasHTTPPrefix(raw) {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Host != ""
}

// hostnameOf 返回 URL 的主机名（不含端口），用作源名称兜底；解析失败时回退原串。
func hostnameOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return rawURL
	}
	return parsed.Hostname()
}

// sourceKey 用归一化 URL 的 FNV-1a 32 位哈希生成稳定源 key：sub_<8hex>。
//
// 与条目顺序无关，因此订阅重新拉取后同一地址的 key 不变，源健康状态（熔断/停用）
// 可以跨刷新延续。32 位哈希在单份订阅（≤150 条）内的碰撞概率约 3e-6，可接受；
// 万一碰撞，两个源会共享健康状态，但不会影响功能正确性。
func sourceKey(rawURL string) string {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(rawURL))
	return fmt.Sprintf("sub_%08x", hasher.Sum32())
}

// displayName 返回源名称：缺省时用主机名兜底，保证前端永远有可展示的文字。
func displayName(name, target string) string {
	if name != "" {
		return name
	}
	return hostnameOf(target)
}
