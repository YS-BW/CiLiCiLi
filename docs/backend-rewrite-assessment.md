# LibreTV 后端重写评估：Go / Rust 之前，先把「慢」拆开看

> 评估对象：LibreTV **v2.21.0**（commit `b08c824`，Next.js 15 + TypeScript）
> 实测环境：本机 Mac（Docker 容器 v2.19.2，走代理出网）+ 腾讯云上海 CVM（S5.LARGE4，直连）
> 日期：2026-10-23

---

## 0. 结论先行

1. **服务端不是瓶颈。** 实测 Next.js 处理一个搜索请求的框架开销是 **1.4–5.9 ms**（缓存命中时整个响应 5–8 ms），而搜索总耗时是 **0.3–10 s**。换成 Go/Rust，最多省掉这 5 ms 和一点内存，**不会让用户感觉变快**。
2. **慢的来源是 19 个第三方采集站 + 本机出口带宽。** VPS 到 19 个源：最快 0.35–0.48 s，中位数 1.4–2.4 s，P90 2.3–3.4 s；当前有 5/19 个源失败（3 个 reset、1 个 8 s 连接超时、1 个会挂到 20 s）。
3. **真正能快的是 5 个架构动作，不是语言**：播放直连、软截止 + 增量流、服务端全局源健康 + 负缓存、持久缓存 + SWR、连接池预热/域名缓存。这些在 Go/Rust 里做更自然，但**用 TS 也能做**。
4. **语言选 Go。** 这是 I/O 密集型 + 大量胶水逻辑的聚合/代理服务，Go 的开发速度、流式转发、连接池控制、单二进制部署都更合适；Rust 只有在「你想学 Rust / 追求极限 p99 / 内存 <20MB」时才更优。VPS 上 Docker Hub 不通这件事，单二进制 `scp + systemd` 就能解决。
5. **值得重写，但请把目标写成「可治理的缓存/健康/连接层 + 原生 App 直连」，而不是「换个语言所以更快」。** 否则重写完成后，用户感知的搜索耗时几乎不会变。

---

## 1. 项目现状（读到的范围）

### 1.1 规模与结构

| 项 | 数值 |
| --- | --- |
| 版本 / 提交 | v2.21.0 / `b08c824`（2026-10-23） |
| 提交总数 | 894 |
| 源码 | `src/` 约 1.3 MB；服务端相关（`src/app/api` 1,715 行 + 服务端 lib 4,319 行，均不含测试）**约 6.0 k 行** |
| 核心 VOD 后端 | `ssrf` / `fetch-utils` / `cms-parser` / `proxy-handlers` / `auth` / `search` / `detail` = **1,559 行**，与交接文档的 1,550 行一致 |
| 完整服务端 | 再加画质探测、直播（M3U/EPG/测活/长连接代理）、推荐、订阅、发布 = 6.0 k 行 |
| 部署 | Next.js standalone → Node 22 Alpine，镜像约 350 MB，空闲约 47 MiB |
| 许可证 | AGPL-3.0-or-later（重写时注意 clean-room） |

### 1.2 API 面（20 个 route 文件 / 16 组接口，重写要对齐的行为）

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| GET | `/api/status` | 版本、鉴权状态、预置源 |
| POST/GET/DELETE | `/api/auth` | 登录（HMAC cookie，90 天）、状态、登出 |
| POST | `/api/search`（`?stream=1`） | 19 源并发聚合搜索，NDJSON 逐源推送；60 s 内存缓存 |
| GET | `/api/detail` | 详情：`ac=videolist&ids=` 优先，失败回退爬详情页 HTML |
| POST | `/api/quality`（`?stream=1`） | 画质探测：m3u8 档位 → 分片 SPS/MP4 moov；3 并发，12 s/条 |
| GET | `/api/proxy?url=` / `/api/proxy/<encoded>` | 流式代理（图片/m3u8 重写/分片），Range 透传 |
| POST | `/api/source/test` | 单源探活 |
| GET | `/api/source-list?url=` | 订阅拉取（LibreTV / TVBOX / 目录三种格式） |
| POST | `/api/publish` | 发布源列表到第三方粘贴板 |
| GET | `/api/douban`、`/api/bangumi/calendar`、`/api/hot-list` | 首页推荐（各 10 min–1 h 缓存） |
| GET | `/api/live/playlist`、`/api/live/epg` | 直播列表（10 min 缓存）、XMLTV 节目单（6 h 缓存） |
| POST | `/api/live/probe` | 直播分片级测活（16 并发，10 s/条，30 min/2 min 缓存） |
| GET | `/api/live/stream?url=` | 直播长连接代理（FLV/HLS，拿到响应头后不限 body 时长） |

