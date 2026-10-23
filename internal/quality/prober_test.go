package quality

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cilicili/internal/model"
	"cilicili/internal/upstream"
)

const (
	testSourceURL  = "https://cms.example.com/api.php/provide/vod"
	testEpisodeURL = "https://cdn.example.com/v/index.m3u8"
)

func simpleDoer(handler func(string) (*upstream.Response, error)) *fakeDoer {
	return &fakeDoer{fn: func(_ context.Context, rawURL string) (*upstream.Response, error) {
		return handler(rawURL)
	}}
}

func masterPlaylist(resolution string) string {
	return "#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\nlow.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=" + resolution + "\nhigh.m3u8\n"
}

func mediaPlaylist(initURI, keyLine, firstSegment string) string {
	text := "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:6\n"
	if keyLine != "" {
		text += keyLine + "\n"
	}
	if initURI != "" {
		text += "#EXT-X-MAP:URI=\"" + initURI + "\"\n"
	}
	text += "#EXTINF:6.0,\n" + firstSegment + "\n#EXT-X-ENDLIST\n"
	return text
}

func TestProbeMasterPlaylist(t *testing.T) {
	detail := detailJSON(t, "42", []string{testEpisodeURL})
	tests := []struct {
		name   string
		master string
		want   int
	}{
		{name: "RESOLUTION", master: masterPlaylist("1920x1080"), want: 1080},
		{name: "横竖写反取短边", master: masterPlaylist("720x1280"), want: 720},
		{name: "仅 BANDWIDTH 推断", master: "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=3000000\nv.m3u8\n", want: 720},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
				switch {
				case strings.Contains(rawURL, "ac=videolist"):
					return byteResponse(rawURL, 200, detail), nil
				case strings.HasSuffix(rawURL, "index.m3u8"):
					return textResponse(rawURL, 200, tc.master), nil
				default:
					return textResponse(rawURL, 404, "not found"), nil
				}
			})
			res := New(Options{Client: client}).Probe(context.Background(), probeItem(testSourceURL, "", "42"))
			if res.Error != "" {
				t.Fatalf("Probe error = %q, want empty", res.Error)
			}
			if res.Height != tc.want {
				t.Fatalf("Probe height = %d, want %d", res.Height, tc.want)
			}
			if res.SourceKey != "test" || res.VodID != "42" {
				t.Fatalf("结果标识 = (%q,%q), want (test,42)", res.SourceKey, res.VodID)
			}
		})
	}
}

func TestProbeMediaPlaylistFMP4Init(t *testing.T) {
	detail := detailJSON(t, "42", []string{testEpisodeURL})
	playlist := mediaPlaylist("init.mp4", "", "seg0.m4s")
	client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist"):
			return byteResponse(rawURL, 200, detail), nil
		case strings.HasSuffix(rawURL, "index.m3u8"):
			return textResponse(rawURL, 200, playlist), nil
		case strings.HasSuffix(rawURL, "init.mp4"):
			return byteResponse(rawURL, 200, fmp4Init(1920, 1080)), nil
		case strings.HasSuffix(rawURL, "seg0.m4s"):
			return byteResponse(rawURL, 200, []byte("not-used-media-bytes")), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	})
	res := New(Options{Client: client}).Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if res.Error != "" || res.Height != 1080 {
		t.Fatalf("Probe = (%d, %q), want (1080, \"\")", res.Height, res.Error)
	}
}

func TestProbeMediaPlaylistTSSPS(t *testing.T) {
	detail := detailJSON(t, "42", []string{testEpisodeURL})
	playlist := mediaPlaylist("", "", "seg0.ts")
	client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist"):
			return byteResponse(rawURL, 200, detail), nil
		case strings.HasSuffix(rawURL, "index.m3u8"):
			return textResponse(rawURL, 200, playlist), nil
		case strings.HasSuffix(rawURL, "seg0.ts"):
			return byteResponse(rawURL, 200, buildTSStream(1280, 720)), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	})
	res := New(Options{Client: client}).Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if res.Error != "" || res.Height != 720 {
		t.Fatalf("Probe = (%d, %q), want (720, \"\")", res.Height, res.Error)
	}
}

