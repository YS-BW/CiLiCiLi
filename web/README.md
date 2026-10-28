# CiLiCiLi Web

CiLiCiLi 的 Web 客户端：React 18 + TypeScript + Vite，播放内核 hls.js + 原生 `<video>`。

设计与取舍见 [docs/web/WEB-CLIENT.md](../../docs/web/WEB-CLIENT.md)；产品原型见 [docs/web/prototype-netflix.html](../../docs/web/prototype-netflix.html)。

## 本地开发

```bash
# 1. 先起后端（8080）：仓库根目录 make run 或 docker compose up -d cilicili
# 2. 起前端 dev server（Vite 自动把 /api 反代到 127.0.0.1:8080）
cd web
npm install
npm run dev          # http://localhost:5173
```

可选：后端在别的端口时 `VITE_API_TARGET=http://127.0.0.1:18080 npm run dev`。

## 生产构建 + 容器

前端产物 `dist/` 由 Vite 构建；容器只用 Nginx 托管并同源反代 `/api/`。

```bash
cd web
npm run build                       # 生成 dist/

# 方式一（推荐，本机无外网也可）：用预构建产物打镜像
docker build -t cilicili-web:latest .

# 方式二（构建机有 npm 源时）：镜像内自包含构建
docker build -f Dockerfile.full -t cilicili-web:latest .
```

或在仓库根目录一起起：

```bash
docker compose up -d --build        # 后端 :8080 + Web :3000
# 打开 http://localhost:3000
```

容器通过 `BACKEND_UPSTREAM`（默认 `http://backend:8080`）指定后端；compose 里设为
`http://cilicili:8080`（后端服务名）。**必须同源**——cookie 鉴权和图片代理都依赖它。

## 结构

```
src/
  api/client.ts        // 唯一 API 边界：auth / search(流式) / detail / me / 聚合
  api/types.ts         // 与 docs/API.md 对齐的类型
  lib/playback-recovery.ts  // HLS 错误恢复决策器（独立可测）
  lib/auth.tsx         // 登录态 context
  components/Player.tsx     // 播放器：hls.js 配置 + 直连→代理回退 + 续播 + 进度上报
  components/Nav.tsx / PosterCard.tsx
  pages/               // Login / Home / Search / Detail / Favorites
  styles/global.css    // Netflix 风深色主题
```

## 关键实现

- **流式搜索**：`/api/search?stream=1` 用 `fetch` + `ReadableStream` 手写 NDJSON 解析，
  首源到达即前端聚合出卡片，`partial_done` 结束加载态，`done` 用后端 `media[]` 定稿。
- **播放器**：hls.js 调参（`enableWorker`、`maxBufferLength=30` 等）；致命网络错误前 2 次保留直连，
  到阈值/清单错误自动切 `/api/proxy`；`PlaybackRecovery` 独立决策（网络退避 3 次 / 媒体恢复 2 次 / 硬失败换源）；
  续播等 `loadedmetadata` 后再定位；进度 5s 节流上报，单位秒。
- **图片**：封面/横版图经 `/api/proxy`（豆瓣图必须），`loading=lazy decoding=async`。