### 1.3 关键机制（重写时不能丢的语义）

- **搜索**：每源先取第 1 页读 `pagecount`，后续页并行抓（默认上限 5 页）；单源总死线 10 s；单页 8 s；失败/超时只影响该源。
- **缓存**：搜索 60 s、详情 60 s、画质 30 天（失败 2 min）、直播列表 10 min、EPG 6 h、推荐 10–60 min；**全部在进程内存，重启即失效**。
- **源健康**：客户端 localStorage 记录，连续 2 次失败后按 30 min → 24 h → 长期停用；**只对本设备生效**，新设备/新用户仍需先踩一遍死源。
- **SSRF**：字面量校验 + DNS 解析校验，重定向逐跳复检；proxy 有图片域白名单。
- **代理**：`fetch` 自动解压后转发，m3u8 文本重写为同源代理地址；对上游 `fetch failed` 重试 1 次。
- **鉴权**：httpOnly cookie + HMAC，90 天；登录限速 10 次/10 min（内存）。
- **流式**：搜索/画质用 NDJSON 逐条推送，首字节即 enqueue；`X-Accel-Buffering: no`。

### 1.4 线上部署现状

- 腾讯云上海 CVM：**4 vCPU**（Xeon 8255C）/ **3.6 GB** / 40 GB 磁盘，nginx 1.29.8 反代 `127.0.0.1:8080`。
- **当前 nginx 返回 502，LibreTV 容器未运行**，本机也没有留下该镜像；所以 VPS 侧只有「裸源站」实测，端到端数字来自本机容器。
- VPS **连不上 Docker Hub**，GHCR 会停滞，只能走 `ghcr.nju.edu.cn` 镜像（交接文档已记录）。
- **实测 VPS 出口带宽约 2.86 Mbps**（32 MiB / 93.7 s，SSH 传输），与交接文档的 2.7 Mbps 基本一致。

---

## 2. 实测数据

### 2.1 方法

- **VPS → 19 源**：Python 线程池 × 19 并发，每个源用独立 `curl`（冷连接，带浏览器 UA），`ac=videolist&wd=庆余年&pg=1`，3 轮；记录 DNS/TCP/TLS/TTFB/总耗时。
- **本机 App 端到端**：登录本地 LibreTV 容器（v2.19.2，走 Clash 代理出网），POST `/api/search?stream=1`，逐行记时间戳；同时记录服务端自报的 `ms`。单源、全源、缓存命中、非流式分别测。
- **VPS 冷/热连接**：同一批源，第 1 轮新建连接，第 2/3 轮复用连接并改用不同关键词（避免源站缓存混淆），共 3 轮。
- 原始数据在 `docs/bench/`：`vps_raw.json`、`warm3.json`、`app_local_cold.json`。

### 2.2 VPS → 19 源（3 轮，冷连接）

| 轮次 | 成功 | 最快源 | 中位源 | P90 | 最慢（成功） | 批次墙钟 |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | 14/19 | 477 ms | 1665 ms | 2330 ms | 3412 ms | 20.0 s（被挂死源拖到 curl 上限） |
| 2 | 14/19 | 353 ms | 2442 ms | 3354 ms | 5214 ms | 20.0 s |
| 3 | 14/19 | 484 ms | 1375 ms | 3250 ms | 4228 ms | 20.0 s |

