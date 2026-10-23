package cms

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"cilicili/internal/model"
)

// DefaultUserAgent 是 LibreTV 浏览器端使用的 UA，源站普遍按桌面浏览器放行。
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"

var utf8BOM = []byte("\xef\xbb\xbf")

// RequestHeaders 返回访问 Apple CMS 接口的请求头；ua 为空时使用 DefaultUserAgent。
func RequestHeaders(ua string) http.Header {
	if ua = strings.TrimSpace(ua); ua == "" {
		ua = DefaultUserAgent
	}
	h := make(http.Header, 2)
	h.Set("User-Agent", ua)
	h.Set("Accept", "application/json")
	return h
}

// flexString 兼容源站把字符串、数字、布尔混用在同一字段（vod_id、pagecount 常见）。
type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*f = ""
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	// 对象/数组字段无业务意义，按空值处理。
	if data[0] == '{' || data[0] == '[' {
		*f = ""
		return nil
	}
	// 数字先归一化（3.0 → "3"），与 JS String() 输出一致；true/false 保留原文。
	if n, err := strconv.ParseFloat(string(data), 64); err == nil {
		*f = flexString(strconv.FormatFloat(n, 'f', -1, 64))
		return nil
	}
	*f = flexString(string(data))
	return nil
}

// cleanField 统一 trim 并还原 HTML 实体；返回空串表示该字段不存在。
func cleanField(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(s))
}

// normalizeText 去掉简介里的标签、还原实体并合并空白。
func normalizeText(fragment string) string {
	fragment = htmlTagRE.ReplaceAllString(fragment, " ")
	fragment = html.UnescapeString(fragment)
	return strings.Join(strings.Fields(fragment), " ")
}

func trimJSONPayload(data []byte) []byte {
	data = bytes.TrimPrefix(data, utf8BOM)
	return bytes.TrimSpace(data)
}

func isJSONObject(data []byte) bool {
	return len(data) > 0 && data[0] == '{'
}

func sourceFields(src model.SourceConfig) (key, name, url string) {
	return strings.TrimSpace(src.Key), strings.TrimSpace(src.Name), strings.TrimSpace(src.URL)
}

// ---------------------------------------------------------------- 搜索

type searchResponse struct {
	List      json.RawMessage `json:"list"`
	PageCount flexString      `json:"pagecount"`
}

type rawSearchItem struct {
	VodID      flexString `json:"vod_id"`
	VodName    flexString `json:"vod_name"`
	VodPic     flexString `json:"vod_pic"`
	TypeName   flexString `json:"type_name"`
	VodYear    flexString `json:"vod_year"`
	VodArea    flexString `json:"vod_area"`
	VodRemarks flexString `json:"vod_remarks"`
}

// ParseSearchList 解析 Apple CMS 的搜索响应（ac=videolist）。
// list 为 null/缺失时视为空结果；list 不是数组时返回错误。
func ParseSearchList(data []byte, src model.SourceConfig) ([]model.SearchResultItem, int, error) {
	payload := trimJSONPayload(data)
	if !isJSONObject(payload) {
		return nil, 0, errors.New("API返回的数据格式无效：响应不是 JSON 对象")
	}
	var resp searchResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, 0, fmt.Errorf("API返回的数据格式无效: %w", err)
	}

	items := make([]model.SearchResultItem, 0)
	rawList := bytes.TrimSpace(resp.List)
	switch {
	case len(rawList) == 0 || bytes.Equal(rawList, []byte("null")):
		// 部分源站无结果时返回 list: null，视为空结果。
	case rawList[0] != '[':
		return nil, 0, errors.New("API返回的数据格式无效：list 字段不是数组")
	default:
		var rows []json.RawMessage
		if err := json.Unmarshal(rawList, &rows); err != nil {
			return nil, 0, fmt.Errorf("API返回的数据格式无效: %w", err)
		}
		items = make([]model.SearchResultItem, 0, len(rows))
		for _, row := range rows {
			row = bytes.TrimSpace(row)
			// 单条记录不是对象时跳过，避免一条脏数据拖垮整个源。
			if len(row) == 0 || row[0] != '{' {
				continue
			}
			var raw rawSearchItem
			if err := json.Unmarshal(row, &raw); err != nil {
				return nil, 0, fmt.Errorf("API返回的数据格式无效: %w", err)
			}
			item := toSearchResultItem(raw, src)
			if item.VodID == "" && item.Name == "" {
				continue
			}
			items = append(items, item)
		}
	}

	return items, parsePageCount(string(resp.PageCount)), nil
}

