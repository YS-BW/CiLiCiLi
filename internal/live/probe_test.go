package live

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cilicili/internal/cache"
	"cilicili/internal/model"
	"cilicili/internal/upstream"
)

func probeCollect(t *testing.T, svc *Service, urls []string) []model.LiveProbeResult {
	t.Helper()
	var results []model.LiveProbeResult
	svc.Probe(context.Background(), urls, func(res model.LiveProbeResult) error {
		results = append(results, res)
		return nil
	})
	return results
}

// m3u8Server 模拟一套完整的 HLS 源：master → variant → 分片。
func m3u8Server(t *testing.T) *fakeDoer {
	t.Helper()
	const (
		entryURL = "https://cdn.example.com/live/index.m3u8"
		mediaURL = "https://cdn.example.com/live/media.m3u8"
		segURL   = "https://cdn.example.com/live/seg2.ts"
	)
	payload := bytes.Repeat([]byte("t"), probeThroughputBytes)

	return &fakeDoer{fn: func(_ context.Context, rawURL string, opts upstream.Options) (*upstream.Response, error) {
		switch rawURL {
		case entryURL:
			if got := opts.Headers.Get("Range"); got != "" {
				t.Errorf("manifest 请求不应带 Range，实际 %q", got)
			}
			return testResponse(200, "application/vnd.apple.mpegurl", `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=2000000,CODECS="hvc1.1.6.L93.B0,mp4a.40.2"
media.m3u8
`, rawURL), nil
		case mediaURL:
			return testResponse(200, "application/vnd.apple.mpegurl", `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:100
#EXTINF:6.0,
seg1.ts
#EXTINF:6.0,
seg2.ts
`, rawURL), nil
		case segURL:
			switch opts.Headers.Get("Range") {
			case "bytes=0-1":
				return testResponse(206, "video/mp2t", "", rawURL), nil
			case fmt.Sprintf("bytes=0-%d", probeThroughputBytes-1):
				return testResponseBody(206, "video/mp2t",
					&slowReader{data: payload, chunk: 8 << 10, delay: 10 * time.Millisecond}, rawURL), nil
			default:
				return testResponse(200, "video/mp2t", "", rawURL), nil
			}
		}
		return nil, fmt.Errorf("未预期的请求: %s", rawURL)
	}}
}

func TestProbeM3U8ThreeLevels(t *testing.T) {
	t.Parallel()

	doer := m3u8Server(t)
	svc, _ := newTestService(t, doer)
	const entryURL = "https://cdn.example.com/live/index.m3u8"

	results := probeCollect(t, svc, []string{entryURL})
	if len(results) != 1 {
		t.Fatalf("结果数 = %d", len(results))
	}
	res := results[0]
	if !res.OK {
		t.Fatalf("探测失败: %+v", res)
	}
	if res.Level != "segment" {
		t.Errorf("Level = %q，期望 segment", res.Level)
	}
	if res.Codec != "hvc1.1.6.L93.B0,mp4a.40.2" {
		t.Errorf("Codec = %q", res.Codec)
	}
	if res.Status != 206 {
		t.Errorf("Status = %d，期望 206", res.Status)
	}
	if res.Kbps <= 0 {
		t.Errorf("Kbps = %d，期望 >0（吞吐采样）", res.Kbps)
	}

	// 分片探测只请求最新分片（seg2），不请求最早过期分片。
	for _, call := range doer.callsFor("https://cdn.example.com/live/seg1.ts") {
		t.Errorf("不应请求最早分片: %+v", call)
	}
	segCalls := doer.callsFor("https://cdn.example.com/live/seg2.ts")
	if len(segCalls) != 2 {
		t.Fatalf("分片请求数 = %d，期望 2（可达性 + 吞吐采样）", len(segCalls))
	}
	if got := segCalls[0].header.Get("Range"); got != "bytes=0-1" {
		t.Errorf("分片可达性探测 Range = %q", got)
	}
	if got := segCalls[1].header.Get("Range"); got != fmt.Sprintf("bytes=0-%d", probeThroughputBytes-1) {
		t.Errorf("吞吐采样 Range = %q", got)
	}
}

