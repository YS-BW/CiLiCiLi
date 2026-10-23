# Benchmark 原始数据

对应分析文档：`../backend-rewrite-assessment.md`

| 文件 | 来源 | 说明 |
| --- | --- | --- |
| `vps_raw.json` | 腾讯云上海 CVM → 19 个采集站 | 19 并发、每源独立 curl（冷连接）、带 Chrome UA、`ac=videolist&wd=庆余年&pg=1`、3 轮；逐源记录 DNS/TCP/TLS/TTFB/总耗时 |
| `warm3.json` | 同一 VPS | 第 1 轮冷连接；第 2/3 轮复用连接、换关键词（避免源站缓存）；15 个源可用 |
| `app_local_cold.json` | 本机 Docker（LibreTV v2.19.2） | 登录后直接打 `/api/search`：单源、19 源流式、19 源非流式、缓存命中；服务端自报 `ms` 与端到端耗时对比 |

## 复现方式

```bash
# VPS 裸源站
scp vps_raw.json  # 仅数据
python3 - <<'PY'
import json
d=json.load(open('vps_raw.json'))
print(d['rounds'])
PY

# 本机 App 端到端（需要本地 LibreTV 容器监听 8080）
# 见仓库外临时脚本 /tmp/bench/bench_app2.py
```

## 关键边界

- **VPS 上 LibreTV 容器当前未运行**（nginx 502），所以端到端数据来自本机容器；VPS 只测「到源站的网络耗时」和「出口带宽」。
- 本机容器从 Docker 继承了 `HTTP_PROXY`（Clash），出网路径与 VPS 直连不同，因此**不要把本机的 19 源绝对耗时与 VPS 对比**；本机数据只用于「端到端 − 服务端自报 = 框架开销」。
- 采集站响应波动极大：同一源不同轮次可差 10 倍；任何单次数字都不应作为 SLA。
- 关键词不同，源站命中/无结果的数量也会不同（例如 `ckzy` 有时挂死、有时 2 s 返回）。
