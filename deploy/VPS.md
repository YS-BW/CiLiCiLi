# 当前 VPS 后端部署

部署时间：2026-10-25。服务器：`124.221.121.195`。

## 2026-10-28 更新：TMDB 横版图（backdrop）

- 新增：TMDB 搜索结果解析 `backdrop_path`，`Media` / `Media.metadata` / `SearchMedia` 均返回
  `backdrop` 字段（`https://image.tmdb.org/t/p/w1280/...`），供 Web 首页主推/详情背景使用。
- 媒体库 `meta_backdrop` 列自动迁移（`ALTER TABLE ADD COLUMN`，老库无损）。
- 新镜像 tag：`cilicili-backend:backdrop-20261028-210107`；release 目录
  `/opt/cilicili-backend/releases/backdrop-20261028-210107`，`current` 已切到该目录。
- 部署前备份：`/opt/cilicili-backend/backups/pre-backdrop-20261028-210242/`（sources.db 一致快照，23 源）。
- 部署后校验：容器 healthy；线上源仍为 **23 个**（无损）；登录→resolve→refresh→详情，`backdrop` 真实返回 TMDB w1280 URL。
- 回退：`ln -sfn /opt/cilicili-backend/releases/20261025-012829 /opt/cilicili-backend/current` 后 `docker compose up -d`。
- ⚠️ 本地 Mac 的 Docker（Clash TUN）到 `api.tmdb.org` 会 fake-IP / TLS 超时，导致本地容器刮削失败；
  这是本地网络环境问题，VPS 正常。本地验证 backdrop 需 `LIVE_ALLOW_PRIVATE=true` + 直连 DNS。

---

## 2026-10-29 更新：前端并入后端 + 全站 Web

- 前端构建产物 dist 并入后端镜像（`/web`），后端以 `WEB_DIR=/web` 直接托管 SPA（同源，免 Nginx 镜像）。
- 新镜像 tag：`cilicili-backend:web-20261029-020809`（以旧镜像为底座离线构建）；release 目录 `/opt/cilicili-backend/releases/web-20261029-020809`。
- 宿主 Nginx：`location /` 由 `return 404` 改为反代 `http://127.0.0.1:8080`（/api 规则不变），已 reload。
- 部署前备份：`/opt/cilicili-backend/backups/preweb-20261029-020830`（sources.db 快照，23 源）。
- 部署后校验：HTTPS 首页 200、/api/health 正常、搜索 45 条、首页 douban 带评分/源数、big 封面 200、**源 23 保留、历史 8 条保留**。
- 访问：`https://124.221.121.195/`（自签证书）。回退：把 current 指回上一个 release 后 compose up -d，并把 nginx `location /` 改回 return 404。

---

以下为 2026-10-25 首次部署记录：

- 仅运行 `cilicili-backend`，Docker 健康状态为 healthy，自动重启策略 unless-stopped。
- 部署时版本：`84dcea5-d887ec72c7`，包含部署时本地未提交的后端改动。
- 镜像：`cilicili-backend:84dcea5-d887ec72c7`；同时设置 `cilicili-backend:latest` 标签。
- 当前部署入口：`/opt/cilicili-backend/current`。
- 数据：`/var/lib/cilicili`，挂载到 `/data`，保留原观看记录、媒体和源配置。
- 完整停机备份：`/opt/cilicili-backend/backups/20261025-012829/data.tar.gz`。
- 容器仅绑定 `127.0.0.1:8080`，宿主机 Nginx 提供 HTTP/HTTPS `/api/*` 转发；不再托管旧 Web。
- `.env` 权限 0600，保留线上密钥；不复制 Mac 的 host.docker.internal 代理，关闭 LIVE_ALLOW_PRIVATE。
- 现有源库保留；默认源文件已同步本地配置，不重置线上源标识。
- 媒体库和 TMDB 自动匹配按本地配置启用，TMDB 实测返回 200。

## 管理

```sh
cd /opt/cilicili-backend/current
docker compose -f compose.yml ps
docker compose -f compose.yml logs --tail=100
docker compose -f compose.yml up -d --no-build --pull never
```

不要使用旧 `/opt/cilicili/compose.yml`。旧容器 `cilicili-rollback-20261025-012829` 已停止、设置 restart=no，仅供回退。

## 验收

本地 `go test ./...` 通过。公网健康接口正常，未登录源管理返回 401。
登录、推荐、聚合搜索、媒体详情、封面缓存、36 集线路均已验证；HLS 分片经 Nginx/Go 代理返回 206，成功读取 1024 字节 TS 数据。
原有 6 条观看记录仍在。未写入测试播放进度或收藏。

程序 SHA-256：`423e582a81c49c64eb169ae3c23f08fbcca237a0b4abb8ce11b4d0202e0e2316`。
源码快照摘要：`d887ec72c708cdb0651bd235c5d6b1927eb557a32a10421ce17d120e852226d2`。

HTTPS 沿用原来的自签名证书；本次没有更换证书。
