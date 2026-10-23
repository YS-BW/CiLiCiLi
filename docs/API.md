# cilicili API 契约（v1，服务端阶段）

Base URL：`/api`。除特别说明外：

- 请求/响应均为 `application/json; charset=utf-8`。
- 鉴权：Web 用 httpOnly cookie `cilici_session`；App 用 `Authorization: Bearer <token>`。
- 受保护接口在未登录时返回 `401 {"error":"未登录"}`；服务器未配置密码时返回 `503`。
- 错误统一为 `{"error":"..."}`。
- 时间：token 过期时间用 Unix 毫秒；其他时间不直接暴露。

## 1. 公开接口

### GET /api/health

无需鉴权。

```json
{"ok":true,"version":"1.0.0","uptimeMs":12345}
```

### GET /api/status

无需鉴权；用于 Web 判断登录态与预置源。

```json
{
  "passwordRequired": true,
  "verified": false,
  "version": "1.0.0",
  "defaultSources": [
    {"key":"env_0","name":"暴风资源","url":"https://.../api.php/provide/vod"}
  ],
  "defaultLiveSources": [],
  "defaultSubscriptions": [],
  "defaultRecommendSource": "",
  "defaultImageMode": "",
  "capabilities": ["search.stream","search.soft_deadline","search.server_health","detail","proxy","sources","auth.bearer"]
}
```

### POST /api/auth

```json
{"password":"..."}
```

成功：`200`

```json
{"success":true,"token":"v1.<...>.<...>","expiresAt":1760000000000}
```

同时设置 `Set-Cookie: cilici_session=...; HttpOnly; SameSite=Lax; Secure(https)`。
失败：`401 {"error":"密码错误"}`；限速：`429`；未配置密码：`503`。

### GET /api/auth

```json
{"success":true,"verified":true}
```

### DELETE /api/auth

清除 cookie；`{"success":true}`。

## 2. 搜索

### POST /api/search

需要登录。

```json
{
  "wd": "庆余年",
  "sources": [
    {"key":"env_0","name":"暴风资源","url":"https://.../api.php/provide/vod"}
  ],
  "filterAdult": true,
  "fast": true
}
```

- `sources` 可省略/为空：使用服务端配置的全部源。
- `filterAdult` 默认 `true`。
- `fast` 默认 `true`：服务端按健康度（熔断等级 → EWMA 延迟）只查最快的 `SEARCH_FAST_SOURCES`（默认 8）个源；传 `false` 查全部源。响应头 `X-Search-Sources: 8/18` 表示实际参与/请求的源数。
- 快速模式是首选：首源通常 < 300 ms，软截止 1.2 s，完成通常 1.2–2.5 s；全量模式用于“搜索全部源”。
- 返回（非流式，等待硬截止 6s）：

```json
{
  "list": [
    {
      "sourceKey":"env_0","sourceName":"暴风资源","vodId":"12345",
      "name":"庆余年","pic":"https://...","typeName":"国产剧",
      "year":"2019","area":"大陆","remarks":"全46集",
      "sourceUrl":"https://.../api.php/provide/vod"
    }
  ],
  "failures": [
    {"sourceKey":"env_7","error":"请求超时（>6s）","timedOut":true}
  ]
}
```

### POST /api/search?stream=1

需要登录。响应 `application/x-ndjson`，逐行 JSON，服务端主动 flush。

事件顺序：

1. `{"type":"source","sourceKey":"env_0","ok":true,"list":[...],"ms":812}`
   - 每个源结算即推送；失败源 `ok:false` 且带 `error`/`timedOut`。
2. `{"type":"partial_done","list":[...],"failures":[...],"partial":true}`
   - 默认 1.2s 软截止发送；客户端可立即结束“加载中”，后续 source 事件仍可追加。
3. `{"type":"source", ...}`（软截止后完成的慢源继续补）
4. `{"type":"done","list":[...],"failures":[...]}`
   - 所有源完成或 6s 硬截止时发送；这是最终聚合结果（去重/成人过滤/相关性过滤/排序后）。

缓存：相同关键词 + 源集合 + 过滤开关 + 快速模式，5 分钟内直接返回缓存（流式会立即发 `done`）。

## 3. 详情

### GET /api/detail

需要登录。Query：

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| `id` | 是 | 影片 ID（`[A-Za-z0-9_-]{1,64}`） |
| `source` | 二选一 | `SourceConfig` 的 JSON 字符串（URL 编码） |
| `sourceKey` | 二选一 | 服务端已注册源的 key |

响应：

```json
{
  "episodes":["https://.../index.m3u8","https://.../index.m3u8"],
  "videoInfo":{
    "title":"庆余年","cover":"https://...","desc":"...",
    "typeName":"国产剧","year":"2019","area":"大陆",
    "director":"...","actor":"...","remarks":"全46集",
    "sourceKey":"env_0","sourceName":"暴风资源","sourceUrl":"https://..."
  }
}
```

状态码：`400` 参数无效；`404` 无播放资源；`502` 上游失败。

## 4. 源管理

### GET /api/sources

需要登录。返回注册源与服务端健康快照：

```json
{
  "sources":[{"key":"env_0","name":"暴风资源","url":"https://..."}],
  "health":[
    {"key":"env_0","ewmaMs":812,"failStreak":0,"level":0,"openUntil":0,"disabled":false,"successes":12,"failures":0}
  ]
}
```

健康语义：连续 2 次失败 → 30 分钟熔断；再犯 → 24 小时；再犯 → 长期停用（`disabled:true`，需管理接口重置）。熔断中的源不参与搜索，且不等待。

### POST /api/sources/test

需要登录。

```json
{"url":"https://.../api.php/provide/vod"}
```

响应（始终 200，由 `ok` 区分）：

```json
{"ok":true,"ms":812,"count":20}
{"ok":false,"ms":6000,"error":"请求超时（>6s）"}
```

## 5. 代理

### GET /api/proxy?url=<encoded>

也兼容 `GET /api/proxy/<encoded>`。用于图片、m3u8、分片（Web 端 CORS/防盗链兜底）。

- 已登录：任意公网 http(s)。
- 未登录：仅放行豆瓣 / Bangumi 图片域（精确后缀白名单）。
- 透传 `Range`/`If-*`，支持 `HEAD`；分片流式转发。
- m3u8 会重写内部的 variant/分片/key/map 地址，指向本代理。
- 返回 `Access-Control-Allow-Origin: *`。
- 错误：`400` 缺/非法 URL；`401` 未登录；`403` SSRF 拒绝；`502` 上游失败。

### GET /api/live/stream?url=<encoded>

直播长连接代理（FLV/HLS）。只限制等待响应头的时间（默认 15s），body 不限时。其余同 `/api/proxy`。

## 6. 暂未实现（已占位，返回 501）

| 接口 | 计划 |
| --- | --- |
| `POST /api/quality`（`?stream=1`） | 画质探测 |
| `GET /api/source-list?url=` | 源订阅（LibreTV/TVBOX/目录） |
| `POST /api/publish` | 发布源列表 |
| `GET /api/douban`、`/api/bangumi/calendar`、`/api/hot-list` | 首页推荐 |
| `GET /api/live/playlist`、`/api/live/epg`、`POST /api/live/probe` | 直播列表/EPG/测活 |

## 7. 兼容性与版本

- 路径保持 `/api/...`，与 LibreTV 前端大体兼容；新增字段只增不改。
- 破坏性变更走 `/api/v2` 或新增字段 + 客户端适配。
- 服务端能力通过 `/api/status.capabilities` 暴露，客户端按能力降级。
