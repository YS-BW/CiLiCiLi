package quality

import (
	"bytes"
	"testing"
)

func TestParseBestHeight(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
		ok   bool
	}{
		{
			name: "多码率取最高档",
			text: "#EXTM3U\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\nlow.m3u8\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080\nhigh.m3u8\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720\nmid.m3u8\n",
			want: 1080,
			ok:   true,
		},
		{
			name: "4K",
			text: "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=20000000,RESOLUTION=3840x2160\nv.m3u8\n",
			want: 2160,
			ok:   true,
		},
		{
			name: "横竖写反取短边",
			text: "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2800000,RESOLUTION=720x1280\nv.m3u8\n",
			want: 720,
			ok:   true,
		},
		{
			name: "仅 BANDWIDTH 按码率推断",
			text: "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=5000000\nv.m3u8\n",
			want: 1080,
			ok:   true,
		},
		{
			name: "仅 BANDWIDTH 低码率",
			text: "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=900000\nv.m3u8\n",
			want: 480,
			ok:   true,
		},
		{
			name: "媒体列表无档位",
			text: "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\nseg0.ts\n",
			want: 0,
			ok:   false,
		},
		{
			name: "只有标签没有变体地址",
			text: "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=5000000\n",
			want: 0,
			ok:   false,
		},
		{
			name: "码率与分辨率都缺失",
			text: "#EXTM3U\n#EXT-X-STREAM-INF:CODECS=\"avc1.4d401f\"\nv.m3u8\n",
			want: 0,
			ok:   false,
		},
		{
			name: "注释行不消耗 pending",
			text: "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=3000000\n# 注释\nv.m3u8\n",
			want: 720,
			ok:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			height, ok := ParseBestHeight(tc.text)
			if ok != tc.ok || height != tc.want {
				t.Fatalf("ParseBestHeight = (%d, %v), want (%d, %v)", height, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestParseMediaPlaylist(t *testing.T) {
	text := "#EXTM3U\r\n" +
		"#EXT-X-VERSION:3\r\n" +
		"#EXT-X-MEDIA-SEQUENCE:7\r\n" +
		"#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\",IV=0x000102030405060708090a0b0c0d0e0f\r\n" +
		"#EXT-X-MAP:URI=\"init.mp4\"\r\n" +
		"#EXTINF:6.0,\r\n" +
		"seg0.ts\r\n" +
		"#EXTINF:6.0,\r\n" +
		"seg1.ts\r\n"
	info := ParseMediaPlaylist(text)

	if info.InitURI != "init.mp4" {
		t.Fatalf("InitURI = %q, want init.mp4", info.InitURI)
	}
	if info.FirstSegment != "seg0.ts" {
		t.Fatalf("FirstSegment = %q, want seg0.ts", info.FirstSegment)
	}
	if info.KeyMethod != "AES-128" {
		t.Fatalf("KeyMethod = %q, want AES-128", info.KeyMethod)
	}
	if info.KeyURI != "key.bin" {
		t.Fatalf("KeyURI = %q, want key.bin", info.KeyURI)
	}
	wantIV := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	if !bytes.Equal(info.KeyIV, wantIV) {
		t.Fatalf("KeyIV = %x, want %x", info.KeyIV, wantIV)
	}
	if info.MediaSequence != 7 {
		t.Fatalf("MediaSequence = %d, want 7", info.MediaSequence)
	}
}

func TestParseMediaPlaylistDefaults(t *testing.T) {
	info := ParseMediaPlaylist("#EXTM3U\n#EXTINF:4,\nhttps://cdn.example.com/a/seg.ts\n")
	if info.InitURI != "" || info.KeyMethod != "" || info.KeyURI != "" || info.KeyIV != nil {
		t.Fatalf("无 init/key 时不应有值: %+v", info)
	}
	if info.FirstSegment != "https://cdn.example.com/a/seg.ts" {
		t.Fatalf("FirstSegment = %q", info.FirstSegment)
	}
	if info.MediaSequence != 0 {
		t.Fatalf("MediaSequence = %d, want 0", info.MediaSequence)
	}
}

func TestParseMediaPlaylistOddIVIgnored(t *testing.T) {
	info := ParseMediaPlaylist("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\",IV=0xabc\nseg.ts\n")
	if info.KeyIV != nil {
		t.Fatalf("非法 IV 应为 nil, got %x", info.KeyIV)
	}
}

func TestInferHeightFromBitrate(t *testing.T) {
	tests := []struct {
		bps  int
		want int
	}{
		{0, 0},
		{-1, 0},
		{399999, 240},
		{400000, 360},
		{799999, 360},
		{800000, 480},
		{1500000, 540},
		{3000000, 720},
		{5000000, 1080},
		{10000000, 2160},
		{20000000, 4320},
	}
	for _, tc := range tests {
		if got := InferHeightFromBitrate(tc.bps); got != tc.want {
			t.Fatalf("InferHeightFromBitrate(%d) = %d, want %d", tc.bps, got, tc.want)
		}
	}
}

func TestProbeEpisodeOrder(t *testing.T) {
	tests := []struct {
		name     string
		episodes []string
		want     []string
	}{
		{name: "空", episodes: nil, want: nil},
		{name: "单集", episodes: []string{"a"}, want: []string{"a"}},
		{name: "两集只探中间（等于最后一集）", episodes: []string{"a", "b"}, want: []string{"b"}},
		{name: "三集中间+最后一集", episodes: []string{"a", "b", "c"}, want: []string{"b", "c"}},
		{name: "四集", episodes: []string{"a", "b", "c", "d"}, want: []string{"c", "d"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ProbeEpisodeOrder(tc.episodes)
			if len(got) != len(tc.want) {
				t.Fatalf("ProbeEpisodeOrder = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ProbeEpisodeOrder = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
