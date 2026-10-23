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
  "episodes": ["https://cdn.test/1/index.m3u8", "https://cdn.test/2/index.m3u8"],
  "playLines": [
    {
      "id": "l1",
      "name": "ffm3u8",
      "index": 1,
      "preferred": true,
      "episodes": [
        {"id":"l1e1","name":"第01集","index":1,"url":"https://cdn.test/1/index.m3u8"},
        {"id":"l1e2","name":"第02集","index":2,"url":"https://cdn.test/2/index.m3u8"}
      ]
    },
    {
      "id": "l2",
      "name": "llm3u8",
      "index": 2,
      "preferred": false,
      "episodes": [
        {"id":"l2e1","name":"正片","index":1,"url":"https://cdn2.test/full.mp4"}
      ]
    }
  ],
  "videoInfo": {
    "title":"庆余年","cover":"https://...","desc":"...",
    "typeName":"国产剧","year":"2019","area":"大陆",
    "director":"...","actor":"...","remarks":"全46集",
    "sourceKey":"env_0","sourceName":"暴风资源","sourceUrl":"https://..."
  }
}
```

字段说明（Fladder 等客户端按此渲染播放列表）：

- `playLines[]`：采集站返回的全部可用线路（`vod_play_from` 对应 `name`，缺失时为「线路N」）；
  每条线路按原顺序给出 `episodes[]`。
- `playLines[].preferred`：第一条包含 `.m3u8` 直链的线路为 `true`（采集站首条常是分享页/中转页），
  客户端可默认选它；服务端不探测线路是否真的可播。
- `episodes[].name`：采集站原始集名（如「第01集」「正片」「预告」），没有则空字符串，不推测。
- `episodes[].id`：来源内稳定标识，格式 `l<线路序号>e<集序号>`，**不是播放 URL**；客户端保存进度时应使用
  它，而不是可能过期的 URL。
- `episodes[].index`：在该线路内的 1-based 顺序。
- `episodes[].url`：播放地址，可能随时间失效；继续播放前应重新请求本接口。
- 顶层 `episodes` 是旧字段（优先 m3u8 线路的扁平 URL 列表），仅用于兼容旧客户端；
  新客户端请使用 `playLines`。

#### 标识与绑定规则

- 影片身份：`sourceKey + vodId`（同一作品在不同源下是两条记录，第一版不做跨源合并）。
- 分集身份：`lineId + episodeId`（服务端生成的 `l1e1` 形式），与 URL 解耦。
- 豆瓣 / Bangumi 推荐卡片的 `id` 属于推荐域，**不是**采集站 `vodId`；点击推荐后应先用标题调用
  `/api/search`，由用户选择源与影片。
- 未知字段一律为空值，服务端不推测季数、时长、剧情或缩略图。

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

### POST /api/sources/reset

清除单个源的熔断/长期停用状态。

```json
{"key":"env_0"}
```

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

## 6. 画质探测

### POST /api/quality（`?stream=1`）

需要登录。Body：

```json
{"items":[{"source":{"key":"env_0","name":"暴风资源","url":"https://..."},"id":"12345"}]}
```

- 非流式：`{"results":[{"sourceKey":"env_0","vodId":"12345","height":1080}]}`；
  `height` 缺失=测不出（单码率/直链/防盗链），有 `error`=本次失败。
- 流式：先发一行 `{"ping":true}`，再逐条发结果；服务端并发 3、同 host ≤3。
- 缓存：成功 30 天 / 失败 2 分钟。单条总死线默认 8s。

## 7. 直播

| 接口 | 说明 |
| --- | --- |
| `GET /api/live/playlist?url=<m3u>&force=1` | 拉取并解析 M3U；10 分钟缓存，stale 6 小时兜底 |
| `GET /api/live/epg?url=<xmltv>&channel=<tvgId>&force=1` | XMLTV 节目单；6 小时缓存，返回 current/next/programs |
| `POST /api/live/probe`（`?stream=1`） | 批量测活；body `{"urls":[...]}`，最多 50 条；三级探测 manifest→variant→分片，返回 level/codec/kbps |
| `GET /api/live/stream?url=<encoded>` | 长连接代理（FLV/HLS），只限制响应头等待时间，body 不限时 |

## 8. 首页推荐

| 接口 | 说明 |
| --- | --- |
| `GET /api/douban?type=movie\|tv&tag=<tag>&pageStart=0&pageSize=25` | 豆瓣推荐；10 分钟缓存 |
| `GET /api/bangumi/calendar` | Bangumi 每日放送；30 分钟缓存 |
| `GET /api/hot-list?id=<id>` | 影视榜单；1 小时缓存；未知 id 返回 400。可用 ids：`douban_movie_weekly` / `douban_tv_chinese` / `douban_tv_global` / `douban_show_chinese` / `douban_show_global` / `baidu_teleplay` |

## 9. 源订阅

### GET /api/source-list?url=<encoded>

需要登录。拉取远程订阅，支持 LibreTV-SourceList / TVBOX / 接口目录；返回：

```json
{
  "name": "订阅名",
  "sources": [{"key":"sub_ab12cd34","name":"源名","url":"https://..."}],
  "liveSources": [{"key":"sub_...","name":"直播","url":"https://.../list.m3u","epg":"..."}],
  "stats": {"format":"libretv","skipped":0,"truncated":0}
}
```

## 10. 观看进度与收藏

第一版为**默认用户**（无账号体系）；接口需要登录，数据独立持久化（`userdata.db`，不走缓存 TTL）。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| PUT | `/api/me/progress` | 保存/覆盖当前观看进度（同一 `sourceKey+vodId` 只保留一条） |
| GET | `/api/me/history?unfinished=1&limit=50` | 观看历史；`unfinished=1` 只返回未看完（继续观看） |
| DELETE | `/api/me/history/{id}` | 删除历史；`id` = `sourceKey:vodId` |
| GET | `/api/me/favorites?limit=100` | 收藏列表 |
| PUT | `/api/me/favorites` | 收藏影片（保存标题/封面快照） |
| DELETE | `/api/me/favorites/{id}` | 取消收藏；`id` = `sourceKey:vodId` |

### PUT /api/me/progress

```json
{
  "sourceKey": "env_0",
  "sourceName": "暴风资源",
  "vodId": "12345",
  "title": "庆余年",
  "cover": "https://...",
  "lineId": "l1",
  "lineName": "ffm3u8",
  "episodeId": "l1e3",
  "episodeName": "第03集",
  "episodeIndex": 3,
  "position": 120.5,
  "duration": 2700,
  "completed": false
}
```

- `sourceKey` / `vodId` / `episodeId` 必填；`position`、`duration` 单位为秒，由播放器上报；
  `duration <= 0` 表示未知。
- 返回保存后的记录，包含服务端生成的 `id`（`sourceKey:vodId`）与 `updatedAt`（Unix 毫秒）。
- 服务端不推断“已看完”：`completed` 由客户端给出。

### GET /api/me/history

```json
{"items":[{"id":"env_0:12345","sourceKey":"env_0","vodId":"12345","episodeId":"l1e3","position":120.5,"duration":2700,"completed":false,"updatedAt":1760000000000}]}
```

按 `updatedAt` 倒序。`unfinished=1` 过滤：`completed=false` 且 `position>0` 且（时长未知或未到 95%）。

### GET /api/me/favorites

```json
{"items":[{"id":"env_0:12345","sourceKey":"env_0","vodId":"12345","title":"庆余年","cover":"https://...","createdAt":1760000000000,"updatedAt":1760000000000}]}
```

### 客户端流程（Fladder）

1. 详情页从 `playLines` 渲染线路/分集；播放时不使用顶层 `episodes`。
2. 播放中按节流（如每 5–10s、暂停/退出时）调用 `PUT /api/me/progress`，带当前 `episodeId`。
3. 首页「继续观看」用 `GET /api/me/history?unfinished=1`；点击后**重新请求** `/api/detail` 拿最新
   `playLines`，再按 `episodeId` 定位分集播放（不要直接使用历史里的 URL）。
4. 收藏按钮调用 `PUT/DELETE /api/me/favorites`，快照用于无源时展示。
5. 换源复用 `/api/search` + `/api/detail`；自动下一集由客户端按当前线路的 `episodes` 完成。

## 11. 暂未实现（返回 501）

| 接口 | 计划 |
| --- | --- |
| `POST /api/publish` | 发布源列表到第三方粘贴板（低优先级） |

## 12. 兼容性与版本

- 路径保持 `/api/...`，与 LibreTV 前端大体兼容；新增字段只增不改。
- 破坏性变更走 `/api/v2` 或新增字段 + 客户端适配。
- 服务端能力通过 `/api/status.capabilities` 暴露，客户端按能力降级。
