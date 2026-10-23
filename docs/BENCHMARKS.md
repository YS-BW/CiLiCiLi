# 性能实测（Go 重写版）

> 环境：腾讯云上海 CVM（4 vCPU Xeon 8255C / 3.6 GB），Go 1.27.3 静态二进制，18 个点播源。
> 对比基线：LibreTV v2.21.0（Next.js），同机/同源实测见 `backend-rewrite-assessment.md`。
> 日期：2026-10-24

## 1. 搜索

### LibreTV（基线）

| 指标 | 数值 |
| --- | --- |
| 首源返回 | 0.35–0.48 s（VPS 直连源站） |
| 单源中位 | 1.4–2.4 s |
| 全部完成 | 3.4–8.0 s；有死源时 `done` 等 10 s |
| 服务端开销 | 1.4–5.9 ms（与语言无关） |

### cilicili（Go）

**VPS 内部 bench（`cmd/bench`，健康预热后，fast 模式 8/18 源）**

| 关键词 | 首源 | 软截止 partial_done | done | 条数 |
| --- | --- | --- | --- | --- |
| 庆余年 | 257 ms | 1203 ms | **1259 ms** | 89 |
| 繁花 | 292 ms | 1203 ms | 3448 ms | 328 |
| 漫长的季节 | 168 ms | 1202 ms | 2914 ms | 14 |

**外部 HTTPS（App/Web 视角，fast 模式）**

| 关键词 | 首源 | partial | done | 源数 |
| --- | --- | --- | --- | --- |
| 庆余年 | 121 ms | —（8 源都很快） | **200 ms** | 8/18 |
| 繁花 | 115 ms | — | **224 ms** | 8/18 |
| 狂飙 | 117 ms | — | **229 ms** | 8/18 |
| 漫长的季节 | 427 ms | 1319 ms | 1740 ms | 8/18 |

- 缓存命中：**0.1–1 ms**（内存 LRU）。
- 死源：预热后服务端熔断，后续搜索 **0 次网络请求**（约 0–1 ms 返回）。
- 全量模式（18/18 源）：无死源时约 1.7–5 s；配合 `partial_done` 1.2 s 已可结束 UI loading。

## 2. 为什么快了

1. **fast 模式**：按健康度（熔断等级 → EWMA 延迟）只查最快的 8 个源；全量源按需再查。
2. **软截止**：1.2 s 发 `partial_done`，慢源结果继续补；UI 不必等 `done`。
3. **服务端全局健康**：连续 2 次失败熔断 30m → 24h → 长期；新设备不再先踩死源。
4. **两级缓存**：搜索/源级/详情/画质持久化到 bbolt，重启不丢；失败结果只缓存 60 s。
5. **连接池 + 启动预热**：启动时后台探测源，复用 TCP/TLS，填充健康状态。
6. **服务端开销**：Go 版在缓存命中/框架层约 1–2 ms，比 Next.js 略低，但不是主因。

## 3. 播放

- VPS 出口实测 **2.86 Mbps**（32 MiB / 93.7 s），1080p HLS 直转必卡。
- 结论不变：Web/App 优先直连 CDN；`/api/proxy` 只做图片/防盗链/CORS 兜底。
- 详情接口已缓存 10 分钟，起播前只差一次详情 + m3u8 拉取；后续可做「详情预热 + 首分片预取」。

## 4. 复现

```bash
# 本机
go run ./cmd/bench -sources configs/sources.json -keywords 庆余年,繁花 -runs 2 -warm -verbose

# VPS（交叉编译后传上去）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bench ./cmd/bench
./bench -sources sources.json -keywords 庆余年 -runs 2 -warm
```

> 采集站波动极大：同一源不同轮次可差 5–10 倍；上表是多次实测的典型值，不应作为 SLA。
> VPS 上服务端启动时会有 1–2 分钟后台预热，预热完成前首次搜索可能仍等待慢源。