func TestProbeAES128(t *testing.T) {
	detail := detailJSON(t, "42", []string{testEpisodeURL})
	key := bytes.Repeat([]byte{0x2a}, 16)
	plain := buildTSStream(1920, 1080)

	explicitIV := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	explicitIVLine := "#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\",IV=0x000102030405060708090a0b0c0d0e0f"

	tests := []struct {
		name     string
		keyLine  string
		iv       []byte
		wantH    int
		wantErrs bool
	}{
		{name: "显式 IV", keyLine: explicitIVLine, iv: explicitIV, wantH: 1080},
		{name: "默认 IV 取媒体序列号", keyLine: "#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"", iv: defaultIV(5), wantH: 1080},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			playlist := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:5\n" +
				tc.keyLine + "\n#EXTINF:6.0,\nseg0.ts\n"
			ciphertext := encryptAES128CBC(plain, key, tc.iv)
			client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
				switch {
				case strings.Contains(rawURL, "ac=videolist"):
					return byteResponse(rawURL, 200, detail), nil
				case strings.HasSuffix(rawURL, "index.m3u8"):
					return textResponse(rawURL, 200, playlist), nil
				case strings.HasSuffix(rawURL, "key.bin"):
					return byteResponse(rawURL, 200, key), nil
				case strings.HasSuffix(rawURL, "seg0.ts"):
					return byteResponse(rawURL, 200, ciphertext), nil
				default:
					return textResponse(rawURL, 404, "not found"), nil
				}
			})
			res := New(Options{Client: client}).Probe(context.Background(), probeItem(testSourceURL, "", "42"))
			if res.Error != "" || res.Height != tc.wantH {
				t.Fatalf("Probe = (%d, %q), want (%d, \"\")", res.Height, res.Error, tc.wantH)
			}
		})
	}
}

func TestProbeUnsupportedEncryptionIsUnknown(t *testing.T) {
	detail := detailJSON(t, "42", []string{testEpisodeURL})
	plain := buildTSStream(1920, 1080)
	tests := []struct{ name, keyLine string }{
		{name: "SAMPLE-AES", keyLine: "#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"key.bin\""},
		{name: "AES-128 但 key 过短", keyLine: "#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			playlist := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n" + tc.keyLine + "\n#EXTINF:6.0,\nseg0.ts\n"
			client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
				switch {
				case strings.Contains(rawURL, "ac=videolist"):
					return byteResponse(rawURL, 200, detail), nil
				case strings.HasSuffix(rawURL, "index.m3u8"):
					return textResponse(rawURL, 200, playlist), nil
				case strings.HasSuffix(rawURL, "key.bin"):
					return byteResponse(rawURL, 200, []byte{0x01, 0x02}), nil
				case strings.HasSuffix(rawURL, "seg0.ts"):
					return byteResponse(rawURL, 200, plain), nil
				default:
					return textResponse(rawURL, 404, "not found"), nil
				}
			})
			res := New(Options{Client: client}).Probe(context.Background(), probeItem(testSourceURL, "", "42"))
			if res.Error != "" || res.Height != 0 {
				t.Fatalf("Probe = (%d, %q), want 测不出 (0, \"\")", res.Height, res.Error)
			}
		})
	}
}

func TestProbeDetailPageFallback(t *testing.T) {
	detailPage := `<html><body><h1>测试</h1><a href="$https://cdn.example.com/v/index.m3u8">播放</a></body></html>`
	tests := []struct {
		name       string
		sourceURL  string
		detailRoot string
		wantHost   string
	}{
		{name: "origin 推导", sourceURL: testSourceURL, detailRoot: "", wantHost: "cms.example.com"},
		{name: "source.detail 优先", sourceURL: testSourceURL, detailRoot: "https://detail.example.com", wantHost: "detail.example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
				switch {
				case strings.Contains(rawURL, "ac=videolist"):
					return textResponse(rawURL, 500, "boom"), nil
				case strings.Contains(rawURL, "/index.php/vod/detail/id/42.html"):
					return textResponse(rawURL, 200, detailPage), nil
				case strings.HasSuffix(rawURL, "index.m3u8"):
					return textResponse(rawURL, 200, masterPlaylist("1920x1080")), nil
				default:
					return textResponse(rawURL, 404, "not found"), nil
				}
			})
			res := New(Options{Client: client}).Probe(context.Background(),
				probeItem(tc.sourceURL, tc.detailRoot, "42"))
			if res.Error != "" || res.Height != 1080 {
				t.Fatalf("Probe = (%d, %q), want (1080, \"\")", res.Height, res.Error)
			}
			if n := client.countContaining(tc.wantHost); n == 0 {
				t.Fatalf("详情页请求应打到 %s，实际请求: %v", tc.wantHost, client.urls())
			}
		})
	}
}