第 1 轮逐源：

| 源 | 总耗时 | TTFB | 备注 |
| --- | --- | --- | --- |
| 360采集 | 477 ms | 477 ms | 最快 |
| 影视非凡直连源 | 617 ms | 519 ms | DNS 265 ms |
| 虎牙采集 | 1109 ms | 759 ms | TLS 279 ms |
| 影视电影天堂源 | 1201 ms | 345 ms | 首字节后下载慢 |
| 光速资源 | 1227 ms | 972 ms | |
| 量子资源 | 1531 ms | 1121 ms | |
| 暴风资源 | 1613 ms | 1339 ms | |
| 非凡资源·ffzy5 | 1716 ms | 750 ms | DNS 384 ms |
| 火狐采集 | 1749 ms | 1154 ms | |
| 非凡资源·cj | 2007 ms | 755 ms | |
| 红牛资源 | 2041 ms | 1720 ms | |
| 百度资源 | 2330 ms | 2330 ms | |
| 影视极速直连源 | 2886 ms | 1039 ms | |
| 影视建安源 | 3412 ms | 128 ms | 返回 881 KB，下载慢 |
| 如意资源 / 尤酷 / ikun | 69–331 ms | — | **连接被重置**（快速失败） |
| 非凡资源·ffzy1 | 8001 ms | — | **连接超时** |
| ckzy资源 | 20002 ms | — | **挂死**；App 会被 10 s 源死线截断（另一次测试 2 s 返回，**不稳定**） |

> 注：5 个失效源里，3 个是快速 reset（几百毫秒），1 个 8 s 连接超时，1 个会挂死。App 的 10 s 单源死线决定了**最坏情况下 `done` 事件要等 10 s**。交接文档里「4 个源瞬时 fetch failed、每次白等 1.1–1.4 s」是同一现象。

### 2.3 VPS 冷/热连接（不同关键词，3 轮）

| 轮次 | 成功 | 最快 | 中位 | 最慢 |
| --- | --- | --- | --- | --- |
| 冷连接 | 15/19 | 575 ms | 2143 ms | 5312 ms |
| 热连接 + 新关键词 | 15/19 | 424 ms | 1555 ms | 4045 ms |
| 热连接 + 新关键词 | 15/19 | 512 ms | 1437 ms | 3720 ms |

**解读**：连接复用/预热有收益（中位数约 −30%），但**个体方差远大于收益**（同一源在不同轮次 0.4–4 s 波动）。这也是为什么「换语言」不是主杠杆：服务端能省的是毫秒，源站波动是秒。

### 2.4 App 端到端（本机容器，冷缓存）

| 场景 | 首源返回 | `done` | 备注 |
| --- | --- | --- | --- |
| 19 源流式搜索 · 繁花 | 926 ms | 8028 ms | 某源 8 s |
| 19 源流式搜索 · 漫长的季节 | 1957 ms | 5298 ms | |
| 19 源流式搜索 · 隐入尘烟 | 315 ms | 3420 ms | |
| 19 源非流式 · 第二十条 | — | 3055 ms | 等全部源 |
| 19 源非流式 · 热辣滚烫 | — | 3114 ms | |
| 19 源非流式 · 飞驰人生2 | — | 4282 ms | |
| 缓存命中（同一关键词，60 s 内） | — | **5.4–8.1 ms** | 内存缓存 |
| 单源搜索（冷） | 616–891 ms | 同左 | 总耗时 − 服务端自报 = **1.3–5.9 ms** |
| `/api/status`（无上游） | — | **1.4 ms（p50）** | Next.js 路由本身 |

**结论：语言能影响的那部分（框架 + JSON + 鉴权 + 解析）只有毫秒级；用户体感的那几秒全在上游。**

### 2.5 VPS 出口带宽

