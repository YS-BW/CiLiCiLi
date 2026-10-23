// Package proxy 实现流式上游代理：图片、m3u8 与分片。
//
// 合同（由本包实现）：
//
//	func New(opts Options) http.Handler
//	func NewLive(opts Options) http.Handler
//
// 行为要求：
//   - GET/HEAD only；从 ?url= 取目标，也兼容 /api/proxy/<encoded> 路径形式；
//   - 默认要求已登录；匿名仅放行 Options.AnonymousImageHosts 精确后缀匹配的图片域；
//   - 目标必须过 upstream.Doer 的 SSRF 校验（DNS 固定、逐跳重定向复检）；
//   - 透传 Range/If-*，返回 200/206 与 content-type/content-range/accept-ranges/etag；
//   - 自动解压后不要透传 content-length/encoding；
//   - m3u8 文本重写为同源代理地址（用最终 URL 作为相对路径基准）；
//   - 分片/图片/直播流用 io.CopyBuffer 流式转发，不整体读入内存；
//   - 设置 Access-Control-Allow-Origin: *（Web 端直连受限时的兜底）；
//   - 未知/非法目标返回 JSON 错误，不泄露内网信息。
package proxy

import (
	"log/slog"
	"time"

	"cilicili/internal/auth"
	"cilicili/internal/upstream"
)

// Options 是代理 Handler 的构造参数。
type Options struct {
	Client upstream.Doer
	UA     string
	Auth   *auth.Manager
	Logger *slog.Logger

	// AnonymousImageHosts 允许未登录访问的图片域（精确后缀匹配，如 doubanio.com）。
	AnonymousImageHosts []string
	// DoubanReferer 为豆瓣图片追加 Referer。
	DoubanReferer bool
	// LiveAllowPrivate 放行内网直播源（部署者显式开启）。
	LiveAllowPrivate bool
	// HeaderTimeout 是等待上游响应头的超时；直播可放宽。
	HeaderTimeout time.Duration
}
