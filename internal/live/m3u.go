package live

import (
	"net/url"
	"regexp"
	"sort"
	"strings"

	"cilicili/internal/model"
)

// M3U 属性与标题的解析规则（与 LibreTV 的 m3u-parser 对齐）：
//   - 属性一律 `key="value"`，key 大小写不敏感（统一转小写）；
//   - 标题取「去掉属性片段后」第一个逗号之后的文本，因此属性写在标题后面
//     （非标准写法 `#EXTINF:-1,CCTV-1 tvg-id="cctv1"`）也能正确取到名称；
//   - `#EXTGRP:` 只在 EXTINF 未带 group-title 时补充分组。
var (
	m3uAttrRe      = regexp.MustCompile(`([A-Za-z0-9_-]+)="([^"]*)"`)
	m3uAttrStripRe = regexp.MustCompile(`[A-Za-z0-9_-]+="[^"]*"`)
	extinfPrefixRe = regexp.MustCompile(`(?i)^#EXTINF\s*:\s*`)
	playlistNameRe = regexp.MustCompile(`(?m)^#PLAYLIST:(.+)$`)
)

// ParseM3U 解析 M3U/M3U8 直播订阅文本。
//
// 必须含 `#EXTM3U` 头（防 200 的 HTML 错误页被当成播放列表）；只保留
// http(s) 流地址（rtp/udp/rtsp 等浏览器播不了的协议直接丢弃），并按 URL
// 去重。频道 id 优先 tvg-id，缺失时用 URL 的稳定 hash；groups 去重排序。
func ParseM3U(data []byte) (*model.LivePlaylistResponse, error) {
	return parseM3U(data, "")
}

// parseM3U 是带基准地址的解析实现：订阅里的相对流地址按 baseURL 解析为绝对地址
// （LibreTV 同样以订阅地址作为 base）。导出的 ParseM3U 没有基准地址。
func parseM3U(data []byte, baseURL string) (*model.LivePlaylistResponse, error) {
	text := strings.TrimPrefix(string(data), "\uFEFF")
	if strings.TrimSpace(text) == "" {
		return nil, ErrInvalidPlaylist
	}
	lines := strings.Split(text, "\n")

	// 首个非空行必须是 #EXTM3U：这是播放列表与 HTML/JSON 错误页的关键区分点。
	headerOK := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		headerOK = hasPrefixFold(line, "#EXTM3U")
		break
	}
	if !headerOK {
		return nil, ErrInvalidPlaylist
	}

	var base *url.URL
	if baseURL != "" {
		if parsed, err := url.Parse(baseURL); err == nil {
			base = parsed
		}
	}

	channels := make([]model.LiveChannel, 0, 64)
	groups := make([]string, 0, 8)
	groupSeen := make(map[string]bool, 8)
	seen := make(map[string]bool, 64)

	var cur *extinf
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case hasPrefixFold(line, "#EXTINF"):
			cur = parseExtinf(line)
		case hasPrefixFold(line, "#EXTGRP:"):
			group := strings.TrimSpace(line[len("#EXTGRP:"):])
			if cur != nil && group != "" && cur.attrs["group-title"] == "" {
				cur.extGroup = group
			}
		case strings.HasPrefix(line, "#"):
			// 其余指令（#EXTVLCOPT、#EXT-X-* 等）忽略。
		default:
			// 非注释行即流地址；无论是否合法都结束当前 EXTINF 上下文。
			ch := cur
			cur = nil
			streamURL := normalizeStreamURL(line, base)
			if streamURL == "" || seen[streamURL] {
				continue
			}
			seen[streamURL] = true

			var attrs map[string]string
			if ch != nil {
				attrs = ch.attrs
			}
			tvgID := attrs["tvg-id"]
			name := ""
			if ch != nil {
				name = ch.title
			}
			if name == "" {
				name = nameFromURL(streamURL)
			}
			group := attrs["group-title"]
			if group == "" && ch != nil {
				group = ch.extGroup
			}
			logo := ""
			if raw := attrs["tvg-logo"]; raw != "" {
				logo = normalizeStreamURL(raw, base)
			}
			id := tvgID
			if id == "" {
				id = stableID(streamURL)
			}
			channels = append(channels, model.LiveChannel{
				ID:      id,
				Name:    name,
				URL:     streamURL,
				Logo:    logo,
				Group:   group,
				TvgID:   tvgID,
				RawName: attrs["tvg-name"],
			})
			if group != "" && !groupSeen[group] {
				groupSeen[group] = true
				groups = append(groups, group)
			}
		}
	}

	sort.Strings(groups)
	return &model.LivePlaylistResponse{
		Name:     playlistName(text),
		Channels: channels,
		Groups:   groups,
	}, nil
}

// extinf 是一条 #EXTINF 指令的解析结果。
type extinf struct {
	attrs    map[string]string
	title    string
	extGroup string
}

func parseExtinf(line string) *extinf {
	attrs := make(map[string]string, 4)
	for _, m := range m3uAttrRe.FindAllStringSubmatch(line, -1) {
		attrs[strings.ToLower(m[1])] = strings.TrimSpace(m[2])
	}

	stripped := m3uAttrStripRe.ReplaceAllString(line, "")
	stripped = extinfPrefixRe.ReplaceAllString(stripped, "")
	title := ""
	if idx := strings.Index(stripped, ","); idx >= 0 {
		title = strings.Trim(strings.TrimSpace(stripped[idx+1:]), `"`)
		title = strings.TrimSpace(title)
	}
	return &extinf{attrs: attrs, title: title}
}

// playlistName 取 `#PLAYLIST:<name>` 声明（LibreTV 的订阅导出会写这一行）。
func playlistName(text string) string {
	m := playlistNameRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// normalizeStreamURL 把地址解析为绝对地址；只放行 http(s)。
func normalizeStreamURL(raw string, base *url.URL) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	if base != nil {
		parsed = base.ResolveReference(parsed)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	if parsed.Host == "" {
		return ""
	}
	return parsed.String()
}

// nameFromURL 是无名称频道时的兜底名称：取路径末段并去掉媒体扩展名，
// 仍为空则用主机名（与 LibreTV 的 nameFromUrl 一致）。
func nameFromURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "未命名频道"
	}
	if last := lastPathSegment(parsed.Path); last != "" {
		return last
	}
	if host := parsed.Hostname(); host != "" {
		return host
	}
	return "未命名频道"
}

func lastPathSegment(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	last := parts[len(parts)-1]
	if decoded, err := url.PathUnescape(last); err == nil {
		last = decoded
	}
	lower := strings.ToLower(last)
	for _, ext := range []string{".m3u8", ".m3u", ".flv", ".ts", ".mp4"} {
		if strings.HasSuffix(lower, ext) {
			last = last[:len(last)-len(ext)]
			break
		}
	}
	return strings.TrimSpace(last)
}

// stableID 由任意字符串生成稳定的短 id（djb2 + base36，与 LibreTV 的 stableId 一致）。
func stableID(input string) string {
	var h uint32 = 5381
	for i := 0; i < len(input); i++ {
		h = h*33 + uint32(input[i])
	}
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	var buf [7]byte
	i := len(buf)
	for h > 0 {
		i--
		buf[i] = digits[h%36]
		h /= 36
	}
	return string(buf[i:])
}

func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return strings.EqualFold(s[:len(prefix)], prefix)
}
