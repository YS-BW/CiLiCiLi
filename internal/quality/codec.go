package quality

import (
	"crypto/aes"
	"crypto/cipher"

	"errors"
)

// 从码流里直接挖分辨率：MP4（含 fMP4 的 init segment）读 moov/stsd 的
// VisualSampleEntry，MPEG-TS 读 H.264 SPS。只解析必要的头部字段，
// 不做完整 demux、不解码像素。移植自 LibreTV 的 codec-parse.ts。
//
// 已知不支持 HEVC/H.265 的 SPS：跳过 profile_tier_level 的逻辑繁琐且缺少
// 真实样本验证，写错会静默给出错误分辨率（比「未知」更糟）。这类源返回
// 「测不出」，由播放器实测回填。

// MediaResolution 是从码流里解析出的画面尺寸。
type MediaResolution struct {
	Width  int
	Height int
}

const (
	tsPacketSize    = 188
	maxESBytes      = 256 << 10
	maxStartCodes   = 64
	maxBoxDepth     = 8
	maxSpsCycles    = 256
	maxSpsCropValue = 1 << 20
)

var errBitstream = errors.New("位流解析失败")

// H.264 里带 chroma_format_idc 等扩展字段的 profile（Baseline/Main 没有这些）。
var h264HighProfiles = map[int]bool{
	100: true, 110: true, 122: true, 244: true, 44: true,
	83: true, 86: true, 118: true, 128: true, 138: true, 139: true, 134: true, 135: true,
}

// chromaSubsampling 返回 chroma_format_idc 对应的 [SubWidthC, SubHeightC]，
// 用于把裁剪偏移换算成像素。
func chromaSubsampling(chromaFormatIDC int) (int, int) {
	switch chromaFormatIDC {
	case 1: // 4:2:0
		return 2, 2
	case 2: // 4:2:2
		return 2, 1
	default: // 0 单色 / 3 4:4:4
		return 1, 1
	}
}

// BitReader 按位读取 SPS 的变长位流（exp-golomb 不能按字节读）。
type BitReader struct {
	data []byte
	pos  int
}

// ReadBits 读取 n 位（n<=32）。
func (r *BitReader) ReadBits(n int) (uint64, error) {
	if n < 0 || n > 32 {
		return 0, errBitstream
	}
	if r.pos+n > len(r.data)*8 {
		return 0, errBitstream
	}
	var value uint64
	for i := 0; i < n; i++ {
		byteValue := r.data[r.pos>>3]
		bit := (byteValue >> uint(7-r.pos&7)) & 1
		value = value<<1 | uint64(bit)
		r.pos++
	}
	return value, nil
}

// ReadBit 读取一位。
func (r *BitReader) ReadBit() (bool, error) {
	value, err := r.ReadBits(1)
	return value == 1, err
}

// ReadUE 读取无符号 exp-golomb。
func (r *BitReader) ReadUE() (uint64, error) {
	zeros := 0
	for {
		bit, err := r.ReadBits(1)
		if err != nil {
			return 0, err
		}
		if bit == 1 {
			break
		}
		zeros++
		if zeros > 32 {
			return 0, errBitstream
		}
	}
	if zeros == 0 {
		return 0, nil
	}
	rest, err := r.ReadBits(zeros)
	if err != nil {
		return 0, err
	}
	return (uint64(1) << uint(zeros)) - 1 + rest, nil
}

// ReadSE 读取有符号 exp-golomb。
func (r *BitReader) ReadSE() (int64, error) {
	value, err := r.ReadUE()
	if err != nil {
		return 0, err
	}
	if value&1 == 1 {
		return int64(value+1) / 2, nil
	}
	return -int64(value) / 2, nil
}

