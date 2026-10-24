# 部署说明（后端）

后端是单个 Go 二进制，除 `PASSWORD` 外无必须的外部依赖。

## Docker（推荐）

```bash
cp .env.example .env        # 改 PASSWORD
docker compose up -d --build
curl -s http://127.0.0.1:8080/api/health
```

- 数据默认挂载到 `./data`（可改 `docker-compose.yml` 的 volumes）
- 镜像内置 `/etc/cilicili/sources.json` 作为首次启动的默认源
- 国内网络无法拉 `golang` 基础镜像时，用预编译方式：

```bash
make linux   # 产出 bin/cilicili-linux-amd64
docker build -f deploy/Dockerfile.prebuilt -t cilicili-backend:latest .
docker run -d --name cilicili-backend --restart unless-stopped \
  --env-file .env -p 8080:8080 -v "$PWD/data:/data" cilicili-backend:latest
```

## 裸机 + systemd

```bash
make linux
scp bin/cilicili-linux-amd64 root@host:/usr/local/bin/cilicili
```

```ini
[Unit]
Description=CiLiCiLi backend
After=network-online.target

[Service]
WorkingDirectory=/var/lib/cilicili
EnvironmentFile=/etc/cilicili/env
ExecStart=/usr/local/bin/cilicili
Restart=always
User=cilicili

[Install]
WantedBy=multi-user.target
```

`/etc/cilicili/env` 至少包含：

```
PASSWORD=change-me
LISTEN=127.0.0.1:8080
DATA_DIR=/var/lib/cilicili
SOURCES_FILE=/etc/cilicili/sources.json
```

## 对外提供 HTTPS

后端只监听本机，由 nginx/caddy 反代即可（`/api/*` 全部转发，注意保留 `Host`、
`X-Forwarded-For`、`X-Forwarded-Proto`；视频代理是长连接，读超时建议 >= 1h）。
