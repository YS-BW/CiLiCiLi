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
  "fast": true,
  "includeMedia": false
}
```

- `sources` 可省略/为空：使用服务端配置的全部源。
- `filterAdult` 默认 `true`。
- `fast` 默认 `true`：服务端按健康度（熔断等级 → EWMA 延迟）只查最快的 `SEARCH_FAST_SOURCES`（默认 8）个源；传 `false` 查全部源。响应头 `X-Search-Sources: 8/18` 表示实际参与/请求的源数。
- 快速模式是首选：首源通常 < 300 ms，软截止 1.2 s，完成通常 1.2–2.5 s；全量模式用于“搜索全部源”。
- `includeMedia` 默认 `false`：为 `true` 时，非流式响应额外返回 `media` 数组（聚合后的媒体卡片，一张影片只出现一次）；
  流式响应的 `done` 事件会带 `media`。卡片结构见 [媒体聚合](§12)。
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
      "id": "ln:ffm3u8",
      "name": "ffm3u8",
      "index": 1,
      "preferred": true,
      "episodes": [
        {"id":"e:第01集","name":"第01集","index":1,"url":"https://cdn.test/1/index.m3u8"},
        {"id":"l1e2","name":"第02集","index":2,"url":"https://cdn.test/2/index.m3u8"}
      ]
    },
    {
      "id": "ln:llm3u8",
      "name": "llm3u8",
      "index": 2,
      "preferred": false,
      "episodes": [
        {"id":"e:正片","name":"正片","index":1,"url":"https://cdn2.test/full.mp4"}
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
- `episodes[].id`：来源内「名称优先」的稳定标识，格式 `e:<原始集名>`（如 `e:第03集`）。
  **不是播放 URL**；顺序变化（插入集数/调换线路）不会改变它。无集名时退化为 `e:@<序号>`
  （如 `e:@3`）；同一线路内重名追加 `#2`（如 `e:正片#2`）。
- `playLines[].id`：线路稳定标识，格式 `ln:<线路名>`（如 `ln:ffm3u8`）；同名线路追加 `#2`。
- `episodes[].index`：在该线路内的 1-based 顺序。
- `episodes[].url`：播放地址，可能随时间失效；继续播放前应重新请求本接口。
- 顶层 `episodes` 是旧字段（优先 m3u8 线路的扁平 URL 列表），仅用于兼容旧客户端；
  新客户端请使用 `playLines`。

#### 标识与绑定规则

- 影片身份：`sourceKey + vodId`（同一作品在不同源下是两条记录，第一版不做跨源合并）。
- 分集身份：优先 `episodeId`（名称型，如 `e:第03集`），与 URL 解耦；跨线路同名集 ID 相同，
  便于换线续播。客户端定位顺序应为：**episodeId → episodeName → episodeIndex**；
  线路定位：**lineId → lineName → preferred 线路**。
- 豆瓣推荐卡片的 `id` 属于推荐域，**不是**采集站 `vodId`；点击推荐后应先用标题调用
  `/api/search`，由用户选择源与影片。
- 未知字段一律为空值，服务端不推测季数、时长、剧情或缩略图。

状态码：`400` 参数无效；`404` 无播放资源；`502` 上游失败。

## 4. 源管理

需要登录。源配置持久化在 `SOURCES_DB`（默认 `$DATA_DIR/sources.db`，SQLite）：

- 首次启动时若库为空，从 `SOURCES_FILE` 导入一次，之后以库为准；
- 增删改/排序/启停**立即热替换** Registry，不需要重启；禁用源不参与搜索；
- `key` 由服务端生成（`src_<8hex>`，基于 URL），也可在创建时指定。

### GET /api/sources

返回全部源（含禁用）与服务端健康快照：

```json
{
  "sources":[
    {"key":"env_0","name":"暴风资源","url":"https://...","enabled":true,"sortOrder":0,"createdAt":1792827000000,"updatedAt":1792827000000}
  ],
  "health":[
    {"key":"env_0","ewmaMs":812,"failStreak":0,"level":0,"openUntil":0,"disabled":false,"successes":12,"failures":0}
  ]
}
```

健康语义：连续 2 次失败 → 30 分钟熔断；再犯 → 24 小时；再犯 → 长期停用（`disabled:true`，需管理接口重置）。熔断中的源不参与搜索，且不等待。

