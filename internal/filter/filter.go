package filter

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/mozillazg/go-pinyin"

	"cilicili/internal/model"
)

// adultKeywords 与 LibreTV cms-parser.ts 的 ADULT_KEYWORDS 保持一致（原文含重复项，此处去重）。
// 匹配源站分类名（type_name），忽略大小写。
var adultKeywords = []string{
	"伦理片", "福利", "倫理片", "福利片", "日本伦理", "日本福利", "日本福利片", "日本伦理片",
	"擦边短剧", "擦边", "韩国伦理", "里番动漫", "门事件", "萝莉少女", "制服诱惑", "国产传媒",
	"cosplay", "黑丝诱惑", "无码", "日本无码", "有码", "日本有码", "SWAG",
	"网红主播", "色情片", "同性片", "福利视频", "无码高清", "有码高清", "91视频", "自拍偷拍",
	"mini传媒",
}

var adultKeywordsLower = func() []string {
	lower := make([]string, 0, len(adultKeywords))
	seen := make(map[string]struct{}, len(adultKeywords))
	for _, kw := range adultKeywords {
		kw = strings.ToLower(kw)
		if _, ok := seen[kw]; ok {
			continue
		}
		seen[kw] = struct{}{}
		lower = append(lower, kw)
	}
	return lower
}()

