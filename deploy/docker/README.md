# Docker 部署（单容器：API + Flutter Web）

容器内由一个 Go 进程同时提供 `/api/*` 和 Flutter Web 静态文件（`WEB_DIR`），
对外只暴露 `127.0.0.1:8080`，由宿主机 nginx 做 TLS/反代。

## 构建

在项目根目录：

```bash
# 1) 交叉编译 Linux 二进制
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags '-s -w' -o /tmp/cilici-deploy/cilicili ./cmd/cilici

# 2) 构建 Web（Flutter 3.35.7）
cd clients/fladder && flutter build web --no-wasm-dry-run --no-web-resources-cdn --pwa-strategy=none
cd -

# 3) 组装 context：cilicili + web/ + sources.json + ca-certificates.crt + zoneinfo/
#    （Dockerfile/compose.yml 即本目录文件）
```

`Dockerfile` 基于已存在的 `alpine:3` 镜像（不依赖 Docker Hub），把 CA 证书与
`zoneinfo` 打进镜像，避免容器内再联网安装。

## 运行

```bash
cd /opt/cilicili   # 上传后的目录（二进制、web、sources.json、Dockerfile、compose.yml、.env）
docker compose up -d --build
docker ps --filter name=cilicili
curl -s http://127.0.0.1:8080/api/health
```

`.env` 至少包含：

```
PASSWORD=...
PROXY_SECRET=...
LISTEN=0.0.0.0:8080
DATA_DIR=/data
CACHE_DB=/data/cache.db
USER_DB=/data/userdata.db
MEDIA_DB=/data/media.db
SOURCES_DB=/data/sources.db
MEDIA_COVER_DIR=/data/covers
SOURCES_FILE=/etc/cilicili/sources.json
WEB_DIR=/web
TMDB_READ_TOKEN=...
LOG_LEVEL=info
```

`/var/lib/cilicili` 挂载到容器 `/data`，保留缓存、健康、用户数据与媒体库。
宿主机 nginx 的 `/` 继续反代 `127.0.0.1:8080` 即可同时服务 Web 和 API。