func TestProbeTimeout(t *testing.T) {
	client := &fakeDoer{fn: func(ctx context.Context, rawURL string) (*upstream.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	p := New(Options{Client: client, Timeout: 80 * time.Millisecond})
	start := time.Now()
	res := p.Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时探测耗时 %v，应立即返回", elapsed)
	}
	if !strings.Contains(res.Error, "超时") {
		t.Fatalf("Error = %q, want 包含「超时」", res.Error)
	}
	if strings.Contains(res.Error, "http") || strings.Contains(res.Error, "example.com") {
		t.Fatalf("错误文案泄露了 URL: %q", res.Error)
	}
}

func TestProbeBlockedSource(t *testing.T) {
	client := &fakeDoer{fn: func(context.Context, string) (*upstream.Response, error) {
		return nil, upstream.ErrBlocked
	}}
	res := New(Options{Client: client}).Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if res.Error != msgBlocked {
		t.Fatalf("Error = %q, want %q", res.Error, msgBlocked)
	}
}

func TestProbeInvalidInput(t *testing.T) {
	client := &fakeDoer{}
	p := New(Options{Client: client})
	tests := []model.QualityProbeItem{
		probeItem("ftp://cms.example.com/api", "", "42"),
		probeItem("", "", "42"),
		probeItem(testSourceURL, "", ""),
		probeItem(testSourceURL, "", "a/b"),
		probeItem(testSourceURL, "", strings.Repeat("a", 65)),
	}
	for _, item := range tests {
		res := p.Probe(context.Background(), item)
		if res.Error != msgInvalidItem {
			t.Fatalf("Probe(%+v).Error = %q, want %q", item, res.Error, msgInvalidItem)
		}
	}
	if client.count() != 0 {
		t.Fatalf("非法输入不应发起请求，实际 %d 次", client.count())
	}
}

func TestProbeNilClient(t *testing.T) {
	res := New(Options{}).Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if res.Error != msgNotConfigured {
		t.Fatalf("Error = %q, want %q", res.Error, msgNotConfigured)
	}
}

func TestProbeConfigurationDefaults(t *testing.T) {
	p := New(Options{})
	if p.timeout != defaultTimeout {
		t.Fatalf("timeout = %v, want %v", p.timeout, defaultTimeout)
	}
	if p.concurrency != defaultConcurrency || p.hostConcurrency != defaultHostConcurrency {
		t.Fatalf("并发默认值 = (%d,%d), want (%d,%d)",
			p.concurrency, p.hostConcurrency, defaultConcurrency, defaultHostConcurrency)
	}
	if p.ua == "" {
		t.Fatal("UA 应有默认值")
	}
	if p.log == nil {
		t.Fatal("Logger 应有默认值")
	}

	short := New(Options{Timeout: 100 * time.Millisecond})
	if short.segmentBudget > short.timeout {
		t.Fatalf("segmentBudget = %v, 不应超过 timeout %v", short.segmentBudget, short.timeout)
	}
}

func TestProbeCachesSuccessfulResult(t *testing.T) {
	detail := detailJSON(t, "42", []string{testEpisodeURL})
	client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist"):
			return byteResponse(rawURL, 200, detail), nil
		case strings.HasSuffix(rawURL, "index.m3u8"):
			return textResponse(rawURL, 200, masterPlaylist("1920x1080")), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	})
	store := newFakeStore()
	p := New(Options{Client: client, Cache: store})

	first := p.Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if first.Error != "" || first.Height != 1080 {
		t.Fatalf("首次 Probe = (%d, %q), want (1080, \"\")", first.Height, first.Error)
	}
	calls := client.count()
	second := p.Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if second.Error != "" || second.Height != 1080 {
		t.Fatalf("缓存命中 Probe = (%d, %q), want (1080, \"\")", second.Height, second.Error)
	}
	if client.count() != calls {
		t.Fatalf("缓存命中后仍发起了请求：%d -> %d", calls, client.count())
	}
	ttl, ok := store.recordedTTL()
	if !ok || ttl != successTTL {
		t.Fatalf("成功结果 TTL = %v (ok=%v), want %v", ttl, ok, successTTL)
	}
}