// Adult 按分类名过滤成人内容；enabled 为 false 时原样返回。
func Adult(items []model.SearchResultItem, enabled bool) []model.SearchResultItem {
	if !enabled {
		return items
	}
	out := make([]model.SearchResultItem, 0, len(items))
	for _, item := range items {
		if isAdultType(item.TypeName) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func isAdultType(typeName string) bool {
	if typeName == "" {
		return false
	}
	lower := strings.ToLower(typeName)
	for _, kw := range adultKeywordsLower {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// NormalizeTitle 归一化标题/关键词：转小写并去掉空白、标点与符号。
func NormalizeTitle(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// ---------------------------------------------------------------- 相关性

var (
	explicitSeasonAnnotatedRE = regexp.MustCompile(`^(.*?)\s*第\s*([1-9]\d?|[一二两三四五六七八九十]{1,3})\s*季.*$`)
	explicitSeasonStrictRE    = regexp.MustCompile(`^(.*?)\s*第\s*([1-9]\d?|[一二两三四五六七八九十]{1,3})\s*季$`)
	bareSeasonSpacedRE        = regexp.MustCompile(`^(.*?)\s+([1-9]\d?)$`)
	bareSeasonTightRE         = regexp.MustCompile(`^(.*?)\s*([1-9]\d?)$`)
	chineseTenRE              = regexp.MustCompile(`^[一二三四五六七八九]?十[一二三四五六七八九]?$`)
)

// seasonInfo 是标题/关键词末尾的季数信息。
type seasonInfo struct {
	base     string // 去掉季数后缀的片名
	season   int
	explicit bool // true 表示「第 N 季」，false 表示裸数字
}

// parseSeasonSuffix 解析独立的末尾数字或「第 N 季」后缀。
//
// bareNeedsSpace 区分两侧：查询侧传 false，用户习惯直接输入「庆余年2」；
// 标题侧传 true，必须有空格，避免把片名自带的数字（「电影2」）误判成第二季。
// allowAnnotations 为 true 时标题侧允许「第二季粤语」这类附注。
func parseSeasonSuffix(title string, allowAnnotations, bareNeedsSpace bool) (seasonInfo, bool) {
	// Go 的 \s 只认 ASCII 空白，先把 Unicode 空白统一成空格，语义才与 TS 的 \s 一致。
	title = normalizeSpaces(title)
	if title == "" {
		return seasonInfo{}, false
	}

	explicitRE := explicitSeasonStrictRE
	if allowAnnotations {
		explicitRE = explicitSeasonAnnotatedRE
	}
	if m := explicitRE.FindStringSubmatch(title); m != nil {
		season, ok := parseSeasonValue(m[2])
		if !ok || NormalizeTitle(m[1]) == "" {
			return seasonInfo{}, false
		}
		return seasonInfo{base: m[1], season: season, explicit: true}, true
	}

	bareRE := bareSeasonSpacedRE
	if !bareNeedsSpace {
		bareRE = bareSeasonTightRE
	}
	if m := bareRE.FindStringSubmatch(title); m != nil {
		if NormalizeTitle(m[1]) == "" {
			return seasonInfo{}, false
		}
		season, ok := parseSeasonValue(m[2])
		if !ok {
			return seasonInfo{}, false
		}
		return seasonInfo{base: m[1], season: season}, true
	}
	return seasonInfo{}, false
}

func normalizeSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsSpace(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// parseSeasonValue 解析季数：阿拉伯数字、中文数字（一/两/十二/二十三）。
func parseSeasonValue(value string) (int, bool) {
	if n, err := strconv.Atoi(value); err == nil {
		return n, true
	}
	value = strings.ReplaceAll(value, "两", "二")
	if runes := []rune(value); len(runes) == 1 && runes[0] != '十' {
		return chineseDigit(runes[0])
	}
	if !chineseTenRE.MatchString(value) {
		return 0, false
	}
	parts := strings.SplitN(value, "十", 2)
	tens := 1
	if parts[0] != "" {
		v, ok := chineseDigit([]rune(parts[0])[0])
		if !ok {
			return 0, false
		}
		tens = v
	}
	units := 0
	if parts[1] != "" {
		v, ok := chineseDigit([]rune(parts[1])[0])
		if !ok {
			return 0, false
		}
		units = v
	}
	return tens*10 + units, true
}

// chineseDigit 返回中文数字字符对应的 1-9，其它字符返回 false。
func chineseDigit(r rune) (int, bool) {
	switch r {
	case '一':
		return 1, true
	case '二':
		return 2, true
	case '三':
		return 3, true
	case '四':
		return 4, true
	case '五':
		return 5, true
	case '六':
		return 6, true
	case '七':
		return 7, true
	case '八':
		return 8, true
	case '九':
		return 9, true
	default:
		return 0, false
	}
}

// preparedQuery 是与具体结果无关的关键词预处理结果，过滤上千条时只算一次。
type preparedQuery struct {
	normalized string
	season     seasonInfo
	hasSeason  bool
	tokens     []string
}

func prepareQuery(wd string) (preparedQuery, bool) {
	normalized := NormalizeTitle(wd)
	if normalized == "" {
		return preparedQuery{}, false
	}
	q := preparedQuery{normalized: normalized}
	// 查询侧允许「庆余年2」这种无空格写法。
	if season, ok := parseSeasonSuffix(wd, false, false); ok {
		q.season = season
		q.hasSeason = true
	}
	for _, token := range strings.Fields(wd) {
		if token = NormalizeTitle(token); token != "" {
			q.tokens = append(q.tokens, token)
		}
	}
	return q, true
}

// matches 判断已归一化的标题是否命中关键词：整体包含，或每个分词都包含。
func (q preparedQuery) matches(normalizedName string) bool {
	if includesKeyword(normalizedName, q.normalized) {
		return true
	}
	if len(q.tokens) < 2 {
		return false
	}
	for _, token := range q.tokens {
		if !includesKeyword(normalizedName, token) {
			return false
		}
	}
	return true
}

// includesKeyword 判断 keyword 是否完整出现在 title 中：数字必须完整命中，
// 避免第 2 季误匹配 12 或 20。
func includesKeyword(title, keyword string) bool {
	if title == "" || keyword == "" {
		return false
	}
	startsDigit := isASCIIDigit(keyword[0])
	endsDigit := isASCIIDigit(keyword[len(keyword)-1])
	for offset := 0; offset <= len(title)-len(keyword); {
		idx := strings.Index(title[offset:], keyword)
		if idx < 0 {
			return false
		}
		pos := offset + idx
		end := pos + len(keyword)
		beforeDigit := pos > 0 && isASCIIDigit(title[pos-1])
		afterDigit := end < len(title) && isASCIIDigit(title[end])
		if (!startsDigit || !beforeDigit) && (!endsDigit || !afterDigit) {
			return true
		}
		offset = pos + 1
	}
	return false
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

func isRelevant(name string, q preparedQuery) bool {
	normalized := NormalizeTitle(name)
	if normalized == "" {
		return false
	}
	titleSeason, hasTitleSeason := parseSeasonSuffix(name, true, true)
	if q.hasSeason && hasTitleSeason {
		if q.season.season != titleSeason.season {
			return false
		}
		if q.season.explicit || titleSeason.explicit {
			if baseQuery, ok := prepareQuery(q.season.base); ok {
				return baseQuery.matches(NormalizeTitle(titleSeason.base))
			}
		}
	}
	return q.matches(normalized)
}

// Relevant 丢弃与关键词无关的结果；关键词为空或纯标点时不排序也不过滤。
func Relevant(items []model.SearchResultItem, wd string) []model.SearchResultItem {
	q, ok := prepareQuery(wd)
	if !ok {
		return items
	}
	out := make([]model.SearchResultItem, 0, len(items))
	for _, item := range items {
		if isRelevant(item.Name, q) {
			out = append(out, item)
		}
	}
	return out
}

// ---------------------------------------------------------------- 排序

// pinyinArgs 只读共享：go-pinyin 的 Args 在转换过程中不会被修改，可安全并发读。
var pinyinArgs = pinyin.NewArgs()

// sortKey 把名称转成可比较的排序键：汉字转拼音（小写、无声调），
// 拉丁字母/数字保留小写，标点、空白与符号忽略。中文按拼音序而非码位序。
func sortKey(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s) * 2)
	for _, r := range s {
		if py := pinyin.SinglePinyin(r, pinyinArgs); len(py) > 0 {
			b.WriteString(py[0])
			b.WriteByte(' ')
			continue
		}
		r = unicode.ToLower(r)
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Sort 排序：关键词精确命中优先，再按名称拼音序，最后按源名拼音序。
// 完全相同时保持原顺序（稳定排序）。
func Sort(items []model.SearchResultItem, wd string) {
	if len(items) < 2 {
		return
	}
	exact := NormalizeTitle(wd)

	type keyedItem struct {
		item      model.SearchResultItem
		exactHit  bool
		nameKey   string
		sourceKey string
	}
	keyed := make([]keyedItem, len(items))
	for i, item := range items {
		keyed[i] = keyedItem{
			item:      item,
			exactHit:  exact != "" && NormalizeTitle(item.Name) == exact,
			nameKey:   sortKey(item.Name),
			sourceKey: sortKey(item.SourceName),
		}
	}

	sort.SliceStable(keyed, func(i, j int) bool {
		a, b := keyed[i], keyed[j]
		if a.exactHit != b.exactHit {
			return a.exactHit
		}
		if a.nameKey != b.nameKey {
			return a.nameKey < b.nameKey
		}
		if a.sourceKey != b.sourceKey {
			return a.sourceKey < b.sourceKey
		}
		// 拼音键相同时用原始名称兜底，保证顺序确定（例如「庆余年 第二季」与「庆余年第二季」）。
		if a.item.Name != b.item.Name {
			return a.item.Name < b.item.Name
		}
		return a.item.SourceName < b.item.SourceName
	})
	for i := range keyed {
		items[i] = keyed[i].item
	}
}