- 32 MiB 经 SSH 传输耗时 93.7 s → **≈ 2.86 Mbps**。
- 1080p HLS 通常 3–8 Mbps。**经 VPS 转发的播放必然缓冲**，这不是换后端语言能解决的；原生 App 应优先直连 CDN（交接文档实测 10 个真实源均为 HTTPS + `Access-Control-Allow-Origin: *`；原生 App 不需要 CORS，直连更简单）。

---

## 3. 为什么换语言不会让搜索明显变快

搜索耗时的构成（VPS 实测）：

```
0 ms ──────────────────────────────────────────────── 3400 ms
│ 服务端 1–6 ms │ DNS 1–380 ms │ TCP 170–560 ms │ TLS 60–330 ms │ TTFB 130–2300 ms │ body 0–3200 ms │
                └────────────── 19 个源并行，取最慢的那条关键路径 ──────────────────┘
```

- 19 路并发下，**关键路径 = 最慢的健康源**，而不是 19 个源耗时之和。
- Go/Rust 能优化的是最左边那 1–6 ms；即使做到 0 ms，相对 3.4 s 也只有 **0.2%**。
- 唯一有意义的语言相关收益是**长连接池 + DNS 缓存 + 预热**，能让首源少一次 TCP/TLS（约 0.3–0.5 s）；但这是「你怎么维护连接」的架构问题，Go 只是更容易做，不是 Go 本身更快。
- 缓存命中时已经只要 5 ms——换语言最多再省 3–5 ms，用户无感。
- 播放走 VPS 时，瓶颈是 2.86 Mbps 出口；换语言对带宽零影响。

**所以：如果重写后仍然「19 源全选 + 10 s 死线 + 60 s 内存缓存」，体感会和现在一模一样。**

---

## 4. 真正的性能杠杆（按 ROI 排序）

| # | 动作 | 预期效果 | 成本 |
| --- | --- | --- | --- |
| 1 | **播放直连**（App 解析 m3u8 后直连 CDN；VPS 只做搜索/详情，不转发视频） | 播放从「必然缓冲」变成「取决于用户网络」；VPS 出口不再是天花板 | App 端改造；后端保留可选代理 |
| 2 | **软截止 + 增量流**：1.2–1.5 s 发 `partial_done`，慢源结果继续以 `source` 事件补，6 s 硬截止 | 首屏完成时间 **3.4–10 s → 1.2–1.5 s**；用户不需要等死源 | 后端协议 + App 渲染策略 |
| 3 | **服务端全局源健康 + 负缓存**：EWMA 延迟、连续失败、30 min/24 h/长期熔断；失败源 60 s 内直接跳过（不发请求） | 新设备/新用户不再踩 5 个死源；最坏 `done` 从 10 s 降到「健康源上限」≈3.4 s；后续查询更低 | 中；SQLite 持久化 |
| 4 | **持久缓存 + SWR + 热门词预热**：SQLite/内存两级；stale 先返回、后台刷新；启动预热 Top 100 关键词 | 重复/热门搜索 **1.5–4 s → 5 ms 级**；重启不丢缓存 | 中 |
| 5 | **连接池预热 + DNS 缓存**：启动时对 Top 源建连；`IdleConnTimeout 90 s`；DNS 5 min 缓存 + 解析一次后锁 IP | 首源 **0.3–0.5 s → 0.1–0.3 s**（估计值；实测热连接中位 −28%，但方差大） | 低 |
| 6 | **画质探测**：详情返回时后台预热；结果 30 天持久化；按 host 限流下适度提高并发 | 来源面板徽章从「十几秒慢慢亮」变成「打开就有」 | 中 |
| 7 | **传输优化**：gzip/br、ETag/304、nginx 对 GET 接口做 1–5 s 微缓存 | 冷请求省 100–300 ms（body 大的源更明显） | 低 |
| 8 | **冷启动**：Go 静态二进制 <100 ms 起来；Next 容器要几秒 | 部署/重启体验；对日常搜索无感 | 低 |

**不要做的事**：加 CPU/内存（现在 4C/3.6G 完全够）、把并发从 19 提到 50（只会更快触发源站限流）、单纯换语言。

---

