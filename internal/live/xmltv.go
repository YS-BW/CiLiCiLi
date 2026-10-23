package live

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cilicili/internal/model"
)

// maxXMLTVBytes 是 gzip 解压后的 XMLTV 上限（防解压炸弹；正常节目单远小于此）。
const maxXMLTVBytes = 128 << 20

// xmltvTimeRe 匹配 XMLTV 时间：`YYYYMMDDHHMMSS(.fff)?( +hhmm | Z)?`。
var xmltvTimeRe = regexp.MustCompile(
	`^(\d{4})(\d{2})(\d{2})(\d{2})?(\d{2})?(\d{2})?(?:\.(\d+))?\s*(?:([+-])(\d{2}):?(\d{2})|(Z))?$`,
)

// xmltvProgramme 是单个 <programme> 节点的解析目标。
// 用流式 XML 解码而不是整树构建：节目单常达数十 MB，逐节点解码后即可丢弃原始文本。
type xmltvProgramme struct {
	Start   string   `xml:"start,attr"`
	Stop    string   `xml:"stop,attr"`
	Channel string   `xml:"channel,attr"`
	Titles  []string `xml:"title"`
	Descs   []string `xml:"desc"`
}

// ParseXMLTV 解析 XMLTV 节目单，只保留 [now-window, now+window] 区间内的节目，
// 按 channel 索引并按开播时间排序。
//
// 支持 gzip：body 以 1f 8b 魔数开头时自动解压（HTTP 层的 gzip 由 Transport
// 自动解压，这里兜住「直接下载 .gz 文件」的情况）。下载被截断时保留已解析的
// 节目（宽松模式）。window<=0 时取 24 小时；now 为零值时取当前时间。
func ParseXMLTV(data []byte, window time.Duration, now time.Time) (map[string][]model.EpgProgram, error) {
	if len(data) == 0 {
		return nil, ErrInvalidXMLTV
	}
	if window <= 0 {
		window = defaultEPGWindow
	}
	if now.IsZero() {
		now = time.Now()
	}
	body, err := maybeGunzipXMLTV(data)
	if err != nil {
		return nil, err
	}

	nowMs := now.UnixMilli()
	windowEndMs := now.Add(window).UnixMilli()

	result := make(map[string][]model.EpgProgram)
	dec := xml.NewDecoder(bytes.NewReader(body))
	// 真实节目单里常有未声明实体、缺结束标签等小毛病，宽松模式让单条脏数据不毁掉整份 EPG。
	dec.Strict = false
	dec.CharsetReader = xmltvCharsetReader

	rootSeen := false
tokenLoop:
	for {
		tok, tokErr := dec.Token()
		if tokErr == io.EOF {
			break
		}
		if tokErr != nil {
			// 下载被截断时保留已解析的节目（与 LibreTV 的正则扫描一致），
			// 单个残缺节点不应让整份 EPG 作废。
			if rootSeen && isUnexpectedEOF(tokErr) {
				break
			}
			return nil, fmt.Errorf("%w: %v", ErrInvalidXMLTV, tokErr)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "tv":
			rootSeen = true
		case "programme":
			var p xmltvProgramme
			if decErr := dec.DecodeElement(&p, &se); decErr != nil {
				if rootSeen && isUnexpectedEOF(decErr) {
					break tokenLoop
				}
				return nil, fmt.Errorf("%w: %v", ErrInvalidXMLTV, decErr)
			}
			channelID := strings.TrimSpace(p.Channel)
			if channelID == "" {
				continue
			}
			start, startOK := parseXMLTVTime(p.Start)
			stop, stopOK := parseXMLTVTime(p.Stop)
			if !startOK || !stopOK {
				continue
			}
			// 窗口裁剪：已播完或完全在窗口之后的节目直接丢弃。
			if stop <= nowMs || start >= windowEndMs {
				continue
			}
			title := firstNonEmpty(p.Titles)
			if title == "" {
				continue
			}
			result[channelID] = append(result[channelID], model.EpgProgram{
				ChannelID: channelID,
				Start:     start,
				Stop:      stop,
				Title:     title,
				Desc:      firstNonEmpty(p.Descs),
			})
		}
	}
	// 没有 <tv> 根节点说明拿到的不是 XMLTV（常见是 200 的 HTML 错误页）。
	if !rootSeen {
		return nil, ErrInvalidXMLTV
	}
	for _, list := range result {
		sort.Slice(list, func(i, j int) bool { return list[i].Start < list[j].Start })
	}
	return result, nil
}

