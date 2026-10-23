//go:build smoke

package recommend

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"cilicili/internal/upstream"
)

// TestSmokeRealAPIs 是可选的联网冒烟测试（默认测试完全不联网，此文件只在显式
// 启用 smoke 标签时编译）：
//
//	go test -tags smoke -run TestSmokeRealAPIs -v ./internal/recommend/
//
// 用来确认三类推荐接口的真实路径与字段仍与实现一致。
func TestSmokeRealAPIs(t *testing.T) {
	client, err := upstream.New(upstream.Config{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}
	fetcher := New(Options{Client: client, Logger: slog.New(slog.DiscardHandler), Timeout: 20 * time.Second})
	ctx := context.Background()

	items, err := fetcher.Douban(ctx, "movie", "热门", 0, 5)
	if err != nil {
		t.Errorf("Douban: %v", err)
	} else {
		t.Logf("Douban movie 热门: %d 条，首条=%+v", len(items), items[0])
	}

	// 已知环境问题（2026-10 本机实测）：api.bgm.tv 的 AAAA 是 2001::9df0:1408（2001::/32
	// Teredo 段），upstream 的 SSRF 策略「任一解析结果被拒就拒绝整个域名」，因此这里必然
	// 返回「目标地址不可用」。解析逻辑本身由 fixture 单测覆盖，此处只记录不判失败。
	if days, err := fetcher.BangumiCalendar(ctx); err != nil {
		t.Logf("BangumiCalendar 不可达（若为 DNS 返回 2001::/32，属 upstream SSRF 策略）: %v", err)
	} else {
		t.Logf("Bangumi: %d 天，周一条目 %d", len(days), len(days[0].Items))
	}

	for _, id := range HotListIDs() {
		items, err := fetcher.HotList(ctx, id)
		if err != nil {
			t.Errorf("HotList(%s): %v", id, err)
			continue
		}
		t.Logf("HotList(%s): %d 条，首条=%+v", id, len(items), items[0])
	}
}