## 5. Go vs Rust：本项目选 Go

### 5.1 对比

| 维度 | Go | Rust |
| --- | --- | --- |
| 开发速度（胶水/解析/代理） | ★★★★★ 简单直接 | ★★★ 类型/生命周期摩擦更多 |
| 运行性能 | 足够（I/O 密集） | 略优，但这里用不上 |
| 内存 | 20–40 MB | 10–20 MB（3.6 GB 机器无所谓） |
| 并发模型 | goroutine + channel，fan-out/流式最顺手 | tokio 任务，同样能做但心智负担高 |
| HTTP 客户端/连接池控制 | `net/http.Transport` 粒度细，天然支持 | `reqwest` 也强，配置略绕 |
| 流式代理 | `io.Copy` / `ReverseProxy` 一行 | hyper/axum 需要拼装 |
| SSRF 防护（DNS 锁 IP） | 自定义 `DialContext` 很自然 | 也可，但更啰嗦 |
| 单二进制部署 | ★★★★★ 静态交叉编译 | ★★★★★ 同样优秀 |
| 编译/迭代 | 快 | 慢（尤其泛型/宏） |
| 观测 | pprof + Prometheus 生态成熟 | 同样成熟 |
| 招聘/维护 | 更容易 | 更难 |

### 5.2 建议

- **默认 Go**：`net/http` + `chi`/`gin`（或纯 stdlib）+ `modernc.org/sqlite` + `golang.org/x/sync/singleflight` + `golang.org/x/text/collate`（中文排序）。
- **什么情况选 Rust**：你个人想认真学 Rust；或者未来要做单机数万并发 relay、要求 p99 <10 ms、内存 <20 MB。就当前 19 源聚合 + 单人/小团队使用，Rust 的额外收益抵不过 1.5–2× 的开发时间。
- **无论选哪个**：第 4 节的架构动作才是重点；语言只影响实现难易和运维形态。

---

## 6. 推荐架构（Go 版）

```
Android App ──HTTPS + Bearer──> nginx (TLS, 自签/域名)
   │                              │
   │ 直连 CDN 播视频                └──> cilici-core (Go, 127.0.0.1:8080)
   │                                       ├─ source registry + health (SQLite WAL)
   │                                       ├─ cache: LRU + SQLite (TTL/SWR/singleflight)
   │                                       ├─ upstream client (per-host pool, DNS pin, SSRF)
   │                                       ├─ aggregator (fan-out, NDJSON, soft deadline)
   │                                       └─ proxy (图片/m3u8；视频代理默认关闭)
   └── 可选：/api/proxy 仅用于图片/防盗链场景
```

### 6.1 模块划分（建议目录）

```
cmd/core/main.go
internal/config       // env + yaml
internal/httpapi      // 路由、中间件、鉴权、限流
internal/source       // 源注册、订阅同步、健康/熔断
internal/search       // 聚合、分页、过滤、排序、NDJSON
internal/cms          // Apple CMS JSON/HTML 解析
internal/detail       // 详情 + 回退
internal/quality      // m3u8/分片/编码解析
internal/proxy        // 流式转发、m3u8 重写
internal/live         // m3u/xmltv/probe/stream
internal/upstream     // HTTP client、DNS 缓存、SSRF、重试
internal/cache        // LRU + SQLite + SWR
internal/recommend    // douban/bangumi/hot-list
```

### 6.2 API 设计（与旧版共存或替换）

- **认证**：`POST /api/auth`（密码换 Bearer token，长期有效、可多设备）；App 存 Android Keystore。旧的 cookie 模式可作为兼容层保留。
- **搜索**：`POST /api/search`，body `{wd, sources?, filterAdult?}`，`?stream=1` 返回 NDJSON：
  - `{"type":"source", sourceKey, ok, list, ms, timedOut}`
  - `{"type":"partial_done", list, failures, soft:true}` ← 新增，1.2–1.5 s
  - `{"type":"source", ...}`（慢源后续补）
  - `{"type":"done", list, failures}` ← 硬截止/全部完成