func TestProbeCachesUnknownAsSuccess(t *testing.T) {
	detail := detailJSON(t, "42", []string{testEpisodeURL})
	playlist := mediaPlaylist("", "", "seg0.ts")
	client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist"):
			return byteResponse(rawURL, 200, detail), nil
		case strings.HasSuffix(rawURL, "index.m3u8"):
			return textResponse(rawURL, 200, playlist), nil
		case strings.HasSuffix(rawURL, "seg0.ts"):
			return textResponse(rawURL, 200, "<html>not media</html>"), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	})
	store := newFakeStore()
	p := New(Options{Client: client, Cache: store})

	first := p.Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if first.Error != "" || first.Height != 0 {
		t.Fatalf("测不出应为 (0, \"\")，实际 (%d, %q)", first.Height, first.Error)
	}
	calls := client.count()
	second := p.Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if second.Error != "" || second.Height != 0 {
		t.Fatalf("缓存命中 Probe = (%d, %q), want (0, \"\")", second.Height, second.Error)
	}
	if client.count() != calls {
		t.Fatalf("「测不出」也应缓存，实际请求 %d -> %d", calls, client.count())
	}
	ttl, ok := store.recordedTTL()
	if !ok || ttl != successTTL {
		t.Fatalf("「测不出」TTL = %v (ok=%v), want %v", ttl, ok, successTTL)
	}
}

func TestProbeCachesFailureWithShortTTL(t *testing.T) {
	client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
		if strings.Contains(rawURL, "ac=videolist") {
			return byteResponse(rawURL, 200, emptyDetailJSON()), nil
		}
		return textResponse(rawURL, 404, "not found"), nil
	})
	store := newFakeStore()
	p := New(Options{Client: client, Cache: store})

	first := p.Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if first.Error != msgNoEpisodes {
		t.Fatalf("Error = %q, want %q", first.Error, msgNoEpisodes)
	}
	calls := client.count()
	second := p.Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if second.Error != msgNoEpisodes {
		t.Fatalf("缓存命中 Error = %q, want %q", second.Error, msgNoEpisodes)
	}
	if client.count() != calls {
		t.Fatalf("失败结果也应缓存，实际请求 %d -> %d", calls, client.count())
	}
	ttl, ok := store.recordedTTL()
	if !ok || ttl != failureTTL {
		t.Fatalf("失败结果 TTL = %v (ok=%v), want %v", ttl, ok, failureTTL)
	}
}

func TestProbeForbiddenStopsEpisodeRetry(t *testing.T) {
	episodes := []string{
		"https://cdn.example.com/e0.m3u8",
		"https://cdn.example.com/e1.m3u8",
		"https://cdn.example.com/e2.m3u8",
	}
	detail := detailJSON(t, "42", episodes)
	client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist"):
			return byteResponse(rawURL, 200, detail), nil
		case strings.HasSuffix(rawURL, ".m3u8"):
			return textResponse(rawURL, 403, "forbidden"), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	})
	res := New(Options{Client: client}).Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if res.Error != "上游返回 HTTP 403" {
		t.Fatalf("Error = %q, want 上游返回 HTTP 403", res.Error)
	}
	if client.countContaining("e1.m3u8") != 1 {
		t.Fatalf("应探测中间集一次，实际 %v", client.urls())
	}
	if client.countContaining("e2.m3u8") != 0 {
		t.Fatalf("401/403 后不应换集重试，实际请求 %v", client.urls())
	}
}

