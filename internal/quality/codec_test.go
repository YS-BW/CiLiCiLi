package quality

import (
	"bytes"
	"testing"
)

func TestParseH264SPS(t *testing.T) {
	tests := []struct {
		width  int
		height int
	}{
		{width: 1920, height: 1080},
		{width: 1280, height: 720},
		{width: 3840, height: 2160},
		{width: 1920, height: 1038}, // 带裁剪
		{width: 640, height: 360},
	}
	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			res, ok := ParseH264SPS(buildSPSRBSP(tc.width, tc.height))
			if !ok {
				t.Fatalf("ParseH264SPS(%dx%d) 失败", tc.width, tc.height)
			}
			if res.Width != tc.width || res.Height != tc.height {
				t.Fatalf("ParseH264SPS = %dx%d, want %dx%d", res.Width, res.Height, tc.width, tc.height)
			}
		})
	}
}

func TestParseH264SPSHighProfile(t *testing.T) {
	tests := []struct {
		name    string
		scaling bool
		pocType int
	}{
		{name: "High 无 scaling list / poc0", scaling: false, pocType: 0},
		{name: "High 带 scaling list", scaling: true, pocType: 0},
		{name: "High poc type 1", scaling: false, pocType: 1},
		{name: "High poc type 2", scaling: false, pocType: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := ParseH264SPS(buildHighProfileSPSRBSP(1920, 1080, tc.scaling, tc.pocType))
			if !ok {
				t.Fatalf("High profile SPS 解析失败")
			}
			if res.Width != 1920 || res.Height != 1080 {
				t.Fatalf("High profile SPS = %dx%d, want 1920x1080", res.Width, res.Height)
			}
		})
	}
}

func TestChromaSubsampling(t *testing.T) {
	tests := []struct {
		format int
		subW   int
		subH   int
	}{
		{format: 0, subW: 1, subH: 1},
		{format: 1, subW: 2, subH: 2},
		{format: 2, subW: 2, subH: 1},
		{format: 3, subW: 1, subH: 1},
	}
	for _, tc := range tests {
		subW, subH := chromaSubsampling(tc.format)
		if subW != tc.subW || subH != tc.subH {
			t.Fatalf("chromaSubsampling(%d) = (%d,%d), want (%d,%d)",
				tc.format, subW, subH, tc.subW, tc.subH)
		}
	}
}

func TestParseH264SPSInvalid(t *testing.T) {
	if _, ok := ParseH264SPS(nil); ok {
		t.Fatal("空 SPS 不应解析成功")
	}
	if _, ok := ParseH264SPS([]byte{0xff, 0xff, 0xff, 0xff}); ok {
		t.Fatal("垃圾数据不应解析成功")
	}
	// 截断：只保留 SPS 前几个字节。
	sps := buildSPSRBSP(1920, 1080)
	if _, ok := ParseH264SPS(sps[:4]); ok {
		t.Fatal("截断的 SPS 不应解析成功")
	}
}

func TestUnescapeRBSP(t *testing.T) {
	got := UnescapeRBSP([]byte{0x00, 0x00, 0x03, 0x01, 0x00, 0x00, 0x03, 0x00})
	want := []byte{0x00, 0x00, 0x01, 0x00, 0x00, 0x00}
	if !bytes.Equal(got, want) {
		t.Fatalf("UnescapeRBSP = %x, want %x", got, want)
	}
}

func TestParseMP4VideoResolution(t *testing.T) {
	init := fmp4Init(1920, 1080)
	res, ok := ParseMP4VideoResolution(init)
	if !ok {
		t.Fatal("fMP4 init 应解析出分辨率")
	}
	if res.Width != 1920 || res.Height != 1080 {
		t.Fatalf("ParseMP4VideoResolution = %dx%d, want 1920x1080", res.Width, res.Height)
	}

	if _, ok := ParseMP4VideoResolution(nil); ok {
		t.Fatal("空数据不应解析成功")
	}
	if _, ok := ParseMP4VideoResolution([]byte("not a mp4 file")); ok {
		t.Fatal("非 MP4 数据不应解析成功")
	}
	if _, ok := ParseMP4VideoResolution(init[:12]); ok {
		t.Fatal("截断的 MP4 不应解析成功")
	}
}

func TestLooksLikeMP4(t *testing.T) {
	for _, typ := range []string{"ftyp", "styp", "moov", "moof"} {
		buf := append([]byte{0, 0, 0, 16}, []byte(typ)...)
		buf = append(buf, 0, 0, 0, 0)
		if !LooksLikeMP4(buf) {
			t.Fatalf("LooksLikeMP4(%q) = false, want true", typ)
		}
	}
	if LooksLikeMP4(buildTSStream(1280, 720)) {
		t.Fatal("TS 分片不应识别为 MP4")
	}
	if LooksLikeMP4([]byte{0x47}) {
		t.Fatal("过短数据不应识别为 MP4")
	}
}

func TestParseTSVideoResolution(t *testing.T) {
	res, ok := ParseTSVideoResolution(buildTSStream(1280, 720))
	if !ok {
		t.Fatal("TS 分片应解析出分辨率")
	}
	if res.Width != 1280 || res.Height != 720 {
		t.Fatalf("ParseTSVideoResolution = %dx%d, want 1280x720", res.Width, res.Height)
	}

	if _, ok := ParseTSVideoResolution([]byte("garbage")); ok {
		t.Fatal("垃圾数据不应解析成功")
	}
}

func TestParseTSVideoResolutionWithLeadingPadding(t *testing.T) {
	padded := append([]byte{0xde, 0xad, 0xbe}, buildTSStream(1920, 1080)...)
	res, ok := ParseTSVideoResolution(padded)
	if !ok || res.Height != 1080 {
		t.Fatalf("带前导 padding 的 TS 解析 = (%+v, %v), want height 1080", res, ok)
	}
}

func TestDecryptAES128CBCPrefix(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 16)
	iv := defaultIV(9)
	plain := buildTSStream(1920, 1080)
	ciphertext := encryptAES128CBC(plain, key, iv)
	if len(ciphertext) != len(plain)/16*16 {
		t.Fatalf("密文长度 = %d, want %d", len(ciphertext), len(plain)/16*16)
	}

	got := DecryptAES128CBCPrefix(ciphertext, key, iv)
	if !bytes.Equal(got, plain[:len(ciphertext)]) {
		t.Fatal("AES-128-CBC 前缀解密结果与原文不一致")
	}
	res, ok := ParseTSVideoResolution(got)
	if !ok || res.Height != 1080 {
		t.Fatalf("解密后的 TS 解析 = (%+v, %v), want height 1080", res, ok)
	}

	if got := DecryptAES128CBCPrefix(ciphertext[:15], key, iv); got != nil {
		t.Fatal("不足一个块时应返回 nil")
	}
	if got := DecryptAES128CBCPrefix(ciphertext, key[:8], iv); got != nil {
		t.Fatal("key 不足 16 字节时应返回 nil")
	}
	if got := DecryptAES128CBCPrefix(ciphertext, key, iv[:8]); got != nil {
		t.Fatal("IV 不足 16 字节时应返回 nil")
	}
}
