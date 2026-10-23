# 部署（腾讯云 CVM / 任意 Linux）

目标：单二进制 + systemd + nginx，不依赖 Docker（该 VPS 连不上 Docker Hub）。

## 1. 本机交叉编译

```bash
make linux
# 产物：bin/cilicili-linux-amd64
```

## 2. 上传

```bash
scp bin/cilicili-linux-amd64 tencent-vps:/usr/local/bin/cilicili
ssh tencent-vps 'chmod 755 /usr/local/bin/cilicili'
```

## 3. VPS 初始化（一次性）

```bash
ssh tencent-vps
useradd --system --home /var/lib/cilicili --shell /usr/sbin/nologin cilicili || true
install -d -o cilicili -g cilicili -m 750 /var/lib/cilicili
install -d -m 755 /etc/cilicili
# 写入环境变量（参考 .env.example）；至少 PASSWORD / LISTEN=127.0.0.1:8080
cat >/etc/cilicili/env <<'EOF'
PASSWORD=change-me
PROXY_SECRET=请替换为 openssl rand -hex 32
LISTEN=127.0.0.1:8080
DATA_DIR=/var/lib/cilicili
SOURCES_FILE=/etc/cilicili/sources.json
LOG_LEVEL=info
EOF
chmod 600 /etc/cilicili/env
scp configs/sources.json tencent-vps:/etc/cilicili/sources.json  # 从本机执行
```

## 4. systemd

```bash
scp deploy/cilicili.service tencent-vps:/etc/systemd/system/
ssh tencent-vps 'systemctl daemon-reload && systemctl enable --now cilicili'
ssh tencent-vps 'systemctl status cilicili --no-pager'
curl -s http://127.0.0.1:8080/api/health
```

查看日志、重启：

```bash
ssh tencent-vps 'journalctl -u cilicili -f'
ssh tencent-vps 'systemctl restart cilicili'
```

## 5. nginx

把 `deploy/nginx-api.conf` 里的 `location /api/` 合并进现有 443 server 块：

```bash
ssh tencent-vps 'nginx -t && systemctl reload nginx'
curl -sk https://124.221.121.195/api/health
```

## 6. 升级

```bash
make linux
scp bin/cilicili-linux-amd64 tencent-vps:/usr/local/bin/cilicili.new
ssh tencent-vps 'mv /usr/local/bin/cilicili.new /usr/local/bin/cilicili && systemctl restart cilicili'
```

## 7. 回滚

保留上一版二进制：

```bash
ssh tencent-vps 'cp /usr/local/bin/cilicili /usr/local/bin/cilicili.prev'
# 回滚：cp ...prev ... && systemctl restart cilicili
```

## 备注

- 数据目录只存缓存与健康状态，删除即可重建；不会丢用户数据（当前阶段没有服务端用户数据）。
- 若用自签证书，Android App 需要证书 pin；正式产品建议域名 + Let's Encrypt。
- VPS 出口约 2.9 Mbps：Web/App 播放优先直连 CDN，代理只做兜底。
