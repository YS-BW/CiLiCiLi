// Package subscribe 解析远程数据源订阅。
//
// 合同：
//
//	func New(opts Options) *Fetcher
//	func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*model.SourceListPayload, error)
//	func Parse(data []byte) (*model.SourceListPayload, error)
//
// 支持三种格式：
//  1. LibreTV-SourceList JSON：{name?, sources:[{name,url,detail,isAdult}], liveSources:[{name,url,epg}]}；
//     也接受裸 sources 数组与老格式（只有 sources）。
//  2. TVBOX 配置：{sites:[{key,name,api,type,...}], lives:[...]}；只导入 type=1 的 Apple CMS 接口与 M3U；
//     Spider/XML/不可搜索站点计入 skipped 统计。
//  3. 接口目录：裸数组 [{name, baseUrl, enabled}]，仅点播。
//
// 语义：
//   - 去重后再按上限截断（点播 100 / 直播 50）；
//   - URL 只接受 http(s)；去掉尾部空 ? / # 与斜杠；
//   - 源 key 用 URL hash 生成稳定值（sub_<8hex>），保证健康状态跨订阅刷新稳定；
//   - stats 记录格式、跳过原因计数与前 3 个样例、截断数。
package subscribe

import (
	"log/slog"
	"time"

	"cilicili/internal/upstream"
)

// Options 是订阅拉取器的构造参数。
type Options struct {
	Client  upstream.Doer
	Logger  *slog.Logger
	Timeout time.Duration
}
