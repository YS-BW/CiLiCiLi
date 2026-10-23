// Package quality 实现服务端画质探测。
//
// 合同：
//
//	func New(opts Options) *Prober
//	func (p *Prober) Probe(ctx context.Context, item model.QualityProbeItem) model.QualityProbeResult
//	func (p *Prober) ProbeMany(ctx context.Context, items []model.QualityProbeItem, emit func(model.QualityProbeResult) error)
//
// 语义：
//   - 优先解析 m3u8 的 #EXT-X-STREAM-INF RESOLUTION/BANDWIDTH（多码率源）；
//   - 单码率源下沉到首分片：MP4 moov / TS SPS 里提取分辨率；AES-128 加密时先取 key 解密；
//   - HEVC/H.265 TS 分片暂不支持（返回“测不出”，不当作失败）；
//   - 结果缓存：成功 30 天、失败 2 分钟；ProbeMany 内部按 host 限并发，避免打挂弱源；
//   - 单条总死线默认 8s，客户端断开立即停止。
package quality

import (
	"log/slog"
	"time"

	"cilicili/internal/cache"
	"cilicili/internal/upstream"
)

// Options 是画质探测器的构造参数。
type Options struct {
	Client upstream.Doer
	Cache  cache.Store
	Logger *slog.Logger
	UA     string
	// Timeout 是单条探测总死线。
	Timeout time.Duration
	// Concurrency 是 ProbeMany 的并发上限。
	Concurrency int
	// HostConcurrency 是同一 host 的并发上限。
	HostConcurrency int
}