func toSearchResultItem(raw rawSearchItem, src model.SourceConfig) model.SearchResultItem {
	key, name, url := sourceFields(src)
	return model.SearchResultItem{
		SourceKey:  key,
		SourceName: name,
		VodID:      cleanField(string(raw.VodID)),
		Name:       cleanField(string(raw.VodName)),
		Pic:        cleanField(string(raw.VodPic)),
		TypeName:   cleanField(string(raw.TypeName)),
		Year:       cleanField(string(raw.VodYear)),
		Area:       cleanField(string(raw.VodArea)),
		Remarks:    cleanField(string(raw.VodRemarks)),
		SourceURL:  url,
		IsAdult:    src.IsAdult,
	}
}

// parsePageCount 复刻 JS parseInt 的宽松语义：缺失、非法或小于 1 时按 1 页。
func parsePageCount(s string) int {
	n, ok := parseLeadingInt(strings.TrimSpace(s))
	if !ok || n < 1 {
		return 1
	}
	return n
}

func parseLeadingInt(s string) (int, bool) {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return 0, false
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------- 详情

type detailResponse struct {
	List json.RawMessage `json:"list"`
}

type rawDetailItem struct {
	VodName     flexString `json:"vod_name"`
	VodPic      flexString `json:"vod_pic"`
	VodContent  flexString `json:"vod_content"`
	TypeName    flexString `json:"type_name"`
	VodYear     flexString `json:"vod_year"`
	VodArea     flexString `json:"vod_area"`
	VodDirector flexString `json:"vod_director"`
	VodActor    flexString `json:"vod_actor"`
	VodRemarks  flexString `json:"vod_remarks"`
	VodPlayURL  flexString `json:"vod_play_url"`
}

// ParseDetail 解析 Apple CMS 的详情响应（ac=videolist&ids=...）。
// 播放地址解析失败时，从 vod_content 里的 m3u8 链接兜底。
func ParseDetail(data []byte, src model.SourceConfig) (*model.VideoDetail, error) {
	payload := trimJSONPayload(data)
	if !isJSONObject(payload) {
		return nil, errors.New("获取到的详情内容无效：响应不是 JSON 对象")
	}
	var resp detailResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("获取到的详情内容无效: %w", err)
	}
	rawList := bytes.TrimSpace(resp.List)
	if len(rawList) == 0 || rawList[0] != '[' {
		return nil, errors.New("获取到的详情内容无效：list 为空")
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(rawList, &rows); err != nil {
		return nil, fmt.Errorf("获取到的详情内容无效: %w", err)
	}
	if len(rows) == 0 {
		return nil, errors.New("获取到的详情内容无效：list 为空")
	}
	row := bytes.TrimSpace(rows[0])
	if len(row) == 0 || row[0] != '{' {
		return nil, errors.New("获取到的详情内容无效：list[0] 不是对象")
	}
	var raw rawDetailItem
	if err := json.Unmarshal(row, &raw); err != nil {
		return nil, fmt.Errorf("获取到的详情内容无效: %w", err)
	}

	episodes := ExtractEpisodesFromPlayURL(string(raw.VodPlayURL))
	content := html.UnescapeString(string(raw.VodContent))
	if len(episodes) == 0 {
		episodes = ExtractM3U8FromText(content)
	}
	key, name, url := sourceFields(src)
	return &model.VideoDetail{
		Episodes: episodes,
		VideoInfo: model.VideoInfo{
			Title: cleanField(string(raw.VodName)),
			Cover: cleanField(string(raw.VodPic)),
			// vod_content 是富文本；与详情页 HTML 兜底路径一样输出纯文本简介。
			Desc:       normalizeText(string(raw.VodContent)),
			TypeName:   cleanField(string(raw.TypeName)),
			Year:       cleanField(string(raw.VodYear)),
			Area:       cleanField(string(raw.VodArea)),
			Director:   cleanField(string(raw.VodDirector)),
			Actor:      cleanField(string(raw.VodActor)),
			Remarks:    cleanField(string(raw.VodRemarks)),
			SourceKey:  key,
			SourceName: name,
			SourceURL:  url,
		},
	}, nil
}

// ---------------------------------------------------------------- 播放地址

// ExtractEpisodesFromPlayURL 从 vod_play_url 提取一线路的分集直链。
// 格式为「线路1$$$线路2」，每线路「集1$url#集2$url」；优先取含 .m3u8 的线路，
// 均无 m3u8 时取第一条线路（部分源首页线路是网页中转页，不可直接播放）。
func ExtractEpisodesFromPlayURL(playURL string) []string {
	// 部分源站把播放地址里的 & 转义成 &amp;，先还原再解析，否则播放器拿到的是坏 URL。
	playURL = html.UnescapeString(strings.TrimSpace(playURL))
	if playURL == "" {
		return []string{}
	}
	groups := make([][]string, 0, 2)
	for _, group := range strings.Split(playURL, "$$$") {
		episodes := make([]string, 0, 8)
		for _, segment := range strings.Split(group, "#") {
			parts := strings.Split(segment, "$")
			if len(parts) < 2 {
				continue
			}
			url := strings.TrimSpace(parts[1])
			if !isHTTPURL(url) {
				continue
			}
			episodes = append(episodes, url)
		}
		if len(episodes) > 0 {
			groups = append(groups, episodes)
		}
	}
	if len(groups) == 0 {
		return []string{}
	}
	for _, episodes := range groups {
		for _, url := range episodes {
			if strings.Contains(url, ".m3u8") {
				return episodes
			}
		}
	}
	return groups[0]
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

var m3u8InTextRE = regexp.MustCompile(`\$https?://[^"'\s]+?\.m3u8`)

// ExtractM3U8FromText 从简介等文本里提取以 $ 前缀出现的 m3u8 链接。
func ExtractM3U8FromText(text string) []string {
	matches := m3u8InTextRE.FindAllString(text, -1)
	links := make([]string, 0, len(matches))
	for _, m := range matches {
		links = append(links, strings.TrimPrefix(m, "$"))
	}
	return links
}

// ---------------------------------------------------------------- 详情页 HTML 兜底

var (
	m3u8InPageRE   = m3u8InTextRE
	dateHashM3U8RE = regexp.MustCompile(`\$https?://[^"'\s]+?/\d{8}/\d+_[a-f0-9]+/index\.m3u8`)
	htmlTitleRE    = regexp.MustCompile(`<h1[^>]*>([^<]+)</h1>`)
	htmlSketchRE   = regexp.MustCompile(`(?s)<div[^>]*class=["']sketch["'][^>]*>(.*?)</div>`)
	htmlTagRE      = regexp.MustCompile(`<[^>]+>`)
)

// ParseDetailPageHTML 从详情页 HTML 兜底提取播放地址与标题/简介。
// 部分源的列表接口不返回可播地址，需要抓详情页；链接去重并截断 ( 之后的尾缀。
func ParseDetailPageHTML(page []byte, src model.SourceConfig) *model.VideoDetail {
	text := string(bytes.TrimPrefix(page, utf8BOM))
	matches := m3u8InPageRE.FindAllString(text, -1)
	if len(matches) == 0 {
		matches = dateHashM3U8RE.FindAllString(text, -1)
	}

	episodes := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		url := strings.TrimPrefix(m, "$")
		if idx := strings.IndexByte(url, '('); idx > 0 {
			url = url[:idx]
		}
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		if _, ok := seen[url]; ok {
			continue
		}
		seen[url] = struct{}{}
		episodes = append(episodes, url)
	}

	key, name, url := sourceFields(src)
	info := model.VideoInfo{SourceKey: key, SourceName: name, SourceURL: url}
	if m := htmlTitleRE.FindStringSubmatch(text); m != nil {
		info.Title = cleanField(m[1])
	}
	if m := htmlSketchRE.FindStringSubmatch(text); m != nil {
		info.Desc = normalizeText(m[1])
	}
	return &model.VideoDetail{Episodes: episodes, VideoInfo: info}
}
