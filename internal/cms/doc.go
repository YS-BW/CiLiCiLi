// Package cms 解析 Apple CMS（苹果CMS）采集站响应。
//
// 合同（由本包实现，搜索/详情/画质都会调用）：
//
//	func RequestHeaders(ua string) http.Header
//	func ParseSearchList(data []byte, src model.SourceConfig) (items []model.SearchResultItem, pageCount int, err error)
//	func ParseDetail(data []byte, src model.SourceConfig) (*model.VideoDetail, error)
//	func ParseDetailPageHTML(html []byte, src model.SourceConfig) *model.VideoDetail
//	func ExtractEpisodesFromPlayURL(playURL string) []string
//	func ExtractM3U8FromText(text string) []string
//
// 语义必须与 LibreTV 对齐：
//   - list 为 null 视为空结果而非格式错误；非数组才报错；
//   - vod_play_url 形如 "线路1$$$线路2"，每线路 "集1$url#集2$url"；
//     优先取包含 .m3u8 的线路，均无则取第一条；
//   - 详情页 HTML 兜底：匹配 $https...m3u8 与日期哈希路径，标题取 <h1>，简介取 sketch；
//   - 所有字段做 trim；空串不落 JSON。
package cms

// 这里只放包级常量，函数实现见 parse.go。