func TestProbeMediaPlaylistDirectAndMap(t *testing.T) {
	t.Parallel()

	entryURL := "https://cdn.example.com/live/media.m3u8"
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		switch rawURL {
		case entryURL:
			return testResponse(200, "application/vnd.apple.mpegurl", `#EXTM3U
#EXT-X-MAP:URI="init.mp4"
#EXTINF:6.0,
seg1.ts
#EXTINF:6.0,
seg2.ts
`, rawURL), nil
		case "https://cdn.example.com/live/init.mp4", "https://cdn.example.com/live/seg2.ts":
			return testResponse(206, "video/mp4", "", rawURL), nil
		}
		return nil, fmt.Errorf("未预期的请求: %s", rawURL)
	}}
	svc, _ := newTestService(t, doer)
	// 跳过吞吐采样，避免本用例关注点扩散。
	svc.probeBudget = probeThroughputMinRemaining

	results := probeCollect(t, svc, []string{entryURL})
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("探测结果 = %+v", results)
	}
	if results[0].Level != "segment" {
		t.Errorf("Level = %q", results[0].Level)
	}
	if got := len(doer.callsFor("https://cdn.example.com/live/init.mp4")); got != 1 {
		t.Errorf("EXT-X-MAP 初始化段请求数 = %d，期望 1", got)
	}
	if got := len(doer.callsFor("https://cdn.example.com/live/seg2.ts")); got != 0 {
		t.Errorf("有 EXT-X-MAP 时不应再探测媒体分片，实际 %d 次", got)
	}
}

func TestProbeEmptyMediaPlaylistIsManifestLevel(t *testing.T) {
	t.Parallel()

	entryURL := "https://cdn.example.com/live/empty.m3u8"
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		return testResponse(200, "application/vnd.apple.mpegurl", `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:1
`, rawURL), nil
	}}
	svc, _ := newTestService(t, doer)

	results := probeCollect(t, svc, []string{entryURL})
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("探测结果 = %+v", results)
	}
	if results[0].Level != "manifest" {
		t.Errorf("Level = %q，期望 manifest", results[0].Level)
	}
}

func TestProbeRejectsErrorPages(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		url         string
		contentType string
		body        string
		status      int
		wantErr     string
		wantStatus  int
	}{
		{
			name:        "直链返回HTML",
			url:         "https://cdn.example.com/live/channel.flv",
			contentType: "text/html",
			body:        "<html>403 forbidden</html>",
			status:      200,
			wantErr:     "不是流媒体",
		},
		{
			name:        "m3u8伪装的HTML",
			url:         "https://cdn.example.com/live/fake.m3u8",
			contentType: "text/html",
			body:        "<html>error</html>",
			status:      200,
			wantErr:     "不是有效的 m3u8",
		},
		{
			name:        "直链返回JSON",
			url:         "https://cdn.example.com/live/channel.flv",
			contentType: "application/json",
			body:        `{"code":403}`,
			status:      200,
			wantErr:     "不是流媒体",
		},
		{
			name:       "入口非2xx",
			url:        "https://cdn.example.com/live/channel.flv",
			status:     500,
			body:       "oops",
			wantErr:    "入口响应 500",
			wantStatus: 500,
		},
		{
			name:        "master缺少流地址",
			url:         "https://cdn.example.com/live/nostream.m3u8",
			contentType: "application/vnd.apple.mpegurl",
			body:        "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n# no url\n",
			status:      200,
			wantErr:     "没有可用的流地址",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
				return testResponse(tc.status, tc.contentType, tc.body, rawURL), nil
			}}
			svc, _ := newTestService(t, doer)
			results := probeCollect(t, svc, []string{tc.url})
			if len(results) != 1 {
				t.Fatalf("结果数 = %d", len(results))
			}
			res := results[0]
			if res.OK {
				t.Fatalf("应判定失败: %+v", res)
			}
			if !strings.Contains(res.Error, tc.wantErr) {
				t.Errorf("Error = %q，期望包含 %q", res.Error, tc.wantErr)
			}
			if tc.wantStatus != 0 && res.Status != tc.wantStatus {
				t.Errorf("Status = %d，期望 %d", res.Status, tc.wantStatus)
			}
		})
	}
}

