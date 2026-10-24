# CiLiCiLi 后端

CiLiCiLi 的服务端：多源聚合搜索、详情解析、流式代理、媒体聚合/刮削、持久缓存、源健康探测与自动发现。
对外只暴露一套 HTTP API（`/api/*`）；Web / App 客户端不在本仓库内。

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

## 目录

```
cmd/cilici/        程序入口
internal/          API、搜索、详情、代理、缓存、源管理、媒体聚合、用户数据
configs/           默认源配置
docs/              API 与设计文档
deploy/            Docker/prebuilt 与部署说明
data/              运行时数据（gitignore）
```

## 合规

仅供个人学习与自建使用；聚合内容版权归各源站所有，请遵守当地法律法规。
