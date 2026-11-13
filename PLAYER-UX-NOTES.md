# 播放器与详情页 UX 改进说明

分支：`feat/player-quality-proxy-latency`

本次完成三项改动：

1. 播放器支持查看/切换码率（画质）档位；
2. 播放器支持「自动 / 强制直连 / 强制代理」并记住选择；
3. 详情页与一起看为片源/线路显示可核验的播放列表延迟。

---

## 改了哪些文件

### 前端

- `web/src/components/Player.tsx`
  - 在 ArtPlayer 设置面板新增「画质」：`自动` + hls.js 各档位（高度与码率，如 `1080P · 5.00 Mbps`）。
  - 自动档继续使用 ABR（`hls.currentLevel = -1`）；手动选档后固定该档，换集时自动回到自动档。
  - 新增「播放方式」：`自动（直连失败转代理）`、`强制直连`、`强制代理`。
  - 选择立即重新拉流并保留当前播放位置/播放状态；模式写入 `localStorage` 的
    `cilicili.player.proxyMode`，换集后仍生效。
  - 强制直连失败时会明确提示大概率是 CORS/防盗链限制，并建议切换「代理」或「自动」；
    不再静默等待重试。
  - 原有续播、进度上报、弹幕插件、错误恢复、自动代理回退、卡顿看门狗均保留。

- `web/src/pages/Detail.tsx`
  - 片源/线路按钮内显示延迟标签：`测速中…` / `123ms` / `超时` / `失败`。
  - 延迟在源列表渲染后延迟 150ms 后台探测，不阻塞首屏；悬停可看首字节与完成时间。
  - 片源延迟使用后端按 `sourceKey+vodId` 解析；线路延迟直接测当前详情里每条线路的首个 m3u8。

- `web/src/pages/WatchRoom.tsx`
  - 一起看房间内的片源列表（房主可见）显示与详情页一致的延迟标签。

- `web/src/lib/latency.ts`（新增）
  - 前端批量探测、短缓存（30s）、并发请求合并、延迟文案/颜色。

- `web/src/api/client.ts`、`web/src/api/types.ts`
  - 新增 `api.latency(items)` 与相关类型。

- `web/src/styles/global.css`
  - 片源/线路/一起看 chip 的延迟标签样式（绿/黄/红，适配选中态）。

### 后端

- `internal/latency/latency.go`（新增）
  - 轻量播放列表探测：只 GET 一次 m3u8，记录首字节（TTFB）和读取完成时间，单位 ms。
  - 单条默认 4s 超时、失败短缓存 10s、成功缓存 30s，避免重复探测拖慢页面。

- `internal/api/latency.go`（新增）
  - `POST /api/latency`（需登录）。
  - 支持两种条目：
    - `{ "key": "...", "url": "https://.../index.m3u8" }`
    - `{ "key": "...", "sourceKey": "xxx", "vodId": "123" }`（后端解析详情后取首个 m3u8）
  - 最多 20 条，4 并发，单条 5s 总预算；结果按请求顺序返回。
  - 返回字段：`key`、`ok`、`ttfbMs`、`ms`、`status`、`error`、`timedOut`。

- `internal/api/api.go`、`internal/server/server.go`
  - 注册依赖与路由 `POST /api/latency`；`/api/status` capabilities 增加 `latency`。

- `internal/server/server_test.go`、`internal/server/latency_test.go`、`internal/latency/latency_test.go`
  - 补充探测成功/HTTP 失败/超时/缓存与接口鉴权、批量结果测试。

- `cmd/cilici/main.go`
  - 初始化并注入延迟探测器。

---

## 本地运行

后端（默认 8080）：

```bash
cp .env.example .env    # 至少设置 PASSWORD
go run ./cmd/cilici
```

前端开发服务器（默认 5173，自动把 `/api` 反代到 8080）：

```bash
cd web
npm ci
npm run dev
```

生产构建：

```bash
cd web
npm run build
```

---

## 手动验证

### 1. 播放器码率/清晰度

1. 打开任意影片详情页，点击播放。
2. 点播放器右下角设置（齿轮）→「画质」。
3. 应看到「自动」以及当前流的各档位（例如 `720P · 3.00 Mbps`）。
4. 选择某档后应弹出「画质：…」提示；再点「自动」应恢复 ABR。
5. 手动选档后切换下一集，画质应回到「自动」。

> 只有源提供 master playlist（多码率）时才会有多档；单码率源可能只有一个档位。

### 2. 直连 / 代理

1. 同一播放器设置里打开「播放方式」，应看到：
   - 自动（直连失败转代理）
   - 强制直连
   - 强制代理
2. 选「强制代理」后应立即重新拉流，画面从原位置继续。
3. 选「强制直连」时，如果该源有 CORS/防盗链限制，播放器会提示
   “直连失败：可能是 CORS/防盗链限制，请在播放器设置中切换为「代理」或「自动」”。
4. 刷新页面或切换下一集，播放方式应保持上次选择（localStorage）。
5. 选回「自动」后仍应保留原来的直连失败自动回退代理能力。

### 3. 详情页 / 一起看延迟

1. 打开有多片源/多线路的详情页。
2. 片源与线路按钮会先正常出现，随后显示 `测速中…` → `xxxms` / `超时` / `失败`。
3. 鼠标悬停延迟标签可看到「首字节 xxxms · 完成 xxxms」。
4. 切换线路后，之前测得的线路延迟仍可对比。
5. 房主创建「一起看」房间后，房间侧边栏的片源 chip 也应显示同样的延迟标签。

### 自动化检查

```bash
go test ./...
cd web && npm run typecheck && npm run build
```

本次已执行并通过：`go test ./...`、`npm run typecheck`、`npm run build`。

---

## 已知限制

- 延迟是**服务端到播放列表 CDN** 的测量值，不等同于终端用户的真实网络延迟；
  但它是可复现、可核验的 m3u8 首字节/完成时间。
- 其他片源的延迟使用该片源推荐线路的首个 m3u8；不是当前正在看的集数。
- 探测结果有短缓存（后端成功 30s / 失败 10s，前端 30s），源状态刚变化时可能短暂显示旧值。
- 强制直连是否可用完全取决于源站 CORS、防盗链和网络环境；浏览器无法绕过这些限制。
- 画质档位依赖源站提供多码率 master playlist；单码率源没有有意义的 ABR 档位。
- 播放方式会持久化；画质选择按需求只对当前集有效，换集恢复自动。