func TestProbeSegmentForbidden(t *testing.T) {
	t.Parallel()

	entryURL := "https://cdn.example.com/live/index.m3u8"
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		switch rawURL {
		case entryURL:
			return testResponse(200, "application/vnd.apple.mpegurl",
				"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nmedia.m3u8\n", rawURL), nil
		case "https://cdn.example.com/live/media.m3u8":
			return testResponse(200, "application/vnd.apple.mpegurl",
				"#EXTM3U\n#EXTINF:6,\nseg.ts\n", rawURL), nil
		default:
			return testResponse(403, "text/plain", "forbidden", rawURL), nil
		}
	}}
	svc, _ := newTestService(t, doer)

	results := probeCollect(t, svc, []string{entryURL})
	if len(results) != 1 {
		t.Fatalf("结果数 = %d", len(results))
	}
	res := results[0]
	if res.OK || res.Level != "segment" || res.Status != 403 {
		t.Fatalf("探测结果 = %+v，期望分片 403 失败", res)
	}
	if !strings.Contains(res.Error, "分片响应 403") {
		t.Errorf("Error = %q", res.Error)
	}
}

func TestProbePlaceholderRedirect(t *testing.T) {
	t.Parallel()

	const entryURL = "https://cdn.example.com/live/channel.flv"
	doer := &fakeDoer{fn: func(_ context.Context, _ string, _ upstream.Options) (*upstream.Response, error) {
		return testResponse(200, "video/mp4", "", "https://static.example.com/status/error_account_pirated.mp4"), nil
	}}
	svc, _ := newTestService(t, doer)

	results := probeCollect(t, svc, []string{entryURL})
	if len(results) != 1 || results[0].OK {
		t.Fatalf("探测结果 = %+v，期望失败", results)
	}
	if !strings.Contains(results[0].Error, "错误占位内容") {
		t.Errorf("Error = %q", results[0].Error)
	}
}

func TestProbeNormalRedirectNotPlaceholder(t *testing.T) {
	t.Parallel()

	const entryURL = "https://cdn.example.com/live/channel.flv"
	doer := &fakeDoer{fn: func(_ context.Context, _ string, _ upstream.Options) (*upstream.Response, error) {
		return testResponse(200, "video/x-flv", "", "https://cdn2.example.com/edge/channel.flv"), nil
	}}
	svc, _ := newTestService(t, doer)

	results := probeCollect(t, svc, []string{entryURL})
	if len(results) != 1 || !results[0].OK || results[0].Level != "head" {
		t.Fatalf("探测结果 = %+v，期望 head 级通过", results)
	}
}

func TestProbeTimeout(t *testing.T) {
	t.Parallel()

	doer := &fakeDoer{fn: func(ctx context.Context, _ string, _ upstream.Options) (*upstream.Response, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return nil, errors.New("不应等待到 5s")
		}
	}}
	svc, _ := newTestService(t, doer)
	svc.probeBudget = 150 * time.Millisecond

	start := time.Now()
	results := probeCollect(t, svc, []string{"https://cdn.example.com/live/slow.m3u8"})
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("超时预算未生效，耗时 %v", elapsed)
	}
	if len(results) != 1 {
		t.Fatalf("结果数 = %d", len(results))
	}
	res := results[0]
	if res.OK || !res.TimedOut {
		t.Fatalf("探测结果 = %+v，期望 TimedOut", res)
	}
	if !strings.Contains(res.Error, "探测超时") {
		t.Errorf("Error = %q", res.Error)
	}
}

