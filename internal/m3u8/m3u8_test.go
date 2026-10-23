package m3u8

import (
	"net/url"
	"strings"
	"testing"
)

func TestIsPlaylist(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		contentType string
		rawURL      string
		want        bool
	}{
		{"vnd.apple.mpegurl", "application/vnd.apple.mpegurl", "https://cdn.example.com/a", true},
		{"带参数且大写", "Application/VND.Apple.MPEGURL; charset=utf-8", "", true},
		{"audio/mpegurl", "audio/mpegurl", "", true},
		{"application/m3u8", "application/m3u8", "", true},
		{"ct 缺失但 URL 后缀", "", "https://cdn.example.com/live/index.M3U8?token=1", true},
		{"ct 不匹配但 URL 后缀", "application/octet-stream", "https://cdn.example.com/live/index.m3u8#frag", true},
		{"普通分片", "video/mp2t", "https://cdn.example.com/live/seg1.ts", false},
		{"query 里出现 m3u8 不算", "application/json", "https://cdn.example.com/api?m3u8=1", false},
		{"无后缀的 manifest", "text/html", "https://cdn.example.com/live", false},
		{"空输入", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsPlaylist(tc.contentType, tc.rawURL); got != tc.want {
				t.Fatalf("IsPlaylist(%q, %q) = %v, want %v", tc.contentType, tc.rawURL, got, tc.want)
			}
		})
	}
}

// proxyOf 按合同拼出代理地址，避免测试里重复 QueryEscape。
func proxyOf(proxyURLBase, abs string) string {
	return proxyURLBase + url.QueryEscape(abs)
}

func TestRewriteMasterPlaylist(t *testing.T) {
	t.Parallel()

	const (
		base      = "https://cdn.example.com/live/index.m3u8"
		proxyBase = "/api/proxy?url="
	)
	input := strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		`#EXT-X-STREAM-INF:BANDWIDTH=1280000,CODECS="avc1.4d401f,mp4a.40.2"`,
		"720p/index.m3u8",
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",URI="audio/en.m3u8",DEFAULT=YES`,
		`#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=86000,URI="/iframe/720p.m3u8"`,
		`#EXT-X-SESSION-KEY:METHOD=AES-128,URI="https://keys.example.com/k.bin",IV=0x1`,
		`#EXT-X-CONTENT-STEERING:SERVER-URI="https://cdn.example.com/steering.json"`,
		`#EXT-X-KEY:METHOD=AES-128,URI="key.bin"`,
		`#EXT-X-MAP:URI="init.mp4"`,
		"# a plain comment",
		"seg1.ts",
		"https://other.example.com/seg2.ts?token=a%2Fb",
		"/data/seg3.ts",
		"#EXT-X-ENDLIST",
	}, "\n")

	out := Rewrite(input, base, proxyBase)
	lines := strings.Split(out, "\n")
	if len(lines) != 15 {
		t.Fatalf("行数变化：got %d, want 15\n%s", len(lines), out)
	}

	wantByIndex := map[int]string{
		3:  proxyOf(proxyBase, "https://cdn.example.com/live/720p/index.m3u8"),
		4:  `#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",URI="` + proxyOf(proxyBase, "https://cdn.example.com/live/audio/en.m3u8") + `",DEFAULT=YES`,
		5:  `#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=86000,URI="` + proxyOf(proxyBase, "https://cdn.example.com/iframe/720p.m3u8") + `"`,
		6:  `#EXT-X-SESSION-KEY:METHOD=AES-128,URI="` + proxyOf(proxyBase, "https://keys.example.com/k.bin") + `",IV=0x1`,
		8:  `#EXT-X-KEY:METHOD=AES-128,URI="` + proxyOf(proxyBase, "https://cdn.example.com/live/key.bin") + `"`,
		9:  `#EXT-X-MAP:URI="` + proxyOf(proxyBase, "https://cdn.example.com/live/init.mp4") + `"`,
		11: proxyOf(proxyBase, "https://cdn.example.com/live/seg1.ts"),
		12: proxyOf(proxyBase, "https://other.example.com/seg2.ts?token=a%2Fb"),
		13: proxyOf(proxyBase, "https://cdn.example.com/data/seg3.ts"),
	}
	for i, want := range wantByIndex {
		if lines[i] != want {
			t.Errorf("第 %d 行：\n got %q\nwant %q", i, lines[i], want)
		}
	}

	// 标签、注释、顺序与语义无关行原样保留。
	unchanged := map[int]string{
		0:  "#EXTM3U",
		1:  "#EXT-X-VERSION:3",
		2:  `#EXT-X-STREAM-INF:BANDWIDTH=1280000,CODECS="avc1.4d401f,mp4a.40.2"`,
		7:  `#EXT-X-CONTENT-STEERING:SERVER-URI="https://cdn.example.com/steering.json"`,
		10: "# a plain comment",
		14: "#EXT-X-ENDLIST",
	}
	for i, want := range unchanged {
		if lines[i] != want {
			t.Errorf("第 %d 行应保持不变：\n got %q\nwant %q", i, lines[i], want)
		}
	}
}

