package quality

import (
	"encoding/binary"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// 播放列表解析：master playlist 的档位信息（最快的一级）与媒体列表的
// 分片/init/密钥信息（下沉到分片时用）。逻辑对齐 LibreTV 的 quality-probe.ts。

var (
	bandwidthRE  = regexp.MustCompile(`BANDWIDTH=(\d+)`)
	resolutionRE = regexp.MustCompile(`RESOLUTION=(\d+)x(\d+)`)
	uriAttrRE    = regexp.MustCompile(`URI="([^"]+)"`)
	methodRE     = regexp.MustCompile(`METHOD=([^,]+)`)
	ivRE         = regexp.MustCompile(`IV=0x([0-9a-fA-F]+)`)
)

// ParseBestHeight 从 m3u8 文本解析多码率源最高档的短边高度。
//
// 只认 master playlist（含 #EXT-X-STREAM-INF）；媒体列表返回 (0, false)。
// 取短边而非第二个数字：采集站常把宽高写反（1280x720 写成 720x1280），
// 按 height 取会得到 1280，短边才是档位定义的口径。
func ParseBestHeight(text string) (int, bool) {
	if !strings.Contains(text, "#EXT-X-STREAM-INF") {
		return 0, false
	}
	best := 0
	var (
		pending     bool
		pendingBW   int
		pendingHigh int
		pendingRes  bool // 该档位是否声明了 RESOLUTION（声明了就不再按码率推断）
	)
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			pendingBW = firstInt(bandwidthRE, line)
			pendingHigh = 0
			pendingRes = false
			if m := resolutionRE.FindStringSubmatch(line); m != nil {
				pendingRes = true
				w, errW := strconv.Atoi(m[1])
				h, errH := strconv.Atoi(m[2])
				if errW == nil && errH == nil {
					pendingHigh = min(w, h)
				}
			}
			pending = true
			continue
		}
		// 档位标签的下一行才是 variant 地址；注释行与空行不消耗 pending。
		if line == "" || strings.HasPrefix(line, "#") || !pending {
			continue
		}
		height := pendingHigh
		// 只有 BANDWIDTH 的源站按常见转码参数反推高度，与播放器的档位命名口径一致。
		if height == 0 && !pendingRes && pendingBW > 0 {
			height = InferHeightFromBitrate(pendingBW)
		}
		if height > best {
			best = height
		}
		pending = false
	}
	if best <= 0 {
		return 0, false
	}
	return best, true
}

// InferHeightFromBitrate 按码率推断档位高度；分层取自常见转码参数。
// 非正码率返回 0（表示无法推断）。
func InferHeightFromBitrate(bandwidth int) int {
	if bandwidth <= 0 {
		return 0
	}
	tiers := [...]struct{ bps, height int }{
		{20000000, 4320},
		{10000000, 2160},
		{5000000, 1080},
		{3000000, 720},
		{1500000, 540},
		{800000, 480},
		{400000, 360},
	}
	for _, tier := range tiers {
		if bandwidth >= tier.bps {
			return tier.height
		}
	}
	return 240
}

// MediaPlaylistInfo 是媒体列表里探测分片分辨率所需的片段信息。
type MediaPlaylistInfo struct {
	InitURI       string
	FirstSegment  string
	KeyMethod     string
	KeyURI        string
	KeyIV         []byte
	MediaSequence int64
}

// ParseMediaPlaylist 解析媒体列表：init segment、第一个分片、加密方式与 IV。
// 标签按出现顺序覆盖（后端换 key 的流取最后一个），与 LibreTV 一致。
func ParseMediaPlaylist(text string) MediaPlaylistInfo {
	var info MediaPlaylistInfo
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			info.InitURI = firstSubmatch(uriAttrRE, line)
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			info.KeyMethod = strings.TrimSpace(firstSubmatch(methodRE, line))
			info.KeyURI = firstSubmatch(uriAttrRE, line)
			info.KeyIV = nil
			if iv := firstSubmatch(ivRE, line); iv != "" {
				info.KeyIV = decodeHex(iv)
			}
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			value := strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"))
			if seq, err := strconv.ParseInt(value, 10, 64); err == nil {
				info.MediaSequence = seq
			}
		case strings.HasPrefix(line, "#"):
			// 其余标签（EXTINF/DISCONTINUITY/ENDLIST 等）与探测无关。
		default:
			// 第一个非注释行即首个分片（可能是相对路径）。
			if info.FirstSegment == "" {
				info.FirstSegment = line
			}
		}
	}
	return info
}

// defaultIV 按 HLS 规定用媒体序列号构造大端 128 位 IV（未显式给 IV 时）。
func defaultIV(sequence int64) []byte {
	iv := make([]byte, 16)
	// HLS 只取序列号的低 32 位（JS 侧用 >>> 0 截断）。
	binary.BigEndian.PutUint32(iv[12:], uint32(sequence))
	return iv
}

func firstInt(re *regexp.Regexp, s string) int {
	v := firstSubmatch(re, s)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

func firstSubmatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func decodeHex(s string) []byte {
	if s == "" || len(s)%2 != 0 {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}