func TestProbeCacheTTL(t *testing.T) {
	t.Parallel()

	clock := newFixedClock(time.Now())
	var calls int32
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		atomic.AddInt32(&calls, 1)
		if strings.Contains(rawURL, "fail") {
			return testResponse(500, "", "boom", rawURL), nil
		}
		return testResponse(200, "video/x-flv", "", rawURL), nil
	}}
	svc, _ := newTestService(t, doer)
	svc.now = clock.now

	okURL := "https://cdn.example.com/live/ok.flv"
	failURL := "https://cdn.example.com/live/fail.flv"

	probeCollect(t, svc, []string{okURL, failURL})
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("首次探测调用数 = %d，期望 2", got)
	}
	// 成功 30min / 失败 2min 内均命中缓存。
	probeCollect(t, svc, []string{okURL, failURL})
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("缓存命中后调用数 = %d，期望 2", got)
	}

	clock.advance(3 * time.Minute)
	probeCollect(t, svc, []string{okURL, failURL})
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("失败缓存过期后调用数 = %d，期望 3（成功仍命中）", got)
	}

	clock.advance(28 * time.Minute)
	probeCollect(t, svc, []string{okURL, failURL})
	if got := atomic.LoadInt32(&calls); got != 5 {
		t.Fatalf("成功缓存过期后调用数 = %d，期望 5", got)
	}
}

func TestProbeConcurrencyAndHostLimit(t *testing.T) {
	t.Parallel()

	type hostState struct {
		inFlight int
		max      int
	}
	var (
		mu      sync.Mutex
		hosts   = map[string]*hostState{}
		total   int
		maxAll  int
		callCnt int32
	)
	doer := &fakeDoer{fn: func(ctx context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		atomic.AddInt32(&callCnt, 1)
		host := hostOf(rawURL)
		mu.Lock()
		st := hosts[host]
		if st == nil {
			st = &hostState{}
			hosts[host] = st
		}
		st.inFlight++
		total++
		if st.inFlight > st.max {
			st.max = st.inFlight
		}
		if total > maxAll {
			maxAll = total
		}
		mu.Unlock()

		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
		}
		mu.Lock()
		st.inFlight--
		total--
		mu.Unlock()
		return testResponse(200, "video/x-flv", "", rawURL), nil
	}}

	store, closeStore := newMemoryStore(t)
	defer closeStore()
	svc := New(Options{Client: doer, Cache: store, Logger: testLogger(), ProbeConcurrency: 8})

	urls := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		urls = append(urls, fmt.Sprintf("https://same-host.example.com/live/ch%d.flv", i))
	}
	results := probeCollect(t, svc, urls)
	if len(results) != len(urls) {
		t.Fatalf("结果数 = %d", len(results))
	}
	for _, res := range results {
		if !res.OK {
			t.Fatalf("探测失败: %+v", res)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hosts["same-host.example.com"].max > probeHostLimit {
		t.Errorf("单 host 最大并发 = %d，期望 ≤ %d", hosts["same-host.example.com"].max, probeHostLimit)
	}
	if maxAll > 8 {
		t.Errorf("全局最大并发 = %d，期望 ≤ 8", maxAll)
	}
	if hosts["same-host.example.com"].max < 2 {
		t.Errorf("单 host 最大并发 = %d，预期测活有并行度", hosts["same-host.example.com"].max)
	}
}

func TestProbeEmitErrorStops(t *testing.T) {
	t.Parallel()

	var calls int32
	doer := &fakeDoer{fn: func(ctx context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		atomic.AddInt32(&calls, 1)
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
		}
		return testResponse(200, "video/x-flv", "", rawURL), nil
	}}
	store, closeStore := newMemoryStore(t)
	defer closeStore()
	svc := New(Options{Client: doer, Cache: store, Logger: testLogger(), ProbeConcurrency: 4})

	urls := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		urls = append(urls, fmt.Sprintf("https://cdn.example.com/live/ch%d.flv", i))
	}
	var emitted int
	errEmit := errors.New("客户端断开")
	svc.Probe(context.Background(), urls, func(model.LiveProbeResult) error {
		emitted++
		return errEmit
	})
	if emitted != 1 {
		t.Errorf("emit 次数 = %d，期望 1", emitted)
	}
	if got := atomic.LoadInt32(&calls); got > 8 {
		t.Errorf("emit 失败后仍在探测: 调用数 = %d", got)
	}
}