// CurrentAndNext 返回 now 时刻正在播出的节目与下一个节目。
// 重叠节目取最后一个命中者（与 LibreTV 一致）；next 取最早开播的未来节目。
func CurrentAndNext(programs []model.EpgProgram, now time.Time) (*model.EpgProgram, *model.EpgProgram) {
	if now.IsZero() {
		now = time.Now()
	}
	nowMs := now.UnixMilli()
	var current, next *model.EpgProgram
	for i := range programs {
		p := &programs[i]
		if p.Start <= nowMs && p.Stop > nowMs {
			current = p
			continue
		}
		if p.Start > nowMs && (next == nil || p.Start < next.Start) {
			next = p
		}
	}
	return current, next
}

// parseXMLTVTime 解析 XMLTV 时间戳为 Unix 毫秒；格式不合法的返回 false。
//
// 支持 `20240101120000 +0800`、`20240101120000Z`、带毫秒小数以及省略时区
// （无时区按 UTC 处理，与 LibreTV 一致）。
func parseXMLTVTime(raw string) (int64, bool) {
	m := xmltvTimeRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return 0, false
	}
	atoi := func(s string) int {
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0
		}
		return n
	}
	year := atoi(m[1])
	month := atoi(m[2])
	day := atoi(m[3])
	hour := atoi(m[4])
	minute := atoi(m[5])
	second := atoi(m[6])
	if month < 1 || month > 12 || day < 1 || day > 31 ||
		hour > 23 || minute > 59 || second > 60 {
		return 0, false
	}
	nsec := 0
	if frac := m[7]; frac != "" {
		if len(frac) > 9 {
			frac = frac[:9]
		}
		nsec = atoi(frac)
		for i := len(frac); i < 9; i++ {
			nsec *= 10
		}
	}

	loc := time.UTC
	sign := m[8]
	if sign == "+" || sign == "-" {
		offset := atoi(m[9])*3600 + atoi(m[10])*60
		if sign == "-" {
			offset = -offset
		}
		loc = time.FixedZone("", offset)
	}
	return time.Date(year, time.Month(month), day, hour, minute, second, nsec, loc).UnixMilli(), true
}

// maybeGunzipXMLTV 在数据带 gzip 魔数时解压，并限制解压后大小。
func maybeGunzipXMLTV(data []byte) ([]byte, error) {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return data, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: gzip 数据损坏", ErrInvalidXMLTV)
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(io.LimitReader(zr, maxXMLTVBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: 解压失败: %v", ErrInvalidXMLTV, err)
	}
	if len(out) > maxXMLTVBytes {
		return nil, fmt.Errorf("%w: 解压后超过 %d 字节", ErrInvalidXMLTV, maxXMLTVBytes)
	}
	return out, nil
}

// xmltvCharsetReader 支持 XML 声明里的常见字符集。UTF-8 由 XML 包直接处理，
// 这里兜住 ISO-8859-1 系；其它未支持编码返回明确错误而不是乱码解析。
func xmltvCharsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return input, nil
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "windows-1252":
		return &latin1Reader{r: bufio.NewReader(input)}, nil
	default:
		return nil, fmt.Errorf("不支持的 XML 字符集 %q", charset)
	}
}

// latin1Reader 把 ISO-8859-1 字节流转换为 UTF-8。用于老式 XMLTV（少数港台源）。
type latin1Reader struct {
	r       *bufio.Reader
	pending []byte
}

func (l *latin1Reader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(l.pending) > 0 {
			copied := copy(p[n:], l.pending)
			l.pending = l.pending[copied:]
			n += copied
			continue
		}
		b, err := l.r.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		if b < utf8.RuneSelf {
			l.pending = append(l.pending[:0], b)
		} else {
			l.pending = utf8.AppendRune(l.pending[:0], rune(b))
		}
	}
	return n, nil
}

// isUnexpectedEOF 判断 XML 解析错误是否由数据截断造成：encoding/xml 用
// *xml.SyntaxError("unexpected EOF") 报告，不满足 errors.Is(io.ErrUnexpectedEOF)。
func isUnexpectedEOF(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var syntaxErr *xml.SyntaxError
	if errors.As(err, &syntaxErr) {
		return strings.Contains(syntaxErr.Msg, "unexpected EOF")
	}
	return false
}

func firstNonEmpty(values []string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