- **其余**：`/api/status`、`/api/detail`、`/api/quality`、`/api/proxy`、`/api/sources`、`/api/sources/test`、`/api/live/*`、`/api/recommend/*`。
- **兼容**：若还要保留 Web 前端，旧路径全部保留；如果只服务原生 App，可以直接用 `/v1/*` 新契约，少背历史包袱。

### 6.3 超时矩阵（建议）

| 层级 | 值 |
| --- | --- |
| connect | 3 s |
| TLS handshake | 3 s |
| 响应头 | 5 s |
| 单页 body | 5 s |
| 单源总死线 | 6 s（当前 10 s） |
| 搜索硬截止 | 6 s |
| 搜索软截止 | 1.2–1.5 s |
| proxy 响应头 | 8 s |
| 直播响应头 | 15 s，body 不限 |
| 画质单条 | 8 s（当前 12 s） |

### 6.4 缓存与健康

- **两级缓存**：进程内 LRU（热）→ SQLite（冷，WAL，TTL + `stale_while_revalidate`）。
- **键归一化**：关键词 NFKC + 小写 + 去标点；源集合按 URL 排序后 hash；包含 `filterAdult`。
- **健康数据**：每源 `ewma_ms / fail_streak / last_ok / open_until / level`，持久化；所有客户端共享。
- **熔断**：连续 2 次失败 → 30 min；再犯 → 24 h；再犯 → 长期（管理接口恢复）。成功一次降一级。
- **负缓存**：失败源 60 s 内不再发请求；`timedOut` 与真正失败分开记。
- **预热**：启动时并发探测 Top 源并建连；可选每 10 min 后台刷新健康。

### 6.5 SSRF / 安全

- 解析一次 DNS → 校验全部 IP（拒绝私网/回环/链路本地/CGNAT/保留段）→ **用解析到的 IP 建连**（`DialContext` + `tls.Config.ServerName`），避免 DNS 重绑定。
- 重定向手动逐跳（最多 5 跳），每跳复检。
- 代理：图片域白名单（精确后缀）；其余需鉴权；方法只允许 GET/HEAD；剥离 hop-by-hop 头；限制响应体大小。
- 限速：按 `X-Real-IP`（nginx 设置）做令牌桶；登录 10 次/10 min。
- 鉴权：`PASSWORD` 只用于换 token；token HMAC + 过期；恒定时间比较。

### 6.6 部署（绕开 Docker Hub）

```bash
# 本机交叉编译
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o cilici-core ./cmd/core
scp cilici-core tencent-vps:/usr/local/bin/
# VPS
systemctl restart cilici-core   # systemd: Restart=always, EnvironmentFile, ProtectSystem
```

- SQLite：`/var/lib/cilici-core/cache.db`；日志走 journald（JSON）。
- nginx：沿用现有 443 配置，`location /api/ { proxy_pass http://127.0.0.1:8080; proxy_http_version 1.1; proxy_buffering off; proxy_read_timeout 3600s; }`；加 `X-Real-IP`。
- 自签证书可继续用；App 端 pin 证书（注意证书轮换）。
- 监控：`/metrics`（Prometheus）+ `/api/health`；至少记录每源 p50/p95、缓存命中率、熔断状态。

---

## 7. 范围与里程碑

### 7.1 建议先做「App 需要的 VOD 核心」

| 阶段 | 内容 | 估算 |
| --- | --- | --- |
| M0 | 契约测试集：从线上抓 19 源 × 若干关键词的响应做 golden；搭 benchmark harness（本仓库已有一版） | 1–2 天 |
| M1 | 配置/鉴权/status、源注册、上游 client（SSRF/重定向/超时/重试）、搜索聚合 + NDJSON + 聚合过滤排序、详情、两级缓存、健康熔断 | 5–7 天 |
| M2 | 图片/m3u8 代理、画质探测（可后置）、软截止 + partial_done | 3–5 天 |
| M3 | 直播（playlist/EPG/probe/stream）——**只有 App 需要才做** | 4–5 天 |
| M4 | 推荐（豆瓣/Bangumi/榜单）、订阅同步、发布（可选） | 2–3 天 |
| M5 | 压测、灰度、切换 nginx、保留回滚 | 2 天 |