### POST /api/sources

```json
{"name":"新源","url":"https://example.com/api.php/provide/vod","key":"可选","detail":"可选","isAdult":false,"enabled":true}
```

→ `{"source":{...}}`。`400` 参数/地址非法；`409` key 或地址重复。

### PUT /api/sources/{key}

```json
{"name":"改名","url":"https://...","enabled":false}
```

字段均可选，只更新传入项；→ `{"source":{...}}`。`404` 源不存在。

### DELETE /api/sources/{key}

→ `{"success":true}`；`404` 源不存在。

### POST /api/sources/reorder

```json
{"keys":["env_0","src_ab12cd34"]}
```

按给定顺序重排，未列出的源保持原顺序排在后面；→ `{"sources":[...]}`。

### POST /api/sources/reset

清除单个源的熔断/长期停用状态。

```json
{"key":"env_0"}
```

### GET /api/sources/probe / POST /api/sources/probe

源探活（服务端定期静默执行，默认每 12 小时）。

`GET` 返回状态：

```json
{
  "intervalHours": 12,
  "running": false,
  "last": {
    "startedAt": 1792828700000,
    "finishedAt": 1792828710000,
    "total": 23,
    "alive": 16,
    "dead": 7,
    "items": [{"key":"env_0","name":"暴风资源","url":"https://...","ok":true,"ms":831,"count":20}]
  },
  "discover": {"intervalHours":168,"lastAt":1792828734986,"lastAdded":5,"running":false}
}
```

`POST` 修改间隔或立即触发：

```json
{"intervalHours": 6}
{"run": true}
```

- 探活失败会走现有熔断（连续 2 次 → 30 分钟 → 24 小时 → 长期停用）；
- **只有存活（未熔断）的源会真正参与搜索**：搜索前按健康状态过滤，死源不会计入 `X-Search-Sources`；
- 探活间隔持久化在 `SOURCES_DB`，1–168 小时。

### 自动发现新源（服务端）

服务端每隔 **7 天**（`discover_interval_days`，默认 7，可改库/环境变量）自动执行一次：

1. 抓取内置发现渠道（可用 `DISCOVER_SOURCES` 覆盖，逗号分隔）；
   默认走 jsDelivr 镜像（`raw.githubusercontent.com` 在国内/VPS 不可达）：
   - `https://cdn.jsdelivr.net/gh/muzheya/tvbox-config@main/tvbox-diy.json`（纯 API 采集源）
   - `https://cdn.jsdelivr.net/gh/YH-Music/vod-sources@main/vod.b64`（苹果CMS 接口清单）
2. 解析出候选源，与现有源按 URL 去重；
3. 并发探活，**只自动写入存活的新源**（每轮最多 20 个），并热替换 Registry；
4. 用户删除过的源会进入黑名单（`blocked_sources`），自动发现不会再把它加回来；手动重新添加会解除黑名单。

无需前端参与；启动时若距上次发现超过间隔会立即跑一次。

### POST /api/sources/test

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
- 未登录：仅放行豆瓣图片域（精确后缀白名单）。
- 透传 `Range`/`If-*`，支持 `HEAD`；分片流式转发。
- m3u8 会重写内部的 variant/分片/key/map 地址，指向本代理。
- 返回 `Access-Control-Allow-Origin: *`。
- 豆瓣图床对部分请求返回 `200 + text/html` 反爬挑战页（Go TLS 指纹触发）。代理检测到
  「豆瓣图片路径 + 非 image 响应」时，改用镜像 `doubanio.viki.moe` 再取一次；挑战页与
  其他 HTML 响应一律 `Cache-Control: no-store`，避免一次失败被浏览器缓存住。
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
| `GET /api/hot-list?id=<id>` | 影视榜单；1 小时缓存；未知 id 返回 400。可用 ids：`douban_movie_weekly` / `douban_tv_chinese` / `douban_tv_global` / `douban_show_chinese` / `douban_show_global` / `baidu_teleplay` |

## 9. 源订阅

订阅地址持久化在 `SOURCES_DB` 的 `subscriptions` 表；支持 LibreTV-SourceList JSON、TVBox 配置、接口目录。

