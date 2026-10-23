// Package filter 实现搜索结果的成人内容过滤、关键词相关性过滤与排序。
//
// 合同（由本包实现，search 调用）：
//
//	func Adult(items []model.SearchResultItem, enabled bool) []model.SearchResultItem
//	func Relevant(items []model.SearchResultItem, wd string) []model.SearchResultItem
//	func Sort(items []model.SearchResultItem, wd string)
//	func NormalizeTitle(s string) string
//
// 语义必须与 LibreTV 对齐：
//   - 成人过滤按 type_name 关键词（与源站分类名一致）；
//   - 相关性：忽略标点/空白/大小写；多词要求每个词都命中；支持“庆余年2”“第二季”季数别名；
//   - 排序：精确命中（忽略标点）优先，其次按中文名称，再按源名；
//   - 空关键词/纯标点关键词时不过滤。
package filter

// 这里只放包级常量，函数实现见 filter.go。