func TestProbeHalfTimeSkipsLastEpisode(t *testing.T) {
	episodes := []string{
		"https://cdn.example.com/e0.m3u8",
		"https://cdn.example.com/e1.m3u8",
		"https://cdn.example.com/e2.m3u8",
	}
	detail := detailJSON(t, "42", episodes)
	midPlaylist := mediaPlaylist("", "", "half.ts")
	client := &fakeDoer{fn: func(ctx context.Context, rawURL string) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist"):
			return byteResponse(rawURL, 200, detail), nil
		case strings.HasSuffix(rawURL, "e1.m3u8"):
			return textResponse(rawURL, 200, midPlaylist), nil
		case strings.HasSuffix(rawURL, "half.ts"):
			select {
			case <-time.After(300 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return textResponse(rawURL, 200, "garbage"), nil
		case strings.HasSuffix(rawURL, "e2.m3u8"):
			return textResponse(rawURL, 200, masterPlaylist("1920x1080")), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	}}
	p := New(Options{Client: client, Timeout: 500 * time.Millisecond})
	res := p.Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if res.Error != "" || res.Height != 0 {
		t.Fatalf("Probe = (%d, %q), want 测不出 (0, \"\")", res.Height, res.Error)
	}
	if client.countContaining("e2.m3u8") != 0 {
		t.Fatalf("时间过半后不应再探最后一集，实际请求 %v", client.urls())
	}
}

func TestProbeSingleflightMergesConcurrentCalls(t *testing.T) {
	detail := detailJSON(t, "42", []string{testEpisodeURL})
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	client := &fakeDoer{fn: func(ctx context.Context, rawURL string) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist"):
			once.Do(func() { close(started) })
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return byteResponse(rawURL, 200, detail), nil
		case strings.HasSuffix(rawURL, "index.m3u8"):
			return textResponse(rawURL, 200, masterPlaylist("1280x720")), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	}}
	p := New(Options{Client: client, Timeout: 2 * time.Second})

	results := make(chan model.QualityProbeResult, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- p.Probe(context.Background(), probeItem(testSourceURL, "", "42"))
		}()
	}
	<-started
	time.Sleep(30 * time.Millisecond) // 让第二个调用加入 singleflight
	close(release)
	wg.Wait()
	close(results)

	for res := range results {
		if res.Error != "" || res.Height != 720 {
			t.Fatalf("并发 Probe = (%d, %q), want (720, \"\")", res.Height, res.Error)
		}
	}
	if n := client.countContaining("ac=videolist"); n != 1 {
		t.Fatalf("singleflight 未合并：详情接口调用 %d 次，want 1", n)
	}
}

func TestProbeCanceledDoesNotCache(t *testing.T) {
	client := &fakeDoer{fn: func(ctx context.Context, rawURL string) (*upstream.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	store := newFakeStore()
	p := New(Options{Client: client, Cache: store, Timeout: 100 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	res := p.Probe(ctx, probeItem(testSourceURL, "", "42"))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("客户端取消后应立即返回，实际耗时 %v", elapsed)
	}
	if res.Error != msgCanceled {
		t.Fatalf("Error = %q, want %q", res.Error, msgCanceled)
	}
	if store.size() != 0 {
		t.Fatalf("取消的探测不应写缓存，实际写入 %d 条", store.size())
	}
}

func TestProbeManyEmitsSerially(t *testing.T) {
	const count = 5
	detailByID := make(map[string][]byte, count)
	items := make([]model.QualityProbeItem, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%d", i+1)
		items[i] = probeItem(testSourceURL, "", id)
		detailByID[id] = detailJSON(t, id, []string{"https://cdn.example.com/v/" + id + ".m3u8"})
	}
	client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist"):
			for id, detail := range detailByID {
				if strings.Contains(rawURL, "ids="+id) {
					return byteResponse(rawURL, 200, detail), nil
				}
			}
			return textResponse(rawURL, 404, "unknown id"), nil
		case strings.HasSuffix(rawURL, ".m3u8"):
			return textResponse(rawURL, 200, masterPlaylist("1920x1080")), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	})

	var (
		mu          sync.Mutex
		got         []model.QualityProbeResult
		inflight    int
		maxInflight int
	)
	p := New(Options{Client: client, Concurrency: 3})
	p.ProbeMany(context.Background(), items, func(res model.QualityProbeResult) error {
		mu.Lock()
		inflight++
		if inflight > maxInflight {
			maxInflight = inflight
		}
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
		got = append(got, res)
		inflight--
		mu.Unlock()
		return nil
	})

	if len(got) != count {
		t.Fatalf("emit 次数 = %d, want %d", len(got), count)
	}
	if maxInflight != 1 {
		t.Fatalf("emit 并发度 = %d, want 1（必须串行）", maxInflight)
	}
	seen := make(map[string]bool, count)
	for _, res := range got {
		seen[res.VodID] = true
		if res.Error != "" || res.Height != 1080 {
			t.Fatalf("ProbeMany 结果 = (%d, %q), want (1080, \"\")", res.Height, res.Error)
		}
	}
	for i := 0; i < count; i++ {
		if !seen[fmt.Sprintf("%d", i+1)] {
			t.Fatalf("缺少 vodId %d 的结果", i+1)
		}
	}
}

func TestProbeManyStopsOnEmitError(t *testing.T) {
	const count = 6
	detailByID := make(map[string][]byte, count)
	items := make([]model.QualityProbeItem, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%d", i+1)
		items[i] = probeItem(testSourceURL, "", id)
		detailByID[id] = detailJSON(t, id, []string{"https://cdn.example.com/v/" + id + ".m3u8"})
	}
	client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist"):
			for id, detail := range detailByID {
				if strings.Contains(rawURL, "ids="+id) {
					return byteResponse(rawURL, 200, detail), nil
				}
			}
			return textResponse(rawURL, 404, "unknown id"), nil
		case strings.HasSuffix(rawURL, ".m3u8"):
			return textResponse(rawURL, 200, masterPlaylist("1280x720")), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	})

	var emitted int32
	stopErr := errors.New("client gone")
	p := New(Options{Client: client, Concurrency: 2})
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ProbeMany(context.Background(), items, func(model.QualityProbeResult) error {
			if atomic.AddInt32(&emitted, 1) == 2 {
				return stopErr
			}
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ProbeMany 遇到 emit 错误后未及时返回")
	}
	if emitted != 2 {
		t.Fatalf("emit 次数 = %d, want 2（出错后停止）", emitted)
	}
}