### GET /api/subscriptions

→ `{"items":[{"id":1,"name":"订阅名","url":"https://...","enabled":true,"lastFetchAt":...,"lastStatus":"ok","lastError":"","lastCount":12}]}`

### POST /api/subscriptions

```json
{"name":"可选","url":"https://.../sources.json"}
```

→ `{"subscription":{...}}`。`409` 地址重复。

### PUT /api/subscriptions/{id}

```json
{"name":"改名","url":"https://...","enabled":false}
```

字段均可选。

### DELETE /api/subscriptions/{id}

→ `{"success":true}`。

### POST /api/subscriptions/{id}/refresh

拉取并解析订阅，**只返回预览不写入源**，同时记录 `lastStatus/lastCount`：

```json
{
  "subscription":{...},
  "preview":{
    "name":"订阅名",
    "sources":[{"key":"sub_ab12cd34","name":"源名","url":"https://..."}],
    "liveSources":[...],
    "stats":{"format":"libretv","skipped":0,"truncated":0}
  }
}
```

拉取失败返回 `502`，并把订阅标记为 `lastStatus=failed` + `lastError`。

### POST /api/subscriptions/{id}/apply

把订阅里的点播源合并进源列表（按规范化 URL 去重）：

```json
{"sources":[{"name":"源名","url":"https://..."}]}
```

`sources` 省略时服务端重新拉取整份订阅。→ `{"added":2,"skipped":1,"items":[...]}`。

### GET /api/source-list?url=<encoded>

无状态预览（不保存订阅）：拉取远程订阅并返回解析结果，结构同 `preview`。仍保留用于临时查看。

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
  "lineId": "ln:ffm3u8",
  "lineName": "ffm3u8",
  "episodeId": "e:第03集",
  "episodeName": "第03集",
  "episodeIndex": 3,
  "position": 120.5,
  "duration": 2700,
  "completed": false
}
```

- `sourceKey` / `vodId` / `episodeId` 必填；`position`、`duration` 单位为**秒**（前端播放器需按此上报，
  不要传毫秒），由播放器上报；
  `duration <= 0` 表示未知。
- 返回保存后的记录，包含服务端生成的 `id`（`sourceKey:vodId`）与 `updatedAt`（Unix 毫秒）。
- 服务端不推断“已看完”：`completed` 由客户端给出。

### GET /api/me/history

```json
{"items":[{"id":"env_0:12345","sourceKey":"env_0","vodId":"12345","episodeId":"e:第03集","position":120.5,"duration":2700,"completed":false,"updatedAt":1760000000000}]}
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
   若采集站改名/调整导致 `episodeId` 找不到：先按 `episodeName` 精确匹配（优先同线路），
   再按 `episodeIndex` 回退；线路同理（`lineId` → `lineName` → `preferred`）。
   兼容旧版 `l1e3` 形式的进度：同样走这套回退。
4. 收藏按钮调用 `PUT/DELETE /api/me/favorites`，快照用于无源时展示。
5. 换源复用 `/api/search` + `/api/detail`；自动下一集由客户端按当前线路的 `episodes` 完成。

## 11. 暂未实现（返回 501）

| 接口 | 计划 |
| --- | --- |
| `POST /api/publish` | 发布源列表到第三方粘贴板（低优先级） |

## 12. 媒体聚合与元数据搜刮

目标：前端只接收统一的 `mediaId`、封面、简介、演职员和匹配状态，不需要知道数据来自采集站还是第三方源。
采集站原始条目仍然保留（`sourceKey + vodId`），永远可回查。

### 12.1 聚合规则

- 聚合键：`类型 + 归一化标题 + 年份（+ 季数）`，`mediaId` 形如 `media_<16hex>`。
- 标题归一化：去掉季数后缀（`第二季` / `第2季` / `Season 2` / `S02` / `庆余年2`）、尾部年份、标点与空白。
- **缺年份不聚合**，类型无法判断（`other`）也不聚合；这类条目在搜索结果里用 `src:<sourceKey>:<vodId>` 单独成卡。
- 同一作品在不同源下聚合为一张卡；`sourceCount` 表示来源数量，`sources[]` 保留每个源的 `sourceKey/vodId`。
- 第一版不做跨类型（电影/剧集）合并；标题相同但年份/季数不同视为不同媒体。