// UnescapeRBSP 去掉防竞争字节：编码时会把 00 00 00/00 00 01 插成 00 00 03 xx，
// 解析前必须还原，否则后续 exp-golomb 会读到错误的值。
func UnescapeRBSP(src []byte) []byte {
	out := make([]byte, 0, len(src))
	zeros := 0
	for _, b := range src {
		if zeros == 2 && b == 3 {
			zeros = 0
			continue
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// skipScalingLists 跳过 scaling list：High profile 常见，跳错位数后面全乱。
func skipScalingLists(reader *BitReader, chromaFormatIDC int) error {
	count := 8
	if chromaFormatIDC == 3 {
		count = 12
	}
	for i := 0; i < count; i++ {
		present, err := reader.ReadBit()
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		size := 16
		if i >= 6 {
			size = 64
		}
		for j := 0; j < size; j++ {
			if _, err := reader.ReadSE(); err != nil {
				return err
			}
		}
	}
	return nil
}

// ParseH264SPS 解析 H.264 SPS 得到画面尺寸。
// 读到 vui_parameters 之前就停——后面的字段对分辨率没有影响。
func ParseH264SPS(rbsp []byte) (MediaResolution, bool) {
	res, err := parseH264SPS(rbsp)
	if err != nil {
		return MediaResolution{}, false
	}
	return res, true
}

func parseH264SPS(rbsp []byte) (MediaResolution, error) {
	reader := &BitReader{data: rbsp}
	profileIDC, err := reader.ReadBits(8)
	if err != nil {
		return MediaResolution{}, err
	}
	if _, err := reader.ReadBits(8); err != nil { // constraint_set flags + reserved
		return MediaResolution{}, err
	}
	if _, err := reader.ReadBits(8); err != nil { // level_idc
		return MediaResolution{}, err
	}
	if _, err := reader.ReadUE(); err != nil { // seq_parameter_set_id
		return MediaResolution{}, err
	}

	chromaFormatIDC := 1
	if h264HighProfiles[int(profileIDC)] {
		format, err := reader.ReadUE()
		if err != nil {
			return MediaResolution{}, err
		}
		if format > 3 {
			return MediaResolution{}, errBitstream
		}
		chromaFormatIDC = int(format)
		if format == 3 { // separate_colour_plane_flag
			if _, err := reader.ReadBit(); err != nil {
				return MediaResolution{}, err
			}
		}
		if _, err := reader.ReadUE(); err != nil { // bit_depth_luma_minus8
			return MediaResolution{}, err
		}
		if _, err := reader.ReadUE(); err != nil { // bit_depth_chroma_minus8
			return MediaResolution{}, err
		}
		if _, err := reader.ReadBit(); err != nil { // qpprime_y_zero_transform_bypass_flag
			return MediaResolution{}, err
		}
		scalingPresent, err := reader.ReadBit()
		if err != nil {
			return MediaResolution{}, err
		}
		if scalingPresent {
			if err := skipScalingLists(reader, chromaFormatIDC); err != nil {
				return MediaResolution{}, err
			}
		}
	}

	if _, err := reader.ReadUE(); err != nil { // log2_max_frame_num_minus4
		return MediaResolution{}, err
	}
	picOrderCntType, err := reader.ReadUE()
	if err != nil {
		return MediaResolution{}, err
	}
	switch picOrderCntType {
	case 0:
		if _, err := reader.ReadUE(); err != nil { // log2_max_pic_order_cnt_lsb_minus4
			return MediaResolution{}, err
		}
	case 1:
		if _, err := reader.ReadBit(); err != nil { // delta_pic_order_always_zero_flag
			return MediaResolution{}, err
		}
		if _, err := reader.ReadSE(); err != nil { // offset_for_non_ref_pic
			return MediaResolution{}, err
		}
		if _, err := reader.ReadSE(); err != nil { // offset_for_top_to_bottom_field
			return MediaResolution{}, err
		}
		cycles, err := reader.ReadUE()
		if err != nil {
			return MediaResolution{}, err
		}
		if cycles > maxSpsCycles {
			return MediaResolution{}, errBitstream
		}
		for i := uint64(0); i < cycles; i++ {
			if _, err := reader.ReadSE(); err != nil {
				return MediaResolution{}, err
			}
		}
	case 2:
		// 无序帧，无额外字段。
	default:
		return MediaResolution{}, errBitstream
	}

	if _, err := reader.ReadUE(); err != nil { // max_num_ref_frames
		return MediaResolution{}, err
	}
	if _, err := reader.ReadBit(); err != nil { // gaps_in_frame_num_value_allowed_flag
		return MediaResolution{}, err
	}
	widthInMbs, err := reader.ReadUE()
	if err != nil {
		return MediaResolution{}, err
	}
	heightInMapUnits, err := reader.ReadUE()
	if err != nil {
		return MediaResolution{}, err
	}
	frameMbsOnly, err := reader.ReadBit()
	if err != nil {
		return MediaResolution{}, err
	}
	if !frameMbsOnly {
		if _, err := reader.ReadBit(); err != nil { // mb_adaptive_frame_field_flag
			return MediaResolution{}, err
		}
	}
	if _, err := reader.ReadBit(); err != nil { // direct_8x8_inference_flag
		return MediaResolution{}, err
	}

	var cropLeft, cropRight, cropTop, cropBottom uint64
	cropped, err := reader.ReadBit()
	if err != nil {
		return MediaResolution{}, err
	}
	if cropped {
		values := []*uint64{&cropLeft, &cropRight, &cropTop, &cropBottom}
		for _, target := range values {
			v, err := reader.ReadUE()
			if err != nil {
				return MediaResolution{}, err
			}
			if v > maxSpsCropValue {
				return MediaResolution{}, errBitstream
			}
			*target = v
		}
	}

	if widthInMbs > 4096 || heightInMapUnits > 4096 {
		return MediaResolution{}, errBitstream
	}
	width := int(widthInMbs+1) * 16
	// 隔行扫描时每帧占两个 field，高度要翻倍。
	heightFactor := 1
	if !frameMbsOnly {
		heightFactor = 2
	}
	height := heightFactor * int(heightInMapUnits+1) * 16
	if cropped {
		subWidthC, subHeightC := chromaSubsampling(chromaFormatIDC)
		width -= int(cropLeft+cropRight) * subWidthC
		height -= int(cropTop+cropBottom) * subHeightC * heightFactor
	}
	if width <= 0 || height <= 0 {
		return MediaResolution{}, errBitstream
	}
	return MediaResolution{Width: width, Height: height}, nil
}

// findStartCodes 返回 Annex-B 流的起始码位置（00 00 01 之后的 NAL 头位置）。
func findStartCodes(es []byte, limit int) []int {
	positions := make([]int, 0, limit)
	for i := 2; i < len(es) && len(positions) < limit; i++ {
		if es[i] == 1 && es[i-1] == 0 && es[i-2] == 0 {
			positions = append(positions, i+1)
		}
	}
	return positions
}

// findH264SPS 在 Annex-B 基本流里找 SPS（NAL type 7）的 RBSP。
func findH264SPS(es []byte) []byte {
	starts := findStartCodes(es, maxStartCodes)
	for i, from := range starts {
		to := len(es)
		if i+1 < len(starts) {
			to = starts[i+1] - 3
		}
		if to <= from {
			continue
		}
		nal := es[from:to]
		if len(nal) == 0 {
			continue
		}
		if nal[0]&0x1f == 7 {
			return UnescapeRBSP(nal[1:])
		}
	}
	return nil
}

// ---------------------------------------------------------------- MPEG-TS

type tsPacket struct {
	pid          int
	payloadStart bool
	payload      []byte
}

// findTSOffset 定位 TS 同步位置：允许前面有 padding，靠连续 3 个包头的 0x47 判定。
func findTSOffset(buf []byte) int {
	max := len(buf)
	if max > tsPacketSize*4 {
		max = tsPacketSize * 4
	}
	for i := 0; i < max; i++ {
		if i+2*tsPacketSize >= len(buf) {
			break
		}
		if buf[i] == 0x47 && buf[i+tsPacketSize] == 0x47 && buf[i+2*tsPacketSize] == 0x47 {
			return i
		}
	}
	// 退化：只要第一个字节是 0x47 就试（有些分片起始处只有一两个包）。
	if len(buf) > 0 && buf[0] == 0x47 {
		return 0
	}
	return -1
}

func readTSPackets(buf []byte, start int) []tsPacket {
	packets := make([]tsPacket, 0, 8)
	for i := start; i+tsPacketSize <= len(buf); i += tsPacketSize {
		if buf[i] != 0x47 { // 同步丢失，跳过这个包
			continue
		}
		pid := int(buf[i+1]&0x1f)<<8 | int(buf[i+2])
		payloadStart := buf[i+1]&0x40 != 0
		hasAdaptation := buf[i+3]&0x20 != 0
		hasPayload := buf[i+3]&0x10 != 0
		if !hasPayload {
			continue
		}
		begin := i + 4
		if hasAdaptation {
			begin += 1 + int(buf[begin])
		}
		if begin >= i+tsPacketSize || begin >= len(buf) {
			continue
		}
		packets = append(packets, tsPacket{
			pid:          pid,
			payloadStart: payloadStart,
			payload:      buf[begin : i+tsPacketSize],
		})
	}
	return packets
}

// parsePAT 返回第一个节目对应的 PMT PID。
func parsePAT(payload []byte) int {
	if len(payload) == 0 {
		return -1
	}
	begin := 1 + int(payload[0]) // 跳过 pointer_field
	if begin+8 > len(payload) {
		return -1
	}
	if payload[begin] != 0x00 {
		return -1
	}
	sectionLength := int(payload[begin+1]&0x0f)<<8 | int(payload[begin+2])
	end := begin + 3 + sectionLength - 4 // 末尾 4 字节是 CRC
	if end > len(payload) {
		end = len(payload)
	}
	for p := begin + 8; p+4 <= end; p += 4 {
		programNumber := int(payload[p])<<8 | int(payload[p+1])
		if programNumber == 0 { // NIT，不是节目
			continue
		}
		return int(payload[p+2]&0x1f)<<8 | int(payload[p+3])
	}
	return -1
}

// parsePMT 返回 H.264 视频轨的 PID（0x1b）。HEVC(0x24) 不解析。
func parsePMT(payload []byte) int {
	if len(payload) == 0 {
		return -1
	}
	begin := 1 + int(payload[0])
	if begin+12 > len(payload) {
		return -1
	}
	if payload[begin] != 0x02 {
		return -1
	}
	sectionLength := int(payload[begin+1]&0x0f)<<8 | int(payload[begin+2])
	end := begin + 3 + sectionLength - 4
	if end > len(payload) {
		end = len(payload)
	}
	programInfoLength := int(payload[begin+10]&0x0f)<<8 | int(payload[begin+11])
	p := begin + 12 + programInfoLength
	for p+5 <= end && p+5 <= len(payload) {
		streamType := payload[p]
		pid := int(payload[p+1]&0x1f)<<8 | int(payload[p+2])
		esInfoLength := int(payload[p+3]&0x0f)<<8 | int(payload[p+4])
		if streamType == 0x1b {
			return pid
		}
		p += 5 + esInfoLength
	}
	return -1
}

// collectElementaryStream 把指定 PID（或全部 PID）的 PES 载荷拼成连续的基本流。
func collectElementaryStream(buf []byte, start, videoPID int) []byte {
	chunks := make([][]byte, 0, 8)
	total := 0
	for _, packet := range readTSPackets(buf, start) {
		// videoPID < 0 表示 PMT 解析失败，退化为拼所有包——多数源只有一个视频轨。
		if videoPID >= 0 && packet.pid != videoPID {
			continue
		}
		payload := packet.payload
		if packet.payloadStart {
			// PES 头：起始码 00 00 01 + stream_id + 长度 + 可选头。
			if len(payload) > 9 && payload[0] == 0 && payload[1] == 0 && payload[2] == 1 {
				esBegin := 9 + int(payload[8])
				if esBegin >= len(payload) {
					continue
				}
				payload = payload[esBegin:]
			}
		}
		chunks = append(chunks, payload)
		total += len(payload)
		if total >= maxESBytes {
			break
		}
	}
	es := make([]byte, 0, total)
	for _, chunk := range chunks {
		es = append(es, chunk...)
		if len(es) >= maxESBytes {
			es = es[:maxESBytes]
			break
		}
	}
	return es
}

// ParseTSVideoResolution 解析 TS 分片得到分辨率。
func ParseTSVideoResolution(buf []byte) (MediaResolution, bool) {
	res, err := parseTSVideoResolution(buf)
	if err != nil {
		return MediaResolution{}, false
	}
	return res, true
}

func parseTSVideoResolution(buf []byte) (MediaResolution, error) {
	start := findTSOffset(buf)
	if start < 0 {
		return MediaResolution{}, errBitstream
	}

	pmtPID := -1
	videoPID := -1
	for _, packet := range readTSPackets(buf, start) {
		if !packet.payloadStart {
			continue
		}
		if pmtPID < 0 && packet.pid == 0x0000 {
			pmtPID = parsePAT(packet.payload)
			continue
		}
		if pmtPID >= 0 && packet.pid == pmtPID {
			videoPID = parsePMT(packet.payload)
			break
		}
	}

	es := collectElementaryStream(buf, start, videoPID)
	if len(es) == 0 {
		return MediaResolution{}, errBitstream
	}
	sps := findH264SPS(es)
	if sps == nil {
		return MediaResolution{}, errBitstream
	}
	return parseH264SPS(sps)
}

// ---------------------------------------------------------------- MP4 / fMP4

// 承载分辨率的 visual sample entry。
var visualBoxes = map[string]bool{
	"avc1": true, "avc3": true, "hvc1": true, "hev1": true,
	"mp4v": true, "encv": true, "dvh1": true, "dvhe": true,
}

// 需要继续下钻的容器 box，值为其头部长度（stsd 是 full box，多了 4 字节）。
var containerBoxes = map[string]int{
	"moov": 8,
	"trak": 8,
	"mdia": 8,
	"minf": 8,
	"stbl": 8,
	"stsd": 16,
}

func readU32(buf []byte, offset int) (uint32, bool) {
	if offset < 0 || offset+4 > len(buf) {
		return 0, false
	}
	return uint32(buf[offset])<<24 | uint32(buf[offset+1])<<16 | uint32(buf[offset+2])<<8 | uint32(buf[offset+3]), true
}

func boxType(buf []byte, offset int) (string, bool) {
	if offset < 0 || offset+4 > len(buf) {
		return "", false
	}
	return string(buf[offset : offset+4]), true
}

// readVisualSampleEntry 读 VisualSampleEntry 的宽高：
// SampleEntry(6 保留 + 2 data_reference_index) + 2 pre_defined + 2 reserved + 12 pre_defined
// = 载荷偏移 24，宽高各 16 位定点整数。
func readVisualSampleEntry(buf []byte, boxStart int) (MediaResolution, bool) {
	offset := boxStart + 8 + 24
	if offset+4 > len(buf) {
		return MediaResolution{}, false
	}
	width := int(buf[offset])<<8 | int(buf[offset+1])
	height := int(buf[offset+2])<<8 | int(buf[offset+3])
	if width <= 0 || height <= 0 {
		return MediaResolution{}, false
	}
	return MediaResolution{Width: width, Height: height}, true
}

func isPrintableBoxType(typ string) bool {
	for i := 0; i < len(typ); i++ {
		if typ[i] < 0x20 || typ[i] > 0x7e {
			return false
		}
	}
	return true
}

func scanBoxes(buf []byte, from, to, depth int) (MediaResolution, bool) {
	if depth > maxBoxDepth {
		return MediaResolution{}, false
	}
	for p := from; p+8 <= to; {
		typ, ok := boxType(buf, p+4)
		if !ok || !isPrintableBoxType(typ) {
			return MediaResolution{}, false
		}
		size, ok := readU32(buf, p)
		if !ok {
			return MediaResolution{}, false
		}
		header := 8
		if size == 1 {
			if p+16 > to {
				return MediaResolution{}, false
			}
			high, ok := readU32(buf, p+8)
			if !ok {
				return MediaResolution{}, false
			}
			low, ok := readU32(buf, p+12)
			if !ok || high != 0 { // 超过 4GB，分片前缀里不可能
				return MediaResolution{}, false
			}
			size = low
			header = 16
		}
		if size == 0 {
			size = uint32(to - p)
		}
		if int(size) < header || p+int(size) > to {
			return MediaResolution{}, false
		}
		if visualBoxes[typ] {
			if found, ok := readVisualSampleEntry(buf, p); ok {
				return found, true
			}
		}
		if childHeader, isContainer := containerBoxes[typ]; isContainer {
			if found, ok := scanBoxes(buf, p+childHeader, p+int(size), depth+1); ok {
				return found, true
			}
		}
		p += int(size)
	}
	return MediaResolution{}, false
}

// ParseMP4VideoResolution 解析 MP4 / fMP4（init segment）得到分辨率。
func ParseMP4VideoResolution(buf []byte) (MediaResolution, bool) {
	return scanBoxes(buf, 0, len(buf), 0)
}

// LooksLikeMP4 判断分片是否以 MP4 box 开头：拿不到扩展名时用它区分 fMP4 与 TS。
func LooksLikeMP4(buf []byte) bool {
	if len(buf) < 12 {
		return false
	}
	typ, ok := boxType(buf, 4)
	if !ok {
		return false
	}
	switch typ {
	case "ftyp", "styp", "moov", "moof":
		return true
	}
	return false
}

// ---------------------------------------------------------------- 解密

// DecryptAES128CBCPrefix 解密 AES-128-CBC 的分片前缀。
// 只处理完整的 16 字节块：只读了分片开头一小段，末尾必然是不完整块，
// 不做 padding 校验（HLS 的 AES-128 是 CBC 无填充语义）。
func DecryptAES128CBCPrefix(buf, key, iv []byte) []byte {
	usable := len(buf) / aes.BlockSize * aes.BlockSize
	if usable == 0 || len(key) < aes.BlockSize || len(iv) < aes.BlockSize {
		return nil
	}
	block, err := aes.NewCipher(key[:aes.BlockSize])
	if err != nil {
		return nil
	}
	out := make([]byte, usable)
	// 显式复制 IV：CBC 会修改传入的 IV，调用方的切片不应被写坏。
	ivCopy := append([]byte(nil), iv[:aes.BlockSize]...)
	cipher.NewCBCDecrypter(block, ivCopy).CryptBlocks(out, buf[:usable])
	return out
}
