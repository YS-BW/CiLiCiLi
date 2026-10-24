// Package providers 封装第三方元数据源（Bangumi）。
//
// 约定：
//   - 只返回候选，不直接修改媒体库；
//   - 所有请求走 upstream.Doer（SSRF、超时、重定向由它统一处理）；
//   - 未配置 key 时 Enabled=false，直接跳过。
package providers
