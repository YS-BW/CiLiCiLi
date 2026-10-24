package media

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	// 年份：标题末尾 (2019) / （2019） / 无括号的独立四位年份。
	trailingYearParenRE = regexp.MustCompile(`[（(]\s*(\d{4})\s*[)）]\s*$`)
	trailingYearRE      = regexp.MustCompile(`(?:^|[\s\-·])((?:19|20)\d{2})\s*$`)
	// 季数后缀：第二季 / 第2季 / Season 2 / S02。
	seasonRE = regexp.MustCompile(`(?i)(?:第\s*([0-9一二两三四五六七八九十]+)\s*季|season\s*([0-9]+)|s([0-9]{1,2}))$`)
	// 裸季数：中文/字母后紧跟 1-2 位数字，如「庆余年2」「Show 3」。
	bareSeasonRE = regexp.MustCompile(`([\p{Han}A-Za-z])\s*([0-9]{1,2})$`)
	nonTitleRE   = regexp.MustCompile(`[^\p{L}\p{N}]+`)
)

// KindFromType 根据采集站的 type_name 粗略判断媒体类型。
// 只做展示分组，不参与严格匹配；无法判断时返回 other。
func KindFromType(typeName string) Kind {
	t := strings.ToLower(strings.TrimSpace(typeName))
	switch {
	case t == "":
		return KindOther
	case strings.Contains(t, "动漫"), strings.Contains(t, "动画"), strings.Contains(t, "番剧"),
		strings.Contains(t, "国漫"), strings.Contains(t, "日漫"), strings.Contains(t, "动画片"):
		return KindAnime
	case strings.Contains(t, "电影"), strings.Contains(t, "片"), strings.Contains(t, "movie"):
		return KindMovie
	case strings.Contains(t, "剧"), strings.Contains(t, "综艺"), strings.Contains(t, "纪录"),
		strings.Contains(t, "tv"), strings.Contains(t, "show"):
		return KindTV
	default:
		return KindOther
	}
}

// NormalizeTitle 归一化标题：去掉季数/年份后缀、标点、空白并转小写。
// 返回去掉季数后的片名和季数（无法识别时为 0）。
func NormalizeTitle(title string) (base string, season int) {
	s := strings.TrimSpace(title)
	if s == "" {
		return "", 0
	}
	// 1) 去掉尾部括号年份：(2019) / （2019）。
	s = strings.TrimSpace(trailingYearParenRE.ReplaceAllString(s, ""))

	// 2) 显式季数：第N季 / Season N / S02。
	if m := seasonRE.FindStringSubmatch(s); m != nil {
		raw := ""
		for i := 1; i < len(m); i++ {
			if m[i] != "" {
				raw = m[i]
				break
			}
		}
		s = strings.TrimSpace(seasonRE.ReplaceAllString(s, ""))
		season = parseSeasonNumber(raw)
	} else if loc := bareSeasonRE.FindStringSubmatchIndex(s); loc != nil {
		// 3) 裸季数：只接受紧跟在中文/字母后的 1-2 位数字，避免把 1921 这类年份当季数。
		season = parseSeasonNumber(s[loc[4]:loc[5]])
		s = strings.TrimSpace(s[:loc[4]])
	}

	// 4) 去掉尾部独立年份；如果去掉后片名为空，说明标题本身就是年份，不处理。
	if loc := trailingYearRE.FindStringSubmatchIndex(s); loc != nil {
		if trimmed := strings.TrimSpace(s[:loc[2]]); trimmed != "" {
			s = trimmed
		}
	}

	base = nonTitleRE.ReplaceAllString(strings.ToLower(s), "")
	return base, season
}

func parseSeasonNumber(raw string) int {
	if n := atoiSafe(raw); n > 0 {
		return n
	}
	// 中文数字只覆盖常见范围（一~九十九），够用即可。
	digits := map[rune]int{'一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	if raw == "十" {
		return 10
	}
	runes := []rune(raw)
	if len(runes) == 1 {
		return digits[runes[0]]
	}
	if len(runes) == 2 && runes[1] == '十' {
		return digits[runes[0]] * 10
	}
	if len(runes) == 2 && runes[0] == '十' {
		return 10 + digits[runes[1]]
	}
	if len(runes) == 3 && runes[1] == '十' {
		return digits[runes[0]]*10 + digits[runes[2]]
	}
	return 0
}

func atoiSafe(s string) int {
	n := 0
	if s == "" {
		return 0
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
		if n > 999 {
			return 0
		}
	}
	return n
}

// ExtractYear 从“2019”“2019-01-01”“(2019)”里提取四位年份。
func ExtractYear(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if len(raw) >= 4 {
		for i := 0; i+4 <= len(raw); i++ {
			chunk := raw[i : i+4]
			if (strings.HasPrefix(chunk, "19") || strings.HasPrefix(chunk, "20")) && isDigits(chunk) {
				return chunk
			}
		}
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// AggregateKey 生成聚合键。没有年份时返回空 key，表示不自动合并。
// sourceKey/vodId 不参与 key，只作为源引用保存。
func AggregateKey(kind Kind, title, year string) string {
	base, season := NormalizeTitle(title)
	if base == "" {
		return ""
	}
	y := ExtractYear(year)
	if y == "" {
		return ""
	}
	// 类型只区分电影/剧集/动漫；other 参与匹配会误合并，直接不聚合。
	if kind == KindOther {
		return ""
	}
	sum := sha1.Sum([]byte(strings.Join([]string{string(kind), base, y, itoa(season)}, "\x00")))
	return "media_" + hex.EncodeToString(sum[:])[:16]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
