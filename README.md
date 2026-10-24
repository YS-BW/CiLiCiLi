# cilicili（CiLiCiLi）

LibreTV 后端的 Go 重写版：多源聚合搜索、详情解析、流式代理、持久缓存与服务端源健康。
目标是单二进制、可部署、Web 与原生 App 共用一套 API。

> 当前阶段：服务端持续开发；**Fladder Web 已接入核心点播流程**，原生端后续处理。
> 设计依据与实测数据见 [docs/backend-rewrite-assessment.md](docs/backend-rewrite-assessment.md)。

## 特性（规划中按里程碑交付）

- [x] 多源并发聚合搜索（NDJSON 流式 + 软截止 `partial_done`）
- [x] 两级缓存（内存 LRU + bbolt 持久化，TTL/SWR）
- [x] 服务端全局源健康 / 熔断 / 负缓存
- [x] 详情解析（列表接口优先，详情页 HTML 兜底）
- [x] 结构化播放列表：多线路 + 原始集名 + 与 URL 解耦的分集 ID
- [x] 观看进度 / 观看历史 / 收藏（独立持久化，默认用户）
- [x] 媒体聚合（一张影片一张卡）+ 本地封面缓存
- [x] 媒体聚合卡片 + 采集站封面本地缓存（第三方 provider 暂未接入）
- [x] SSRF 防护（DNS 固定、逐跳重定向复检）
- [x] Bearer token + Web cookie 双鉴权
- [x] 流式代理（图片/m3u8/分片/直播）+ m3u8 重写
- [ ] 画质探测（m3u8 / 分片 SPS / MP4 moov）— 进行中
- [ ] 直播（M3U/EPG/测活/长连接）— 进行中
- [x] 首页推荐（豆瓣 / 影视榜单）
- [ ] 源订阅 / 发布 — 进行中
- [x] Fladder Web 核心点播：登录、推荐、搜索、详情/选集、直连/代理播放、续播、收藏
- [ ] 原生 App（Web 稳定后）

## 快速开始

```bash
cp .env.example .env
# 编辑 .env，至少设置 PASSWORD 和 SOURCES_FILE
make run
```

默认监听 `:8080`。健康检查：

```bash
curl -s localhost:8080/api/health
# {"ok":true,"version":"dev","uptimeMs":12}
```

登录拿 token（App 用 Bearer；Web 用 cookie）：

```bash
curl -s -c /tmp/c.txt -H 'Content-Type: application/json' \
  -d '{"password":"你的密码"}' localhost:8080/api/auth
curl -s -b /tmp/c.txt -H 'Content-Type: application/json' \
  -d '{"wd":"庆余年"}' localhost:8080/api/search
```

流式搜索：

```bash
curl -N -b /tmp/c.txt -H 'Content-Type: application/json' \
  -d '{"wd":"庆余年"}' 'localhost:8080/api/search?stream=1'
```

## Web 前端

使用原版 Fladder（Flutter）界面和动画，客户端适配现有 Go API：

```bash
# Flutter 3.35.7；Go 服务先按上文启动
make web-deps
make web-build
make web-serve
```

打开 `http://127.0.0.1:3000`，使用后端密码登录。部署要求静态页面与 `/api` 同源，详见 [Web 接入与运行说明](clients/fladder/CILICILI.md)。

## 配置

全部通过环境变量（与 LibreTV 兼容），详见 [.env.example](.env.example)：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `PASSWORD` | 空 | 必填；为空时受保护接口返回 503 |
| `PROXY_SECRET` | 由 PASSWORD 派生 | token HMAC 密钥；生产建议显式设置 |
| `LISTEN` | `:8080` | 监听地址 |
| `DATA_DIR` | `./data` | 数据目录 |
| `CACHE_DB` | `$DATA_DIR/cache.db` | 持久缓存文件 |
| `USER_DB` | `$DATA_DIR/userdata.db` | 观看进度/收藏（独立于缓存） |
| `MEDIA_DB` | `$DATA_DIR/media.db` | 媒体库（SQLite + WAL） |
| `MEDIA_COVER_DIR` | `$DATA_DIR/covers` | 本地封面文件目录 |
| `MEDIA_ENABLED` | `1` | 关闭后媒体聚合/刮削接口返回 501 |
| `MEDIA_AUTO_CONFIRM` | `0` | 强匹配自动确认（默认只给候选） |
| `SOURCES_FILE` | 空 | 源配置文件（JSON）；与 `DEFAULT_SOURCES` 二选一 |
| `DEFAULT_SOURCES` | 空 | 点播源 JSON 数组（LibreTV 兼容） |
| `DEFAULT_LIVE_SOURCES` | 空 | 直播源 JSON 数组 |
| `SEARCH_MAX_PAGES` | `5` | 每源最多抓取页数 |
| `SEARCH_FAST_SOURCES` | `8` | fast 模式只查健康度最好的 N 个源；0=禁用 |
| `SEARCH_SOURCE_TIMEOUT_MS` | `6000` | 单源总死线 |
| `SEARCH_SOFT_DEADLINE_MS` | `1200` | 软截止（发 partial_done） |
| `SEARCH_HARD_DEADLINE_MS` | `6000` | 搜索硬截止 |
| `SEARCH_CACHE_TTL_MS` | `300000` | 搜索缓存新鲜期 |
| `REQUEST_TIMEOUT` | `8000` | 详情/代理等请求超时 |
| `LIVE_ALLOW_PRIVATE` | `0` | 放行内网直播源（自建 IPTV） |
| `CORS_ORIGINS` | 空 | 允许的 Web 源（逗号分隔；同源部署可不设） |

## Linux 部署（绕开 Docker Hub）

```bash
make linux
scp bin/cilicili-linux-amd64 tencent-vps:/usr/local/bin/cilicili
# VPS 上写 systemd unit，EnvironmentFile=/etc/cilicili/env，ExecStart=/usr/local/bin/cilicili
```

nginx 反代示例：

```nginx
location /api/ {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;
    proxy_read_timeout 3600s;
}
```

## 目录结构

```
cmd/cilici/         入口
internal/api/       HTTP handlers
internal/server/    路由/中间件/鉴权
internal/search/    聚合搜索、流式、缓存
internal/detail/    详情解析
internal/proxy/     流式代理
internal/m3u8/      m3u8 重写
internal/upstream/  出网客户端（SSRF/DNS/超时/重试）
internal/cms/       Apple CMS 解析
internal/filter/    过滤/排序
internal/cache/     两级缓存
internal/source/    源注册表 + 健康/熔断
internal/config/    env 配置
internal/auth/      会话/鉴权
internal/httpx/     JSON/NDJSON 辅助
docs/               设计与 API 文档
configs/            源配置
clients/fladder/    Fladder Flutter 客户端（本轮 Web）
scripts/            Web 本地同源预览与测试
```

## 文档

- [API 契约](docs/API.md)
- [实施计划](docs/PLAN.md)
- [重写评估（LibreTV 基线）](docs/backend-rewrite-assessment.md)
- [Go 版性能实测](docs/BENCHMARKS.md)

## 合规

本项目只聚合第三方公开接口，不存储内容。源站可用性、内容合法性由源站负责；请自行评估使用风险。
