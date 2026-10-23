// Package live 实现直播订阅（M3U）、XMLTV 节目单、频道测活。
//
// 合同：
//
//	type Service struct{...}
//	func New(opts Options) *Service
//	func (s *Service) Playlist(ctx context.Context, url string, force bool) (*model.LivePlaylistResponse, error)
//	func (s *Service) EPG(ctx context.Context, epgURL, channel string, force bool) (*model.LiveEpgResponse, error)
//	func (s *Service) Probe(ctx context.Context, urls []string, emit func(model.LiveProbeResult) error)
//	func ParseM3U(data []byte) (*model.LivePlaylistResponse, error)
//	func ParseXMLTV(data []byte, window time.Duration, now time.Time) (map[string][]model.EpgProgram, error)
//	func CurrentAndNext(programs []model.EpgProgram, now time.Time) (*model.EpgProgram, *model.EpgProgram)
//
// 语义（与 LibreTV 对齐）：
//   - M3U：#EXTM3U；#EXTINF:-1 tvg-id/name/logo/group-title,Name；下一行 URL；跳过非 http(s) URL；
//     频道 id 优先 tvg-id，缺失用 URL 的稳定 hash；groups 去重排序。
//   - XMLTV：支持 gzip；只保留 now 前后 window 内的节目；按 channel 索引。
//   - 测活：m3u8 → variant → 分片（Range bytes=0-1 只取响应头）；非 m3u8 单级探测；
//     结果带短缓存（成功 30min / 失败 2min），按 host 限并发 3，客户端断开即取消。
package live

import (
	"log/slog"
	"time"

	"cilicili/internal/cache"
	"cilicili/internal/upstream"
)

// Options 是直播服务的构造参数。
type Options struct {
	Client           upstream.Doer
	Cache            cache.Store
	Logger           *slog.Logger
	UA               string
	AllowPrivate     bool
	Timeout          time.Duration
	ProbeConcurrency int
}