func TestProbeDedupesAndFilters(t *testing.T) {
	t.Parallel()

	var calls int32
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		atomic.AddInt32(&calls, 1)
		return testResponse(200, "video/x-flv", "", rawURL), nil
	}}
	svc, _ := newTestService(t, doer)

	results := probeCollect(t, svc, []string{
		"https://cdn.example.com/live/a.flv",
		"https://cdn.example.com/live/a.flv",
		"rtp://239.0.0.1:5000",
		"",
		"not a url",
	})
	if len(results) != 1 {
		t.Fatalf("结果数 = %d，期望 1", len(results))
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("调用数 = %d，期望 1", got)
	}
}

func TestProbeCanceledContextStops(t *testing.T) {
	t.Parallel()

	var calls int32
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		atomic.AddInt32(&calls, 1)
		return testResponse(200, "video/x-flv", "", rawURL), nil
	}}
	svc, _ := newTestService(t, doer)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var emitted int
	svc.Probe(ctx, []string{"https://cdn.example.com/live/a.flv"}, func(model.LiveProbeResult) error {
		emitted++
		return nil
	})
	if emitted != 0 || atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("已取消的查询不应发起探测: emitted=%d calls=%d", emitted, calls)
	}
}

func TestProbeNoIPLeak(t *testing.T) {
	t.Parallel()

	doer := &fakeDoer{fn: func(context.Context, string, upstream.Options) (*upstream.Response, error) {
		return nil, &url.Error{
			Op:  "Get",
			URL: "https://cdn.example.com/live/a.flv",
			Err: errors.New("dial tcp 10.1.2.3:80: connect: connection refused"),
		}
	}}
	svc, _ := newTestService(t, doer)

	results := probeCollect(t, svc, []string{"https://cdn.example.com/live/a.flv"})
	if len(results) != 1 {
		t.Fatalf("结果数 = %d", len(results))
	}
	res := results[0]
	if res.OK || res.Error == "" {
		t.Fatalf("探测结果 = %+v", res)
	}
	if strings.Contains(res.Error, "10.1.2.3") {
		t.Fatalf("错误文案泄露内网 IP: %q", res.Error)
	}
	if !strings.Contains(res.Error, "connection refused") {
		t.Errorf("错误文案丢失可诊断信息: %q", res.Error)
	}
}

func TestProbeNoClient(t *testing.T) {
	t.Parallel()

	svc := New(Options{Logger: testLogger()})
	var results []model.LiveProbeResult
	svc.Probe(context.Background(), []string{"https://cdn.example.com/live/a.flv"}, func(res model.LiveProbeResult) error {
		results = append(results, res)
		return nil
	})
	if len(results) != 1 || results[0].OK || !strings.Contains(results[0].Error, "出网客户端") {
		t.Fatalf("结果 = %+v", results)
	}
}

func TestProbeNilEmitDoesNotPanic(t *testing.T) {
	t.Parallel()

	svc := New(Options{Logger: testLogger()})
	svc.Probe(context.Background(), []string{"https://cdn.example.com/live/a.flv"}, nil)
}

func TestProbeBOMAndLeadingWhitespace(t *testing.T) {
	t.Parallel()

	entryURL := "https://cdn.example.com/live/bom.m3u8"
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		switch rawURL {
		case entryURL:
			return testResponse(200, "application/vnd.apple.mpegurl", "\uFEFF  \n#EXTM3U\n#EXTINF:6,\nseg.ts\n", rawURL), nil
		default:
			return testResponse(206, "video/mp2t", "", rawURL), nil
		}
	}}
	svc, _ := newTestService(t, doer)
	svc.probeBudget = probeThroughputMinRemaining // 跳过吞吐采样

	results := probeCollect(t, svc, []string{entryURL})
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("BOM/前导空白处理失败: %+v", results)
	}
	if results[0].Level != "segment" {
		t.Errorf("Level = %q", results[0].Level)
	}
}

