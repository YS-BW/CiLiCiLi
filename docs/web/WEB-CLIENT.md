# CiLiCiLi Web 客户端 —— 技术方案与产品原型

> 状态：讨论稿（Draft，未开工）
> 目标读者：作者本人
> 相关：[API 契约](../API.md) · [服务端规划](../PLAN.md)

## 0. 一句话结论

Web 端本质是一个**纯前端 SPA 调 `/api/*`**，后端契约已完备，不写服务端。
性能的 95% 不在框架，而在三处：**播放链路（起播/兜底/续播）**、**图片加载**、**流式搜索的增量渲染**。
因此本方案把播放器当独立模块认真做，框架选型从简。

## 1. 范围

### 第一版做（P0）
- 密码登录（httpOnly cookie）
- 首页推荐（豆瓣 + 榜单）+ 继续观看
- 流式多源搜索（一张影片一张卡）
- 详情 + 多线路/分集 + HLS 播放
- 观看进度 / 继续观看 / 收藏

### 暂不做（P1+）
- 源管理 / 订阅编辑 / 探活
- 直播 / EPG
- 下载 / 转码 / 外挂字幕服务
- 多用户账号体系（后端仍是默认用户）

## 2. 技术选型

| 层 | 选择 | 说明 |
| --- | --- | --- |
| 框架 | React 18 + TypeScript | 生态大、好维护；性能瓶颈不在此 |
| 构建 | Vite | 秒级启动，产物小 |
| 路由 | React Router | 标准 |
| 数据/缓存 | TanStack Query | 对齐后端 5min 搜索 / 10min 详情缓存 |
| 样式 | Tailwind CSS | 快速出海报网格，桌面优先 |
| 播放内核 | **hls.js + 原生 `<video>`** | 见 §4；可选套 ArtPlayer 出控制条 |
| 列表 | 虚拟滚动（TanStack Virtual） | 几百条搜索结果只留可见 DOM |
| 状态 | Zustand（少量全局：登录态、当前播放） | 轻 |

> **不选**：不引入重 UI 库（Antd/MUI）；不恢复旧的 Flutter Web。
> **备选**：若极致追性能可换 Svelte 5，但播放器/图片优化做法完全不变。

## 3. 部署与鉴权（必须先定，否则全盘返工）

- **必须同源部署**：Nginx 静态根 + `/api/` 反代 Go：
  ```nginx
  root /srv/cilicili-web;
  location / { try_files $uri $uri/ /index.html; }
  location /api/ {
      proxy_pass http://127.0.0.1:8080;
      proxy_http_version 1.1;
      proxy_set_header Host $host;
      proxy_set_header X-Forwarded-Proto $scheme;
      proxy_buffering off;
      proxy_read_timeout 3600s;
  }
  ```
- 鉴权用 httpOnly cookie `cilici_session`，前端 `fetch` 带 `credentials: 'include'`。
- **播放地址不带 token**：代理靠同源 cookie，故前后端不可分属两个域。
- 非流式接口超时：连接 8s / 读 20s（对齐 Android 客户端）。

## 4. 播放器模块（核心）

### 4.1 设计原则
1. **播放器实例只创建一次，换集只切换 HLS 源**，不重建 `<video>`（避免全屏丢失、黑屏有声竞态）。
2. **直连优先，失败自动切 `/api/proxy`**；代理地址由后端重写 m3u8 内部分片。
3. **错误恢复抽成纯函数决策模块**（`PlaybackRecovery`），可单测、不碰 hls 实例。
4. **续播只缓存 `episodeId`，不缓存 URL**；每次播放前重新拉 `/api/detail`。

### 4.2 hls.js 配置（借鉴 LibreTV-Next，按需删减广告过滤/缓存）
```ts
const hlsConfig: Partial<HlsConfig> = {
  debug: false,
  enableWorker: true,              // 解析放 worker，不卡主线程
  backBufferLength: 90,            // 保留 90s 已播，回拖不重下
  maxBufferLength: 30,             // 常态缓冲目标
  maxMaxBufferLength: 60,
  maxBufferSize: 30 * 1000 * 1000, // 30MB
  maxBufferHole: 0.5,              // 容忍小空洞，减少卡顿
  startLevel: -1,                  // 自动起播档位
  abrEwmaDefaultEstimate: 500_000, // 起播带宽预估，避免一上来糊
  fragLoadingMaxRetry: 6,
  fragLoadingRetryDelay: 1000,
  manifestLoadingMaxRetry: 3,
  manifestLoadingRetryDelay: 1000,
  appendErrorMaxRetry: 5,
};
```