### 12.2 状态字段

| 字段 | 取值 | 含义 |
| --- | --- | --- |
| `metadataStatus` | `idle` / `queued` / `scraping` / `done` / `failed` | 第三方元数据刮削状态；失败会退避重试（5m、10m… 上限 6h） |
| `matchStatus` | `pending` / `suggested` / `confirmed` / `rejected` | 匹配状态；`suggested` 表示有候选但未确认 |

自动确认默认关闭（`MEDIA_AUTO_CONFIRM=0`）：只有标题归一化完全一致、年份一致、综合分 ≥0.85 才会自动确认；
否则只产生候选，等待人工确认。

### 12.3 接口

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| POST | `/api/media/resolve` | 批量把采集站条目聚合成卡片；或按 `sourceKey+vodId` 查单个媒体 |
| GET | `/api/media/{id}` | 媒体实体（元数据、来源、候选、封面、状态） |
| GET | `/api/media/{id}/metadata-candidates?refresh=1` | 候选列表；`refresh=1` 重新排队刮削 |
| POST | `/api/media/{id}/match` | 人工确认/拒绝/清除匹配 |
| POST | `/api/media/{id}/cover` | 人工设置封面 URL（服务端下载校验并缓存） |
| GET | `/api/media/{id}/cover` | **无需登录**，返回本地缓存封面图片；没有缓存时 404 |
| POST | `/api/media/{id}/refresh-metadata` | 重新排队刮削（202） |

`POST /api/media/resolve` 两种用法：

```json
{"items":[{"sourceKey":"env_0","sourceName":"暴风资源","vodId":"12345","name":"庆余年","typeName":"国产剧","year":"2019","pic":"https://..."}]}
```

→ `{"items":[SearchMedia...]}`（最多 100 条）。

```json
{"sourceKey":"env_0","vodId":"12345"}
```

→ 单个 `Media`，找不到返回 404。

> **横版图（backdrop）**：TMDB 有 `backdrop_path` 时，`Media`、`Media.metadata`、`SearchMedia` 都会带
> `backdrop` 字段（`https://image.tmdb.org/t/p/w1280/...`，横版 16:9），供 Web 首页主推大图 / 详情页背景使用。
> 字段缺失或为空表示该片暂无横版图，前端应回退到竖版 `cover` 或本地占位。该字段只增不改，旧客户端忽略即可。
> 注意：backdrop 目前是**直连 TMDB 图片 CDN 的 URL**（未像封面那样落盘本地缓存），拿不到时前端回退 `cover`。

`GET /api/media/{id}` 响应（节选）：

```json
{
  "id": "media_cfb94fbb2e1b3b08",
  "kind": "tv",
  "title": "庆余年第二季",
  "year": "2024",
  "metadata": {
    "title": "庆余年第二季", "year": "2024", "overview": "", "cover": "https://...",
    "backdrop": "https://image.tmdb.org/t/p/w1280/xxx.jpg",
    "director": "", "actors": "", "area": "", "genres": "",
    "source": "source", "confidence": 0
  },
  "sources": [{"sourceKey":"360zy","sourceName":"360采集","vodId":"57767","name":"庆余年第二季","pic":"https://..."}],
  "sourceCount": 12,
  "backdrop": "https://image.tmdb.org/t/p/w1280/xxx.jpg",
  "cover": {
    "url": "/api/media/media_cfb94fbb2e1b3b08/cover",
    "localPath": "/var/lib/cilicili/covers/media_cfb94fbb2e1b3b08.jpg",
    "hash": "96baae47187b08859e111ef9c69a32fe",
    "width": 270, "height": 404,
    "source": "bdzy", "reachable": true
  },
  "candidates": [],
  "matchStatus": "suggested",
  "metadataStatus": "done",
  "updatedAt": 1760000000000
}
```

`POST /api/media/{id}/match`：

```json
{"action":"confirm","provider":"<provider>","externalId":"<id>"}
{"action":"reject"}
{"action":"clear"}
```

`POST /api/media/{id}/cover`：

```json
{"url":"https://example.com/poster.jpg"}
```