func TestRewritePlaylistRelativeToFinalURL(t *testing.T) {
	t.Parallel()

	// gslb 302 之后目录变化：相对分片必须基于最终 URL，而不是请求前的地址。
	out := Rewrite("seg.ts", "https://edge2.example.com/redirected/path/index.m3u8", "/api/proxy?url=")
	want := proxyOf("/api/proxy?url=", "https://edge2.example.com/redirected/path/seg.ts")
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestRewriteKeepsCRLF(t *testing.T) {
	t.Parallel()

	out := Rewrite("#EXTM3U\r\nseg.ts\r\n\r\n#EXT-X-ENDLIST\r\n", "https://cdn.example.com/a/index.m3u8", "/api/proxy?url=")
	want := "#EXTM3U\r\n" +
		proxyOf("/api/proxy?url=", "https://cdn.example.com/a/seg.ts") + "\r\n" +
		"\r\n#EXT-X-ENDLIST\r\n"
	if out != want {
		t.Fatalf("CRLF 未保留：\n got %q\nwant %q", out, want)
	}
}

func TestRewritePreservesTrailingNewlineAndEmptyInput(t *testing.T) {
	t.Parallel()

	if got := Rewrite("", "https://cdn.example.com/a.m3u8", "/api/proxy?url="); got != "" {
		t.Fatalf("空文本应返回空串，got %q", got)
	}
	if got := Rewrite("\n\n", "https://cdn.example.com/a.m3u8", "/api/proxy?url="); got != "\n\n" {
		t.Fatalf("空行应原样保留，got %q", got)
	}
	// 文件末尾无换行时也不额外补。
	if got := Rewrite("seg.ts", "https://cdn.example.com/a/index.m3u8", "/api/proxy?url="); strings.HasSuffix(got, "\n") {
		t.Fatalf("不应补行尾换行，got %q", got)
	}
}

func TestRewriteSkipsAlreadyProxiedURIs(t *testing.T) {
	t.Parallel()

	encoded := url.QueryEscape("https://x.com/a.ts")
	lines := []string{
		"#EXTM3U",
		"/api/proxy?url=" + encoded,
		"/api/proxy/" + encoded,
		"/api/live/stream?url=" + encoded,
		"/api/live/stream/" + encoded,
		`#EXT-X-KEY:METHOD=AES-128,URI="/api/proxy?url=` + encoded + `"`,
		`#EXT-X-MAP:URI="/api/live/stream/` + encoded + `"`,
		"#EXT-X-ENDLIST",
	}
	input := strings.Join(lines, "\n")
	if got := Rewrite(input, "https://cdn.example.com/index.m3u8", "/api/proxy?url="); got != input {
		t.Fatalf("已是代理地址不应二次包裹：\n got %q\nwant %q", got, input)
	}

	// 前缀相近但不构成路由边界的本地路径仍应被正常改写（防误判）。
	out := Rewrite("/api/proxyfoo/seg.ts", "https://cdn.example.com/index.m3u8", "/api/proxy?url=")
	want := proxyOf("/api/proxy?url=", "https://cdn.example.com/api/proxyfoo/seg.ts")
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestRewriteKeepsBadURLsAndNonHTTPSchemes(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"http://[::1",
		"%%%",
		"http://",
		"data:application/octet-stream;base64,QUJD",
		"skd://key-id",
		"blob:https://cdn.example.com/abc",
	}
	for _, in := range inputs {
		if got := Rewrite(in, "https://cdn.example.com/index.m3u8", "/api/proxy?url="); got != in {
			t.Errorf("坏/非 http 地址应原样保留：Rewrite(%q) = %q", in, got)
		}
		// 标签 URI 里的坏地址同理。
		line := `#EXT-X-KEY:METHOD=AES-128,URI="` + in + `"`
		if got := Rewrite(line, "https://cdn.example.com/index.m3u8", "/api/proxy?url="); got != line {
			t.Errorf("标签里的坏地址应原样保留：Rewrite(%q) = %q", line, got)
		}
	}

	// 空 URI 属性不动。
	empty := `#EXT-X-MAP:URI=""`
	if got := Rewrite(empty, "https://cdn.example.com/index.m3u8", "/api/proxy?url="); got != empty {
		t.Errorf("空 URI 应原样保留，got %q", got)
	}
}

func TestRewriteLivePrefix(t *testing.T) {
	t.Parallel()

	out := Rewrite("seg1.ts", "https://cdn.example.com/live/index.m3u8", "/api/live/stream?url=")
	want := proxyOf("/api/live/stream?url=", "https://cdn.example.com/live/seg1.ts")
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestRewriteProtocolRelativeAndBOM(t *testing.T) {
	t.Parallel()

	out := Rewrite("//cdn2.example.com/x.ts", "https://cdn.example.com/live/index.m3u8", "/api/proxy?url=")
	want := proxyOf("/api/proxy?url=", "https://cdn2.example.com/x.ts")
	if out != want {
		t.Fatalf("协议相对地址：got %q, want %q", out, want)
	}

	withBOM := "\uFEFF#EXTM3U\nseg.ts\n"
	out = Rewrite(withBOM, "https://cdn.example.com/live/index.m3u8", "/api/proxy?url=")
	wantBOM := "\uFEFF#EXTM3U\n" + proxyOf("/api/proxy?url=", "https://cdn.example.com/live/seg.ts") + "\n"
	if out != wantBOM {
		t.Fatalf("BOM 处理错误：\n got %q\nwant %q", out, wantBOM)
	}
}
