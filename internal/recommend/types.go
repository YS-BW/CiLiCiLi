// Package recommend 实现首页推荐数据源：豆瓣、影视榜单（60s API）。
//
// 合同：
//
//	func New(opts Options) *Fetcher
//	func (f *Fetcher) Douban(ctx context.Context, typ, tag string, pageStart, pageSize int) ([]model.DoubanItem, error)
//
// //	func (f *Fetcher) HotList(ctx context.Context, id string) ([]model.DoubanItem, error)
//
// 语义：
//   - 豆瓣：服务端直连 + 10 分钟缓存；封面防盗链由代理层处理；
//   - Bangumi：免 key，30 分钟缓存；
//   - 榜单：经 60s API（可配置 base），1 小时缓存；未知榜单 id 报错。
package recommend

import (
	"log/slog"
	"time"

	"cilicili/internal/cache"
	"cilicili/internal/upstream"
)

// Options 是推荐服务的构造参数。
type Options struct {
	Client upstream.Doer
	Cache  cache.Store
	Logger *slog.Logger
	UA     string
	// SixtyAPIBase 是 60s API 实例地址，默认 https://60s.crystelf.top。
	SixtyAPIBase string
	// Timeout 单次上游请求超时。
	Timeout time.Duration
}
