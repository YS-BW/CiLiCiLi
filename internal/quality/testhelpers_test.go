package quality

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"cilicili/internal/cache"
	"cilicili/internal/model"
	"cilicili/internal/upstream"
)

// ---------------------------------------------------------------- fake Doer

type fakeCall struct {
	url  string
	opts upstream.Options
}

type fakeDoer struct {
	mu    sync.Mutex
	calls []fakeCall
	fn    func(context.Context, string) (*upstream.Response, error)
}

func (f *fakeDoer) Do(ctx context.Context, rawURL string, opts upstream.Options) (*upstream.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{url: rawURL, opts: opts})
	f.mu.Unlock()
	if f.fn == nil {
		return nil, errors.New("fake: 没有配置响应")
	}
	return f.fn(ctx, rawURL)
}

func (f *fakeDoer) GetJSON(context.Context, string, upstream.Options) ([]byte, error) {
	return nil, errors.New("fake: GetJSON 不应被调用")
}

func (f *fakeDoer) GetText(context.Context, string, upstream.Options) (string, error) {
	return "", errors.New("fake: GetText 不应被调用")
}

func (f *fakeDoer) snapshot() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

func (f *fakeDoer) count() int { return len(f.snapshot()) }

func (f *fakeDoer) countContaining(sub string) int {
	n := 0
	for _, call := range f.snapshot() {
		if strings.Contains(call.url, sub) {
			n++
		}
	}
	return n
}

func (f *fakeDoer) urls() []string {
	calls := f.snapshot()
	out := make([]string, len(calls))
	for i, call := range calls {
		out[i] = call.url
	}
	return out
}

// ---------------------------------------------------------------- fake cache

type fakeStore struct {
	mu       sync.Mutex
	values   map[string][]byte
	ttls     map[string]time.Duration
	staleTTL map[string]time.Duration
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		values:   make(map[string][]byte),
		ttls:     make(map[string]time.Duration),
		staleTTL: make(map[string]time.Duration),
	}
}

func (s *fakeStore) Get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[key]
	return value, ok
}

func (s *fakeStore) GetStale(key string) ([]byte, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[key]
	return value, true, ok
}

func (s *fakeStore) Set(key string, value []byte, ttl, staleTTL time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = append([]byte(nil), value...)
	s.ttls[key] = ttl
	s.staleTTL[key] = staleTTL
}

func (s *fakeStore) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
}

func (s *fakeStore) Stats() cache.Stats { return cache.Stats{} }

func (s *fakeStore) Close() error { return nil }

func (s *fakeStore) recordedTTL() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ttl := range s.ttls {
		return ttl, true
	}
	return 0, false
}

func (s *fakeStore) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.values)
}

// ---------------------------------------------------------------- 响应构造

func byteResponse(rawURL string, status int, body []byte) *upstream.Response {
	return &upstream.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(body)),
		FinalURL:   rawURL,
	}
}

func textResponse(rawURL string, status int, body string) *upstream.Response {
	return byteResponse(rawURL, status, []byte(body))
}

// responseWithFinal 构造带重定向最终 URL 的响应（HLS 相对地址以它为基准解析）。
func responseWithFinal(rawURL, finalURL string, status int, body []byte) *upstream.Response {
	resp := byteResponse(rawURL, status, body)
	resp.FinalURL = finalURL
	return resp
}