func TestProbeManyHostConcurrencyLimit(t *testing.T) {
	const count = 6
	detailByID := make(map[string][]byte, count)
	items := make([]model.QualityProbeItem, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%d", i+1)
		items[i] = probeItem(testSourceURL, "", id)
		detailByID[id] = detailJSON(t, id, []string{"https://cdn.example.com/v/" + id + ".m3u8"})
	}
	var (
		mu        sync.Mutex
		apiNow    int
		apiMax    int
		globalNow int
		globalMax int
	)
	client := &fakeDoer{fn: func(_ context.Context, rawURL string) (*upstream.Response, error) {
		isAPI := strings.Contains(rawURL, "ac=videolist")
		mu.Lock()
		globalNow++
		if globalNow > globalMax {
			globalMax = globalNow
		}
		if isAPI {
			apiNow++
			if apiNow > apiMax {
				apiMax = apiNow
			}
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		globalNow--
		if isAPI {
			apiNow--
		}
		mu.Unlock()

		switch {
		case isAPI:
			for id, detail := range detailByID {
				if strings.Contains(rawURL, "ids="+id) {
					return byteResponse(rawURL, 200, detail), nil
				}
			}
			return textResponse(rawURL, 404, "unknown id"), nil
		case strings.HasSuffix(rawURL, ".m3u8"):
			return textResponse(rawURL, 200, masterPlaylist("1280x720")), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	}}
	p := New(Options{Client: client, Concurrency: 4, HostConcurrency: 2})
	var emitted int32
	p.ProbeMany(context.Background(), items, func(model.QualityProbeResult) error {
		atomic.AddInt32(&emitted, 1)
		return nil
	})
	if emitted != count {
		t.Fatalf("emit 次数 = %d, want %d", emitted, count)
	}
	if apiMax > 2 {
		t.Fatalf("同 host 并发 = %d, want <= 2", apiMax)
	}
	if globalMax > 4 {
		t.Fatalf("全局并发 = %d, want <= 4", globalMax)
	}
}

func TestProbeManyGlobalConcurrencyLimit(t *testing.T) {
	const count = 8
	items := make([]model.QualityProbeItem, count)
	detailByID := make(map[string][]byte, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%d", i+1)
		items[i] = probeItem(fmt.Sprintf("https://cms%d.example.com/api.php/provide/vod", i), "", id)
		detailByID[id] = detailJSON(t, id, []string{"https://cdn" + id + ".example.com/v/index.m3u8"})
	}
	var (
		mu     sync.Mutex
		now    int
		maxNow int
	)
	client := &fakeDoer{fn: func(_ context.Context, rawURL string) (*upstream.Response, error) {
		mu.Lock()
		now++
		if now > maxNow {
			maxNow = now
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		now--
		mu.Unlock()

		if strings.Contains(rawURL, "ac=videolist") {
			for id, detail := range detailByID {
				if strings.Contains(rawURL, "ids="+id) {
					return byteResponse(rawURL, 200, detail), nil
				}
			}
			return textResponse(rawURL, 404, "unknown id"), nil
		}
		if strings.HasSuffix(rawURL, "index.m3u8") {
			return textResponse(rawURL, 200, masterPlaylist("1920x1080")), nil
		}
		return textResponse(rawURL, 404, "not found"), nil
	}}
	p := New(Options{Client: client, Concurrency: 3, HostConcurrency: 3})
	var emitted int32
	p.ProbeMany(context.Background(), items, func(res model.QualityProbeResult) error {
		atomic.AddInt32(&emitted, 1)
		return nil
	})
	if emitted != count {
		t.Fatalf("emit 次数 = %d, want %d", emitted, count)
	}
	if maxNow > 3 {
		t.Fatalf("全局并发 = %d, want <= 3", maxNow)
	}
	if maxNow < 2 {
		t.Fatalf("全局并发 = %d, 探测未并行执行", maxNow)
	}
}

func TestProbeRequestHeaders(t *testing.T) {
	detail := detailJSON(t, "42", []string{testEpisodeURL})
	client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist"):
			return byteResponse(rawURL, 200, detail), nil
		case strings.HasSuffix(rawURL, "index.m3u8"):
			return textResponse(rawURL, 200, masterPlaylist("1280x720")), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	})
	const customUA = "CiLiCiLi-Probe/1.0"
	p := New(Options{Client: client, UA: customUA})
	res := p.Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if res.Error != "" || res.Height != 720 {
		t.Fatalf("Probe = (%d, %q), want (720, \"\")", res.Height, res.Error)
	}

	var m3u8Call, apiCall *fakeCall
	calls := client.snapshot()
	for i := range calls {
		switch {
		case strings.HasSuffix(calls[i].url, "index.m3u8"):
			m3u8Call = &calls[i]
		case strings.Contains(calls[i].url, "ac=videolist"):
			apiCall = &calls[i]
		}
	}
	if apiCall == nil || m3u8Call == nil {
		t.Fatalf("缺少 API 或 m3u8 请求: %v", client.urls())
	}
	if got := apiCall.opts.Headers.Get("User-Agent"); got != customUA {
		t.Fatalf("API UA = %q, want %q", got, customUA)
	}
	if got := apiCall.opts.Headers.Get("Accept"); got != "application/json" {
		t.Fatalf("API Accept = %q, want application/json", got)
	}
	if got := m3u8Call.opts.Headers.Get("Referer"); got != "https://cms.example.com/" {
		t.Fatalf("m3u8 Referer = %q, want 源站 origin", got)
	}
	if got := m3u8Call.opts.Headers.Get("Accept"); got != "*/*" {
		t.Fatalf("m3u8 Accept = %q, want */*", got)
	}
	if got := m3u8Call.opts.Headers.Get("User-Agent"); got != customUA {
		t.Fatalf("m3u8 UA = %q, want %q", got, customUA)
	}
	if !apiCall.opts.SafeRedirects || !m3u8Call.opts.SafeRedirects {
		t.Fatal("所有出网请求必须开启 SafeRedirects")
	}
}

func TestProbeMP4DirectLink(t *testing.T) {
	const movieURL = "https://cdn.example.com/v/movie.mp4"
	detail := detailJSON(t, "42", []string{movieURL})
	client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist"):
			return byteResponse(rawURL, 200, detail), nil
		case strings.HasSuffix(rawURL, "movie.mp4"):
			return byteResponse(rawURL, 200, fmp4Init(1920, 1080)), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	})
	res := New(Options{Client: client}).Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if res.Error != "" || res.Height != 1080 {
		t.Fatalf("mp4 直链 Probe = (%d, %q), want (1080, \"\")", res.Height, res.Error)
	}
}

