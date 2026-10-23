// Package m3u8 识别并重写 HLS 播放列表里的资源地址。
//
// 播放器只应看到本站的代理地址：清单里的分片、子清单、密钥、初始化段与
// 媒体 rendition 都必须继续走本站代理（规避上游 CORS/防盗链），否则浏览器
// 直连上游会失败。相对地址以「重定向后的最终 URL」为基准解析——gslb 调度
// 源 302 之后目录会变，用请求前的 URL 解析会把分片指向错误路径。
package m3u8

import (
	"net/url"
	"regexp"
	"strings"
)

// 本站代理地址前缀（新旧两种形式，见 proxy 包）。已经是代理地址的 URI
// 不再二次包裹，否则每次回源都会套一层。
var proxiedPrefixes = []string{"/api/proxy", "/api/live/stream"}

// uriTags 是允许改写 URI="..." 属性的标签。
//
// 只处理明确承载资源地址的标签，而不是「任何带 URI= 的标签」：
// EXT-X-CONTENT-STEERING 的 SERVER-URI 指向 JSON 转向清单，不属于
// 本包要代理的目标；整行匹配前缀才能避免误伤。
var uriTags = []string{
	"#EXT-X-KEY:",
	"#EXT-X-SESSION-KEY:",
	"#EXT-X-MAP:",
	"#EXT-X-MEDIA:",
	"#EXT-X-I-FRAME-STREAM-INF:",
	"#EXT-X-IMAGE-STREAM-INF:",
	"#EXT-X-PART:",
	"#EXT-X-PRELOAD-HINT:",
	"#EXT-X-RENDITION-REPORT:",
	"#EXT-X-SESSION-DATA:",
}

var uriAttrRe = regexp.MustCompile(`URI="([^"]*)"`)

// IsPlaylist 判断上游响应是否 HLS 播放列表。
// content-type 命中 mpegurl/m3u8 即认为是；content-type 缺失或不可信时
// 退化为检查 URL 路径后缀（忽略 query/fragment）。
func IsPlaylist(contentType, rawURL string) bool {
	ct := strings.ToLower(contentType)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	if strings.Contains(ct, "mpegurl") || strings.Contains(ct, "m3u8") {
		return true
	}

	path := rawURL
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	return strings.HasSuffix(strings.ToLower(path), ".m3u8")
}

// Rewrite 把文本里的播放列表地址改写为 proxyURLBase + url.QueryEscape(绝对地址)。
//
// baseURL 是上游最终 URL（相对地址的解析基准），proxyURLBase 形如
// "/api/proxy?url="。空行、注释、标签顺序与 CRLF 换行都原样保留；
// 无法解析的坏 URL、非 http(s) 地址与已是代理地址的 URI 原样返回。
func Rewrite(text, baseURL, proxyURLBase string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		cr := ""
		if strings.HasSuffix(line, "\r") {
			line, cr = line[:len(line)-1], "\r"
		}
		lines[i] = rewriteLine(line, baseURL, proxyURLBase) + cr
	}
	return strings.Join(lines, "\n")
}

// rewriteLine 处理单行（不含行尾 \r）。空行与注释行原样保留，
// 仅重写 URI 行与带 RFC 8216 URI 属性的标签。
func rewriteLine(line, baseURL, proxyURLBase string) string {
	if strings.TrimSpace(line) == "" {
		return line
	}
	// 部分采集站的 m3u8 带 UTF-8 BOM：保留 BOM，按标签/地址正常分类改写。
	bom := ""
	if strings.HasPrefix(line, "\uFEFF") {
		bom, line = "\uFEFF", strings.TrimPrefix(line, "\uFEFF")
	}
	if strings.HasPrefix(line, "#") {
		if !hasURITag(line) {
			return bom + line
		}
		return bom + rewriteTagURIs(line, baseURL, proxyURLBase)
	}
	return bom + rewriteURI(line, baseURL, proxyURLBase)
}

func hasURITag(line string) bool {
	if !strings.Contains(line, `URI="`) {
		return false
	}
	for _, tag := range uriTags {
		if strings.HasPrefix(line, tag) {
			return true
		}
	}
	return false
}

func rewriteTagURIs(line, baseURL, proxyURLBase string) string {
	return uriAttrRe.ReplaceAllStringFunc(line, func(m string) string {
		value := m[len(`URI="`) : len(m)-1]
		return `URI="` + rewriteURI(value, baseURL, proxyURLBase) + `"`
	})
}

// rewriteURI 把一个资源地址改写为代理地址；无法安全改写时原样返回。
func rewriteURI(raw, baseURL, proxyURLBase string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || isProxied(raw) {
		return raw
	}
	abs, ok := resolveAbsolute(raw, baseURL)
	if !ok {
		return raw
	}
	return proxyURLBase + url.QueryEscape(abs)
}

func isProxied(raw string) bool {
	for _, prefix := range proxiedPrefixes {
		if !strings.HasPrefix(raw, prefix) {
			continue
		}
		// 只认真正的路由边界，避免把 "/api/proxyfoo" 之类本地路径误判为代理地址。
		rest := raw[len(prefix):]
		if rest == "" || rest[0] == '/' || rest[0] == '?' || rest[0] == '#' {
			return true
		}
	}
	return false
}

// resolveAbsolute 以 baseURL 为基准把 raw 解析成绝对 http(s) 地址。
// 坏 URL、非 http(s) 协议（data:/skd:/blob:）与无法锚定的相对地址返回 false。
func resolveAbsolute(raw, baseURL string) (string, bool) {
	ref, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if ref.IsAbs() {
		if !isHTTP(ref) || ref.Host == "" {
			return "", false
		}
		return ref.String(), true
	}

	base, err := url.Parse(baseURL)
	if err != nil || !base.IsAbs() || !isHTTP(base) {
		return "", false
	}
	abs := base.ResolveReference(ref)
	if !abs.IsAbs() || !isHTTP(abs) || abs.Host == "" {
		return "", false
	}
	return abs.String(), true
}

func isHTTP(u *url.URL) bool {
	// url.Parse 会把 scheme 归一化为小写。
	return u.Scheme == "http" || u.Scheme == "https"
}