### 4.3 错误恢复决策（`PlaybackRecovery`）
| 错误类型 | 处理 | 阈值后 |
| --- | --- | --- |
| 格式硬失败（manifestParsing/IncompatibleCodecs/levelEmpty） | 直接换源 | — |
| 媒体解码错误 | `recoverMediaError()`，第 2 次追加 `swapAudioCodec()` | >2 次换源 |
| 网络错误 | 指数退避 `800ms·2^(n-1)` 上限 8s | >3 次换源 |
| 其他致命 | — | 换源 |

- **3s 静默窗**：动作后 3s 内的 `FRAG_LOADED` 视为噪声不清零，防止"源已死"被误判为恢复。
- **`markHealthy`**：`FRAG_LOADED` / `MANIFEST_PARSED` 且过静默窗才清零计数。
- **播放中失败**：回调弹"换源"面板；**起播失败**：错误遮罩 + 提示换源。

### 4.4 直连 → 代理回退时序
```
直连致命网络错误
  → 前 2 次重试仍保留直连
  → attempt>=2 或 manifestLoadError
  → 切 /api/proxy?url=<encoded> 重建 hls（不重建 video）
```
提示浮层："直连失败，改用代理重试…"

### 4.5 续播恢复时序
`MANIFEST_PARSED` 常早于 `loadedmetadata`，duration 未就绪：
1. 监听 `loadedmetadata / durationchange / canplay`；
2. duration 有效 且 非无穷 才消费"已恢复"标记；
3. `saved > 10` 且 `saved < duration-2` 才跳转并提示"已从 xx 继续播放"；
4. 失败不阻塞播放，后续事件可再试。

### 4.6 进度上报
- `PUT /api/me/progress`，节流 5–10s + 暂停/退出时；单位**秒**。
- 带 `episodeId`（不是 URL）。

### 4.7 播放器组件骨架
```
<Player>
  props: mediaUrl, episodeKey, getRestorePosition, onRequestSwitchSource, onProgress
  state: hlsRef, recoveryRef, mediaHealthy, playbackStarted
  methods: setupHls(url, allowProxyFallback), loadEpisode(url), destroy()
</Player>
```

## 5. 图片与列表性能

- 封面优先用 `media[].cover`（后端本地 `/api/media/{id}/cover?v=hash`，immutable 30 天 + ETag）。
- `<img loading="lazy" decoding="async">` + 固定宽高比占位，避免布局抖动。
- 加载失败 → 本地占位图，不再猜第二张源图。
- 搜索结果/推荐用虚拟滚动，只渲染视口内卡片。
- 豆瓣图直连会被反爬，统一走 `/api/proxy`。

## 6. 流式搜索渲染

- `POST /api/search?stream=1`，用 `fetch` + `ReadableStream` 手写 NDJSON 逐行解析（不用 TanStack Query 包流）。
- `partial_done` 到达即结束"加载中"，后续 `source` 事件增量追加。
- 首源到达即用前端规则聚合出卡片（复用 Android 的 `aggregateItemsLocally` 逻辑），`done.media` 到达后以后端为准替换。
- 节点增删做成 keyed diff，避免整列表重渲染。

## 7. 页面结构（桌面优先）

| 路由 | 页面 | 主要接口 |
| --- | --- | --- |
| `/login` | 登录 | `POST /api/auth` |
| `/` | 首页推荐 + 继续观看 | `/api/douban` `/api/hot-list` `/api/me/history?unfinished=1` |
| `/search` | 流式搜索 | `POST /api/search?stream=1` |
| `/play/:sourceKey/:vodId` | 详情 + 播放 | `GET /api/detail` `PUT /api/me/progress` |
| `/favorites` | 收藏 | `/api/me/favorites` |

交互：推荐卡点击 → 用标题跳 `.search`，由用户选源（推荐 id 不是 vodId）。

## 8. 实施顺序

```
M1  脚手架 + 登录 + 同源 /api 打通
M2  流式搜索 + 结果卡片 + 虚拟滚动
M3  详情 + Player 模块（hls.js + 恢复 + 代理回退 + 续播）  ← 关键里程碑
M4  进度 / 继续观看 / 收藏
M5  首页推荐 + 打磨
```

验收线同后端：[PLAN.md §性能目标](../PLAN.md)。重点保证：直连起播 ≤1s（源站允许时）、代理兜底不黑屏、切集不卡死。

## 9. 待讨论问题

1. 播放器直接 `hls.js + 原生 video`，还是套 ArtPlayer 出控制条？（前者最轻，后者开箱有 UI）
2. 是否需要"选源测速"（LibreTV 有）？后端有 `/api/quality`，可做成详情页小测速。
3. 首页推荐是否保留"继续观看"在首屏（桌面宽屏是否两栏）。
4. 是否要做"自动连播下一集"（后端无此逻辑，纯前端）。