func TestProbeUnknownEpisodeFallsThroughToLast(t *testing.T) {
	episodes := []string{
		"https://cdn.example.com/e0.m3u8",
		"https://cdn.example.com/e1.flv", // 非 m3u8/mp4：未知，不报错
		"https://cdn.example.com/e2.m3u8",
	}
	detail := detailJSON(t, "42", episodes)
	client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist"):
			return byteResponse(rawURL, 200, detail), nil
		case strings.HasSuffix(rawURL, "e2.m3u8"):
			return textResponse(rawURL, 200, masterPlaylist("1920x1080")), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	})
	res := New(Options{Client: client}).Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if res.Error != "" || res.Height != 1080 {
		t.Fatalf("Probe = (%d, %q), want (1080, \"\")", res.Height, res.Error)
	}
	if client.countContaining("e1.flv") != 0 {
		t.Fatalf("非 m3u8/mp4 地址不应发起请求: %v", client.urls())
	}
}

func TestProbeCorruptCacheFallsBackToNetwork(t *testing.T) {
	detail := detailJSON(t, "42", []string{testEpisodeURL})
	client := simpleDoer(func(rawURL string) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist"):
			return byteResponse(rawURL, 200, detail), nil
		case strings.HasSuffix(rawURL, "index.m3u8"):
			return textResponse(rawURL, 200, masterPlaylist("1280x720")), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	})
	store := newFakeStore()
	item := probeItem(testSourceURL, "", "42")
	store.Set(cacheKey(item), []byte("not-json"), successTTL, successTTL)

	res := New(Options{Client: client, Cache: store}).Probe(context.Background(), item)
	if res.Error != "" || res.Height != 720 {
		t.Fatalf("坏缓存应回源重探，实际 (%d, %q)", res.Height, res.Error)
	}
}

