# 实施计划（服务端优先）

> 目标：先把 API 做稳、做快，再上 Web 前端，最后做原生 App。
> 语言决策：**Go**（I/O 密集 + 大量胶水逻辑；单二进制部署；VPS 不通 Docker Hub）。
> 依据：[backend-rewrite-assessment.md](backend-rewrite-assessment.md)

## 里程碑

### M0 基建（进行中）
- [x] Go 1.27 工具链 + 模块 + 依赖（bbolt / x/text / x/sync）
- [x] 配置（env + SOURCES_FILE）、日志、鉴权、HTTP 骨架
- [x] 共享契约：model / upstream / cache / source / cms / filter / proxy
- [ ] 四个并行包：upstream、cms+filter、cache+health、proxy+m3u8
- [ ] `go test ./...` 全绿、`make build`、可启动

### M1 搜索核心 ✅
- [x] 搜索聚合：并发、分页、单源死线、硬截止
- [x] 流式 NDJSON + 软截止 `partial_done`
- [x] fast 模式：按健康度选最快 8 源（`SEARCH_FAST_SOURCES`）
- [x] 搜索级缓存 + 源级缓存 + 失败负缓存
- [x] 服务端源健康/熔断接入搜索 + 启动预热
- [x] 真实 18 源端到端验证（VPS 首源 121–427 ms；fast done 0.2–1.7 s）
- [x] 基准：对比 LibreTV（见 [BENCHMARKS.md](BENCHMARKS.md)）
- [x] 单元测试：fake upstream/cache/health + 集成测试

### M2 详情与代理 ✅
- [x] 详情：列表接口 → HTML 兜底 + 缓存
- [x] 代理：流式、Range、m3u8 重写、匿名图片白名单
- [x] 代理测试 + 本机/VPS 真实源验证（图片 / JSON / m3u8）
- [ ] 详情预取 / 首分片预取（播放起播进一步提速，后续）

### M2.5 客户端数据（Fladder 集成）✅
- [x] 详情：多线路 + 原始集名 + 稳定分集 ID（`ln:<线路名>` / `e:<原始集名>`）
- [x] 观看进度 `PUT /api/me/progress`、历史 `GET/DELETE /api/me/history`
- [x] 收藏 `GET/PUT/DELETE /api/me/favorites`
- [x] 独立 bbolt 持久化（userdata.db），默认用户；不放进缓存
- [ ] 多用户账号体系（后续）

### M3 画质（进行中）
- [ ] m3u8 master 解析（RESOLUTION / BANDWIDTH）
- [ ] 媒体列表 → 首分片前缀（MP4 moov / TS SPS）；AES-128 解密
- [ ] `POST /api/quality`（JSON + NDJSON 流）
- [ ] 缓存：成功 30 天 / 失败 2 分钟；详情时后台预热

### M4 直播
- [ ] M3U 解析、XMLTV（gzip）解析、EPG 查询
- [ ] `POST /api/live/probe`（manifest → segment → 吞吐采样；16 并发）
- [ ] `/api/live/playlist`、`/api/live/epg`、`/api/live/stream` 接入
- [ ] `LIVE_ALLOW_PRIVATE` 全链路生效

### M5 推荐 / 订阅
- [ ] 豆瓣（电影/剧集/标签分页）+ 缓存
- [ ] Bangumi 每日放送
- [ ] 60s API 榜单
- [ ] 源订阅：LibreTV / TVBOX / 接口目录；`DEFAULT_SUBSCRIPTIONS` 同步
- [ ] 源发布（可选）

### M6 Fladder Web 核心点播 ✅
- [x] 引入上游 v0.11.1，保留原有 Flutter 组件、动画、主题和播放器。
- [x] 客户端 API 边界适配现有 Go；密码登录、推荐→影片主页、同片搜索归并、主页切换片源、详情/选集、首页缓存。
- [x] 直连/代理线路、进度保存、继续观看、收藏。
- [x] 生产构建、适配测试、本地同源预览；真实 Go + 自制 HLS 浏览器验证。
- [ ] 源管理/订阅页、直播等后续能力按实际后端契约接入。
- [x] Web 页面重组：单一导航/分类、收藏本地筛选、紧凑选集与有效设置；完整中文字体随包提供。
- [x] 搜索/详情持久缓存及刷新路由恢复；播放前强制校验源地址。
- 运行、部署、上游版本及限制见 [Fladder Web](../clients/fladder/CILICILI.md)。

### M7 原生 App（后续）
- 沿用 Fladder Flutter 分支；当前只验收 Web，不另起原生 UI 重写。
- Android 等平台的 API、播放器与设备兼容性另行适配和验证。

## 性能目标（验收）

| 指标 | 目标 |
| --- | --- |
| 首源返回（19 源，VPS） | P50 ≤ 400 ms |
| partial_done（软截止） | ≤ 1.5 s |
| done（健康源） | ≤ 4 s；硬截止 ≤ 6 s |
| 缓存命中 | ≤ 20 ms |
| 死源请求 | 第二次搜索 0 请求（负缓存/熔断） |
| 详情 | P50 ≤ 1 s（缓存命中 ≤ 10 ms） |
| 播放起播 | 直连 CDN；起播 ≤ 1 s（源站允许时） |
| 内存 | RSS ≤ 100 MB |
| 冷启动 | ≤ 300 ms |

## 已知风险

- 采集站波动极大（实测 5/19 失效）；健康/负缓存是功能不是优化。
- VPS 出口 ~2.9 Mbps：视频代理不可作为主路径，Web/App 都应优先直连。
- 源站可能封服务器 IP：按 host 限并发，失败退避，不做全量高频测活。
- 合规：只聚合第三方公开接口；成人源建议剔除。

## 当前工作方式

1. Lead 维护契约与集成；teammate 按写作用域并行实现独立包。
2. 每个包独立测试（不联网），Lead 做端到端 + 真实源 benchmark。
3. 接口变更必须先改契约文件并通知所有依赖方。
