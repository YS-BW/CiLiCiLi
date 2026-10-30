# CiLiCiLi

多源聚合影视客户端：**Go 服务端 + React Web 前端**，自建自用。

- 服务端：多源并发聚合搜索、详情解析、流式代理（图片/m3u8/分片/直播）、媒体聚合与 TMDB 刮削、
  两级持久缓存、源健康熔断与自动发现、SSRF 防护。
- Web 前端：Netflix 风格布局、流式增量搜索、ArtPlayer + hls.js 播放器、封面统一接口。
- Android APK 外壳（WebView 容器）**不在本仓库内**。

- API 文档：[docs/API.md](docs/API.md)
- 前端说明：[docs/web/](docs/web/)
- 设计/评估：[docs/backend-rewrite-assessment.md](docs/backend-rewrite-assessment.md)

- API 文档：[docs/API.md](docs/API.md)
- 设计/评估：[docs/backend-rewrite-assessment.md](docs/backend-rewrite-assessment.md)

## 特性

- 多源并发聚合搜索（NDJSON 流式 + 软截止 `partial_done`），失败源单独上报
- 两级缓存（内存 LRU + bbolt 持久化，TTL/SWR）
- 服务端全局源健康 / 熔断 / 负缓存
- 详情解析（列表接口优先，详情页 HTML 兜底）
- 结构化播放列表：多线路 + 原始集名 + 与 URL 解耦的分集 ID
- 观看进度 / 历史 / 收藏（独立持久化，默认用户）
- 媒体聚合（一张影片一张卡）+ 本地封面缓存 + TMDB 元数据候选
- 源管理（SQLite 持久化、增删改/启停/排序、热生效）+ 源订阅
- 服务端定期静默探活（默认 12h）+ 每 7 天自动发现新源（只收录存活源）
- SSRF 防护（DNS 固定、逐跳重定向复检）
- Bearer token + cookie 双鉴权
- 流式代理（图片 / m3u8 / 分片 / 直播）+ m3u8 重写
- 首页推荐（豆瓣 / 影视榜单）、画质探测、直播接口

## 本地运行

```bash
cp .env.example .env    # 至少改 PASSWORD
go run ./cmd/cilici
# 另开终端
curl -s http://127.0.0.1:8080/api/health
# {"ok":true,"version":"dev","uptimeMs":12}
```

或者：

```bash
make build && make run
make test
```

## Docker

镜像只包含后端二进制（alpine，约 20MB），数据挂载在 `/data`。

```bash
cp .env.example .env
docker compose up -d --build
curl -s http://127.0.0.1:8080/api/health
```

- 源码构建：`Dockerfile`（`golang:1.27-alpine` 多阶段编译）
- 国内/离线：先 `make linux`，再用 `deploy/Dockerfile.prebuilt` 构建（不拉 Go 基础镜像）：
  ```bash
  docker build -f deploy/Dockerfile.prebuilt -t cilicili-backend:latest .
  ```
- 详细说明与 systemd/nginx 反代见 [deploy/README.md](deploy/README.md)。

## 配置

全部走环境变量，参考 [.env.example](.env.example)；`PASSWORD` 必填，其余有默认值。
`SOURCES_FILE` 首次启动导入默认源，之后以 `sources.db` 为准。

## Web 前端

```bash
cd web
npm install
npm run dev     # 开发，默认 5173，反代 /api 到 8080
npm run build   # 产物在 web/dist
```

后端设 `WEB_DIR=/path/to/web/dist` 即可同源托管 SPA（免 Nginx，cookie 鉴权也是同源）。

## 目录

```
cmd/cilici/        程序入口
internal/          API、搜索、详情、代理、缓存、源管理、媒体聚合、用户数据
web/               React + TypeScript + Vite 前端
configs/           默认源配置
docs/              API 与设计文档（docs/web/ 为前端与功能方案）
deploy/            Docker/prebuilt 与部署说明
data/              运行时数据（gitignore）
```

## 致谢

本项目在下列开源工作的基础上构建：

- [LibreTV](https://github.com/LibreSpark/LibreTV) — 最初的服务端设计参考；本仓库的 Go 实现是重写版本，
  HLS 错误恢复策略（`web/src/lib/playback-recovery.ts`）思路移植自 LibreTV-Next
- [ArtPlayer](https://github.com/zhw2590582/ArtPlayer) — 播放器内核
- [hls.js](https://github.com/video-dev/hls.js) — HLS 播放
- [MiSans](https://hyperos.mi.com/font) — 界面字体（小米开源，按 SIL OFL 分发）
- [TMDB](https://www.themoviedb.org/) — 影片元数据与横版剧照（本项目非 TMDB 官方产品）

## 免责声明

仅供个人学习与自建使用。本项目不托管、不存储、不分发任何影视内容，所有内容均来自用户自行配置的
第三方源站；聚合内容的版权归各源站及权利人所有。请遵守当地法律法规，不要用于任何商业或公开传播用途。

## 许可证

[MIT](LICENSE)