func TestHostLimiter(t *testing.T) {
	t.Parallel()

	limiter := newHostLimiter(2)
	release1, err := limiter.acquire(context.Background(), "host")
	if err != nil {
		t.Fatalf("acquire 失败: %v", err)
	}
	release2, err := limiter.acquire(context.Background(), "host")
	if err != nil {
		t.Fatalf("acquire 失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := limiter.acquire(ctx, "host"); err == nil {
		t.Fatal("槽位已满时应阻塞到 ctx 超时")
	}

	release1()
	release3, err := limiter.acquire(context.Background(), "other-host")
	if err != nil {
		t.Fatalf("不同 host 不应互相阻塞: %v", err)
	}
	release2()
	release3()
}

// newMemoryStore 构造测试用纯内存缓存。
func newMemoryStore(t *testing.T) (cache.Store, func()) {
	t.Helper()
	store, err := cache.New(cache.Config{Logger: testLogger(), MaxMemoryEntries: 1000})
	if err != nil {
		t.Fatalf("创建测试缓存失败: %v", err)
	}
	return store, func() { _ = store.Close() }
}

func TestProbeCacheBounded(t *testing.T) {
	t.Parallel()

	svc := New(Options{Logger: testLogger()})
	now := time.Now()
	// 塞满（全部过期），再写入新结果：应清理过期条目而不是无限增长。
	for i := 0; i < probeCacheMaxEntries; i++ {
		svc.probeEntries[fmt.Sprintf("https://cdn.example.com/expired/%d.flv", i)] = probeCacheEntry{
			result:    model.LiveProbeResult{URL: "expired"},
			expiresAt: now.Add(-time.Minute),
		}
	}
	svc.storeProbe(model.LiveProbeResult{URL: "https://cdn.example.com/new.flv", OK: true})
	if got := len(svc.probeEntries); got != 1 {
		t.Fatalf("过期条目未被清理，缓存条目数 = %d", got)
	}
	if hit, ok := svc.cachedProbe("https://cdn.example.com/new.flv"); !ok || !hit.OK {
		t.Fatalf("新结果未写入缓存: %+v", hit)
	}

	// 全部未过期时也要有上限：随机淘汰一条后仍能写入。
	svc2 := New(Options{Logger: testLogger()})
	for i := 0; i < probeCacheMaxEntries; i++ {
		svc2.probeEntries[fmt.Sprintf("https://cdn.example.com/live/%d.flv", i)] = probeCacheEntry{
			result:    model.LiveProbeResult{URL: "live"},
			expiresAt: now.Add(time.Hour),
		}
	}
	svc2.storeProbe(model.LiveProbeResult{URL: "https://cdn.example.com/extra.flv"})
	if got := len(svc2.probeEntries); got != probeCacheMaxEntries {
		t.Fatalf("缓存条目数 = %d，期望保持上限 %d", got, probeCacheMaxEntries)
	}
	if _, ok := svc2.cachedProbe("https://cdn.example.com/extra.flv"); !ok {
		t.Fatal("新结果应写入缓存")
	}
}

func TestProbeCachedResultEmittedImmediately(t *testing.T) {
	t.Parallel()

	var calls int32
	doer := &fakeDoer{fn: func(_ context.Context, rawURL string, _ upstream.Options) (*upstream.Response, error) {
		atomic.AddInt32(&calls, 1)
		return testResponse(200, "video/x-flv", "", rawURL), nil
	}}
	svc, _ := newTestService(t, doer)
	target := "https://cdn.example.com/live/cached.flv"

	// 第一次真实探测并写缓存。
	probeCollect(t, svc, []string{target})
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("调用数 = %d", got)
	}

	// 第二次应直接回调缓存结果，不再出网。
	var emitted []model.LiveProbeResult
	svc.Probe(context.Background(), []string{target}, func(res model.LiveProbeResult) error {
		emitted = append(emitted, res)
		return nil
	})
	if len(emitted) != 1 || !emitted[0].OK {
		t.Fatalf("缓存结果未立即回调: %+v", emitted)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("命中缓存后仍出网: 调用数 = %d", got)
	}
}