**一人 + AI 辅助，VOD 核心约 1–1.5 周；全量对齐约 3 周。Rust 按 1.5–2× 估。**

### 7.2 验收指标（用数据验收，而不是「感觉快了」）

- 首源返回：VPS 上 p50 ≤ 400 ms（预热后 ≤ 300 ms）。
- 首屏软完成：≤ 1.5 s（19 源全选、含 5 个死源的情况）。
- 硬完成：≤ 6 s；死源不参与后 ≤ 4 s。
- 缓存命中：≤ 20 ms。
- 第二次搜索：死源 0 请求（负缓存生效）。
- 播放：App 直连 CDN；代理路径仅作为 fallback。
- 资源：RSS ≤ 100 MB，冷启动 ≤ 300 ms。

### 7.3 测试

- `go test -race ./...`；对 19 源真实响应做 golden 测试（解析/过滤/排序）。
- `httptest` 覆盖：SSRF（含重定向到内网、DNS 重绑定）、超时/取消、NDJSON 断流、Range、m3u8 重写。
- 小流量对比：新旧后端同关键词并行跑 1 天，比较结果集差异和耗时分布。

---

## 8. 风险与合规

- **源站高度不稳定**：本次 24 h 内 19 源有 5 个失效；任何「提速」都必须建立在健康治理上，否则只是把慢源换个地方等。
- **IP 被封**：并发/重试要克制；对同一 host 限 3–5 并发；失败源负缓存；不要 19 源 × 多页无限放大。
- **法律/合规**：LibreTV 只聚合第三方接口；重写不改变内容来源的合规风险。`ckzy` 是成人聚合站，建议剔除。
- **AGPL**：LibreTV 是 AGPL-3.0。按你说的「不参考里面的」做 clean-room（只看行为/协议，不复制代码/注释），新项目可自选许可证；如果直接移植代码则需遵守 AGPL。
- **VPS 带宽**：2.86 Mbps 出口决定了「视频代理」不可行；若产品必须代理，需升级带宽或换靠近用户的节点。
- **自签证书**：App pin 证书要注意轮换；正式产品建议域名 + Let's Encrypt。
- **工具链**：本机与 VPS 都没有 Go/Rust；Go 用官方 tarball 或 `brew install go`，VPS 交叉编译后只放二进制即可。

---

## 9. 需要你确认的 4 件事

1. **「慢」具体指哪一段？** 首屏出结果 / 全部结果出完 / 播放起播 / 画质徽章 / 网页加载。不同答案对应不同优先级；如果是网页加载，原生 App 已经解决，后端重写不影响。
2. **重写范围**：只做 VOD（搜索/详情/代理/画质），还是把直播、推荐、订阅、发布也一起重写？
3. **Go 还是 Rust？** 我的建议是 Go；如果你更想用 Rust 也可以，架构不变，工期按 1.5–2× 估。
4. **是否保留 Web 前端兼容？** 只服务原生 App 的话，可以直接用 `/v1` 新契约，砍掉 cookie/发布/Web 兼容层，工期更短。

---

## 附录：原始数据

- `docs/bench/vps_raw.json` — VPS 19 源 × 3 轮冷连接（含逐源 DNS/TCP/TLS/TTFB/总耗时）
- `docs/bench/warm3.json` — VPS 冷连接 vs 热连接 × 3 轮（不同关键词）
- `docs/bench/app_local_cold.json` — 本机 App 端到端（流式/非流式/单源/缓存/status）

> 边界说明：VPS 上 LibreTV 容器当前未运行（nginx 502），所以「端到端 App」数据来自本机容器（走代理出网，19 源全通）；「源站耗时/带宽」来自 VPS 直连。前者用于测服务端开销，后者用于测真实上游，两者结论一致。