- 只接受 http(s)，服务端下载并校验 `image/*`、≤8 MiB、解码宽高后落盘；失败返回 400/502。
- 封面本地文件按 `mediaId` 命名，保存 `hash/宽高/来源`；前端不依赖外部图片域名。

### 12.4 封面优先级与缓存

1. **人工设置**（`POST /api/media/{id}/cover`）——显式覆盖，只有它高于 TMDB；
2. **TMDB 候选封面**（候选按分数排序，取第一个能下载成功的）；
3. **采集源 pic**（同一媒体的多个源按 `sourceKey` 顺序尝试）；
4. 都没有时 `/cover` 返回 404，前端显示本地占位图。

- 确认候选（`POST /match`）时会用对应 TMDB 封面替换非人工封面；`refresh-metadata` 会清掉旧封面按上述顺序重选。
- 下载后落盘 `MEDIA_COVER_DIR`，SQLite 只存路径/哈希/宽高/来源。
- **前端缓存**：`media[].cover` 是 `/api/media/{id}/cover?v=<hash>`；`hash` 变化时 URL 变化，
  服务端返回 `Cache-Control: public, max-age=2592000, immutable` + `ETag`，浏览器直接命中本地缓存
  （刷新/重进都不再请求 VPS）；同一会话内 Flutter 还有内存图片缓存。`If-None-Match` 命中返回 `304`。
- 搜索卡片 `media[].cover`：本地已缓存时返回上述本地地址，否则返回源站 `pic`；
  异步刮削完成后再次搜索即可拿到本地封面。

### 12.5 Provider 与降级

| Provider | 覆盖 | 配置 |
| --- | --- | --- |
| TMDB | 电影 / 剧集 / 动漫（动漫按 tv 查） | `TMDB_API_KEY` 或 `TMDB_READ_TOKEN`（任一）；`TMDB_API_BASE` 默认 `https://api.tmdb.org` |

- 未配置凭据时 TMDB 自动禁用（`Enabled=false`），流程不等待、不失败；聚合与封面缓存照常工作。
- 国内网络下 `api.themoviedb.org` 会被 DNS 污染，默认用官方别名 `https://api.tmdb.org`；
  图片走 `https://image.tmdb.org`，实测可达。
- 用 v4 读访问令牌时请求带 `Authorization: Bearer <token>`，不把 token 放进 URL。
- 搜索只传 `query` + `language=zh-CN` + `include_adult=false`，不传 year（采集站年份常不准，传了会漏召回）；
  年份只在本地评分里使用。
- 某个 provider 失败不影响其他 provider；全部失败才把任务标为 `failed` 并退避重试（最多 3 次）。
- 免费 API 需遵守 TMDB 条款：应用内注明 “This product uses the TMDB API but is not endorsed or certified by TMDB.”，
  且不得用于商业用途。

### 12.6 存储

- SQLite + WAL：`media.db`，表 `media`、`source_items`、`metadata_candidates`、`cover_cache`、`scrape_jobs`。
- 图片文件在 `MEDIA_COVER_DIR`（默认 `DATA_DIR/covers`），SQLite 只存路径/哈希/来源。
- `media.db` 必须与 `cache.db` / `userdata.db` 分开（启动时校验）。
- 原始上游 JSON 缓存仍在 bbolt；收藏/进度仍在 `userdata.db`，本次未迁移。

### 12.7 前端（Fladder）接入建议

1. 搜索请求带 `"includeMedia": true`，直接渲染 `media[]`；`list` 仅作旧兼容。
2. 卡片图片用 `media[].cover`；加载失败时回退占位图，不必再猜第二张源图。
3. 点击卡片进入详情前，可用 `POST /api/media/resolve` 或直接 `GET /api/media/{id}` 拿统一元数据。
4. 播放仍走 `GET /api/detail`（`sourceKey/vodId` 从 `media.sources[]` 选）；媒体库不替代播放解析。
5. 管理页可展示 `candidates`，调用 `match` 人工确认；封面可手工粘贴 URL。

## 13. 兼容性与版本

- 路径保持 `/api/...`，与 LibreTV 前端大体兼容；新增字段只增不改。
- 破坏性变更走 `/api/v2` 或新增字段 + 客户端适配。
- 服务端能力通过 `/api/status.capabilities` 暴露，客户端按能力降级。