func detailJSON(t *testing.T, id string, episodes []string) []byte {
	t.Helper()
	segments := make([]string, len(episodes))
	for i, episode := range episodes {
		segments[i] = fmt.Sprintf("第%d集$%s", i+1, episode)
	}
	payload := map[string]any{
		"code": 1,
		"list": []map[string]any{{
			"vod_id":       id,
			"vod_name":     "测试影片",
			"vod_play_url": strings.Join(segments, "#"),
		}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("构造详情 JSON: %v", err)
	}
	return data
}

func emptyDetailJSON() []byte {
	return []byte(`{"code":1,"list":[]}`)
}

func probeItem(sourceURL, detail, id string) model.QualityProbeItem {
	return model.QualityProbeItem{
		Source: model.SourceConfig{Key: "test", Name: "测试源", URL: sourceURL, Detail: detail},
		ID:     id,
	}
}

// ---------------------------------------------------------------- MP4 构造

func mp4Box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

// visualSampleEntryPayload 构造 28 字节的 VisualSampleEntry 载荷（宽高在偏移 24/26）。
func visualSampleEntryPayload(width, height uint16) []byte {
	payload := make([]byte, 28)
	binary.BigEndian.PutUint16(payload[24:26], width)
	binary.BigEndian.PutUint16(payload[26:28], height)
	return payload
}

// fmp4Init 构造带 moov/trak/mdia/minf/stbl/stsd/avc1 的 fMP4 init segment。
func fmp4Init(width, height uint16) []byte {
	ftyp := mp4Box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2"))
	avc1 := mp4Box("avc1", visualSampleEntryPayload(width, height))
	stsdPayload := make([]byte, 8, 8+len(avc1))
	binary.BigEndian.PutUint32(stsdPayload[4:8], 1) // entry_count
	stsdPayload = append(stsdPayload, avc1...)
	stsd := mp4Box("stsd", stsdPayload)
	stbl := mp4Box("stbl", stsd)
	minf := mp4Box("minf", stbl)
	mdia := mp4Box("mdia", minf)
	trak := mp4Box("trak", mdia)
	moov := mp4Box("moov", trak)
	return append(ftyp, moov...)
}

// ---------------------------------------------------------------- TS / SPS 构造

type bitWriter struct {
	data  []byte
	cur   byte
	nbits uint
}

func (w *bitWriter) write(value uint64, bits int) {
	for i := bits - 1; i >= 0; i-- {
		w.cur = w.cur<<1 | byte(value>>uint(i))&1
		w.nbits++
		if w.nbits == 8 {
			w.data = append(w.data, w.cur)
			w.cur = 0
			w.nbits = 0
		}
	}
}

func (w *bitWriter) writeUE(value uint64) {
	encoded := value + 1
	width := 0
	for x := encoded; x > 0; x >>= 1 {
		width++
	}
	w.write(0, width-1)
	w.write(encoded, width)
}

func (w *bitWriter) finish() []byte {
	if w.nbits > 0 {
		w.cur <<= 8 - w.nbits
		w.data = append(w.data, w.cur)
		w.cur, w.nbits = 0, 0
	}
	return w.data
}

func (w *bitWriter) writeSE(value int64) {
	if value <= 0 {
		w.writeUE(uint64(-2 * value))
		return
	}
	w.writeUE(uint64(2*value - 1))
}

// writeSPSTail 写 SPS 从 log2_max_frame_num 到 frame_cropping 的公共尾部。
func (w *bitWriter) writeSPSTail(width, height, pocType int) {
	w.writeUE(0) // log2_max_frame_num_minus4
	w.writeUE(uint64(pocType))
	switch pocType {
	case 0:
		w.writeUE(0) // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		w.write(0, 1) // delta_pic_order_always_zero_flag
		w.writeSE(0)  // offset_for_non_ref_pic
		w.writeSE(0)  // offset_for_top_to_bottom_field
		w.writeUE(0)  // num_ref_frames_in_pic_order_cnt_cycle
	}
	w.writeUE(1)  // max_num_ref_frames
	w.write(0, 1) // gaps_in_frame_num_value_allowed_flag

	widthMbs := (width + 15) / 16
	heightMbs := (height + 15) / 16
	cropBottom := (heightMbs*16 - height) / 2
	w.writeUE(uint64(widthMbs - 1))
	w.writeUE(uint64(heightMbs - 1))
	w.write(1, 1) // frame_mbs_only_flag
	w.write(1, 1) // direct_8x8_inference_flag
	if cropBottom > 0 {
		w.write(1, 1) // frame_cropping_flag
		w.writeUE(0)
		w.writeUE(0)
		w.writeUE(0)
		w.writeUE(uint64(cropBottom))
	} else {
		w.write(0, 1)
	}
	w.write(1, 1) // rbsp_stop_one_bit
}

// buildSPSRBSP 构造 Baseline profile 的 SPS 位流（任意尺寸，用 frame_cropping 收尾）。
func buildSPSRBSP(width, height int) []byte {
	w := &bitWriter{}
	w.write(66, 8) // profile_idc Baseline
	w.write(0, 8)  // constraint_set flags
	w.write(30, 8) // level_idc
	w.writeUE(0)   // seq_parameter_set_id
	w.writeSPSTail(width, height, 0)
	return w.finish()
}

// buildHighProfileSPSRBSP 构造 High profile 的 SPS 位流；scaling=true 时带 scaling list，
// pocType 覆盖 0/1/2 三种 pic_order_cnt_type 分支。
func buildHighProfileSPSRBSP(width, height int, scaling bool, pocType int) []byte {
	w := &bitWriter{}
	w.write(100, 8) // profile_idc High
	w.write(0, 8)   // constraint_set flags
	w.write(40, 8)  // level_idc
	w.writeUE(0)    // seq_parameter_set_id
	w.writeUE(1)    // chroma_format_idc 4:2:0
	w.writeUE(0)    // bit_depth_luma_minus8
	w.writeUE(0)    // bit_depth_chroma_minus8
	w.write(0, 1)   // qpprime_y_zero_transform_bypass_flag
	if scaling {
		w.write(1, 1) // seq_scaling_matrix_present_flag
		for i := 0; i < 8; i++ {
			w.write(1, 1) // seq_scaling_list_present_flag
			size := 16
			if i >= 6 {
				size = 64 // 后两个 list 属于 4:4:4，长度 64
			}
			for j := 0; j < size; j++ {
				w.writeSE(0)
			}
		}
	} else {
		w.write(0, 1)
	}
	w.writeSPSTail(width, height, pocType)
	return w.finish()
}

// escapeRBSP 插入防竞争字节（00 00 0x → 00 00 03 0x）。
func escapeRBSP(src []byte) []byte {
	out := make([]byte, 0, len(src)+4)
	zeros := 0
	for _, b := range src {
		if zeros >= 2 && b <= 3 {
			out = append(out, 3)
			zeros = 0
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

func annexBNAL(nal []byte) []byte {
	return append([]byte{0x00, 0x00, 0x00, 0x01}, nal...)
}

func buildPATSection(pmtPID uint16) []byte {
	return []byte{
		0x00,       // table_id PAT
		0xB0, 0x0D, // section_length = 13
		0x00, 0x01, // transport_stream_id
		0xC1,       // version/current_next
		0x00, 0x00, // section_number / last_section_number
		0x00, 0x01, // program_number = 1
		0xE0 | byte(pmtPID>>8)&0x1F, byte(pmtPID), // PMT PID
		0x00, 0x00, 0x00, 0x00, // CRC32（解析器不校验）
	}
}

func buildPMTSection(videoPID uint16) []byte {
	return []byte{
		0x02,       // table_id PMT
		0xB0, 0x12, // section_length = 18
		0x00, 0x01, // program_number
		0xC1,       // version/current_next
		0x00, 0x00, // section_number / last_section_number
		0xE0 | byte(videoPID>>8)&0x1F, byte(videoPID), // PCR_PID
		0xF0, 0x00, // program_info_length = 0
		0x1B,                                          // stream_type H.264
		0xE0 | byte(videoPID>>8)&0x1F, byte(videoPID), // elementary_PID
		0xF0, 0x00, // ES_info_length = 0
		0x00, 0x00, 0x00, 0x00, // CRC32
	}
}

func buildTSPacket(pid uint16, payloadStart bool, cc *byte, payload []byte) []byte {
	pkt := make([]byte, tsPacketSize)
	pkt[0] = 0x47
	pkt[1] = byte(pid>>8) & 0x1F
	if payloadStart {
		pkt[1] |= 0x40
	}
	pkt[2] = byte(pid)
	pkt[3] = 0x10 | (*cc & 0x0F) // 只有载荷，无 adaptation field
	*cc++
	if len(payload) > 184 {
		payload = payload[:184]
	}
	copy(pkt[4:], payload)
	for i := 4 + len(payload); i < tsPacketSize; i++ {
		pkt[i] = 0xFF
	}
	return pkt
}

// buildTSStream 用给定分辨率的 SPS 生成一个最小 TS 分片（PAT + PMT + 视频 PES）。
// 追加一段 SEI 填充，让分片跨多个 TS 包（4 个包 = 752 字节，刚好是 AES 块大小的整数倍）。
func buildTSStream(width, height int) []byte {
	sps := buildSPSRBSP(width, height)
	nal := append([]byte{0x67}, escapeRBSP(sps)...) // nal_ref_idc=3, nal_unit_type=7
	es := annexBNAL(nal)
	filler := append([]byte{0x06}, bytes.Repeat([]byte{0x55}, 200)...) // SEI，解析器不关心
	es = append(es, annexBNAL(filler)...)
	return buildTSStreamFromES(es)
}

func buildTSStreamFromES(es []byte) []byte {
	const (
		patPID   uint16 = 0x0000
		pmtPID   uint16 = 0x1000
		videoPID uint16 = 0x0100
	)
	var out []byte
	patCC, pmtCC, videoCC := byte(0), byte(0), byte(0)

	pat := append([]byte{0x00}, buildPATSection(pmtPID)...) // pointer_field = 0
	out = append(out, buildTSPacket(patPID, true, &patCC, pat)...)
	pmt := append([]byte{0x00}, buildPMTSection(videoPID)...)
	out = append(out, buildTSPacket(pmtPID, true, &pmtCC, pmt)...)

	const maxPayload = 184
	firstPacket := true
	for offset := 0; offset < len(es); {
		payloadStart := firstPacket
		chunk := es[offset:]
		var payload []byte
		if firstPacket {
			// PES 头：00 00 01 + stream_id + 长度 + '10' flags + flags2 + header_data_length
			header := []byte{0x00, 0x00, 0x01, 0xE0, 0x00, 0x00, 0x80, 0x00, 0x00}
			if len(chunk) > maxPayload-len(header) {
				chunk = chunk[:maxPayload-len(header)]
			}
			payload = append(append([]byte(nil), header...), chunk...)
			firstPacket = false
		} else {
			if len(chunk) > maxPayload {
				chunk = chunk[:maxPayload]
			}
			payload = chunk
		}
		offset += len(chunk)
		out = append(out, buildTSPacket(videoPID, payloadStart, &videoCC, payload)...)
	}
	return out
}

// encryptAES128CBC 按 HLS 语义加密前缀（只处理完整块，无 padding）。
func encryptAES128CBC(plain, key, iv []byte) []byte {
	usable := len(plain) / aes.BlockSize * aes.BlockSize
	if usable == 0 {
		return nil
	}
	block, err := aes.NewCipher(key[:aes.BlockSize])
	if err != nil {
		return nil
	}
	out := make([]byte, usable)
	ivCopy := append([]byte(nil), iv[:aes.BlockSize]...)
	cipher.NewCBCEncrypter(block, ivCopy).CryptBlocks(out, plain[:usable])
	return out
}