type timeoutTestError struct{}

func (timeoutTestError) Error() string   { return "i/o timeout" }
func (timeoutTestError) Timeout() bool   { return true }
func (timeoutTestError) Temporary() bool { return true }

func TestClassify(t *testing.T) {
	p := New(Options{})
	ctx := context.Background()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{name: "SSRF 拦截", err: upstream.ErrBlocked, want: msgBlocked},
		{name: "响应过大", err: upstream.ErrTooLarge, want: msgTooLarge},
		{name: "HTTP 状态", err: &upstream.StatusError{StatusCode: 502}, want: "上游返回 HTTP 502"},
		{name: "死线", err: context.DeadlineExceeded, want: msgTimeout},
		{name: "取消", err: context.Canceled, want: msgCanceled},
		{name: "网络超时", err: timeoutTestError{}, want: msgTimeout},
		{name: "其它失败", err: errors.New("dial tcp 10.1.2.3: connect: refused"), want: msgRequestFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.classify(ctx, tc.err); got != tc.want {
				t.Fatalf("classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestProbeRedirectedPlaylistResolvesRelativeSegments(t *testing.T) {
	const requested = "https://cdn.example.com/a/index.m3u8"
	const redirected = "https://edge.example.com/b/index.m3u8"
	detail := detailJSON(t, "42", []string{requested})
	playlist := mediaPlaylist("", "", "seg0.ts")
	client := &fakeDoer{fn: func(_ context.Context, rawURL string) (*upstream.Response, error) {
		switch {
		case strings.Contains(rawURL, "ac=videolist"):
			return byteResponse(rawURL, 200, detail), nil
		case strings.HasSuffix(rawURL, "/a/index.m3u8"):
			// 源站 gslb 调度：清单被 302 到另一个目录，相对分片必须基于最终 URL。
			return responseWithFinal(rawURL, redirected, 200, []byte(playlist)), nil
		case strings.HasSuffix(rawURL, "/b/seg0.ts"):
			return byteResponse(rawURL, 200, buildTSStream(1280, 720)), nil
		default:
			return textResponse(rawURL, 404, "not found"), nil
		}
	}}
	res := New(Options{Client: client}).Probe(context.Background(), probeItem(testSourceURL, "", "42"))
	if res.Error != "" || res.Height != 720 {
		t.Fatalf("Probe = (%d, %q), want (720, \"\")", res.Height, res.Error)
	}
	if client.countContaining("/b/seg0.ts") != 1 {
		t.Fatalf("分片应按重定向后的最终 URL 解析，实际请求 %v", client.urls())
	}
	if client.countContaining("/a/seg0.ts") != 0 {
		t.Fatalf("不应按请求前 URL 解析分片，实际请求 %v", client.urls())
	}
}
