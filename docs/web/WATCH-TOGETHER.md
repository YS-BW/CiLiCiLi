# 一起看（Watch Together）设计方案

目标：多人同步看同一部片（B 站「一起看」），以及抖音式的短视频共同浏览。
本文只讲设计和取舍，不含实现。

---

## 0. 先划三条红线

这三条决定了方案长什么样，绕不过去：

1. **VPS 只有约 2.86 Mbps 上行**。任何「服务端转发视频 / 服务端合流 / 服务端做语音中转」的方案直接排除。
   所有客户端各自直连同一个 CDN 的同一路 m3u8，同步的是**时间轴**，不是码流。
2. **一部片有 N 个采集源，同一集在不同源上时长可能不同**（贴片广告、片头、剪辑差异都见过）。
   房间必须**锁死 `sourceKey + vodId + lineId + episodeId`**，跟随者绝不自己重新搜索。
3. **APK 是 WebView 壳，WebView 的 HTTP 磁盘缓存不可用**（见 `docs/web/ANDROID-APK.md`）。
   所以短视频流的「下一条预取」拿不到原生体验，要提前接受 300–800ms 起播。

---

## 1. 架构：服务端权威时间轴（不做房主广播）

```
             ┌──────────────────────────────┐
  控制命令 → │  Room (服务端单一权威)        │ → 广播 state / presence / chat
             │  timeline = {pos, playing,    │
             │              rate, atServerMs} │
             └──────────────────────────────┘
                       ↑ 时钟对齐（offset）
             ┌─────────┴─────────┬──────────┐
          浏览器               浏览器       APK(WebView)
             各自对接 CDN 的同一路 m3u8，各自缓冲
```

**为什么不是「房主广播自己的进度」**（很多人第一反应是这个）：

- 房主网络抖动 → 全员跟着抖
- 房主退出 → 要选主、要迁移，状态机复杂度翻倍
- 房主自己缓冲时，别人的进度基准就是错的

服务端只存**一条时间轴**，谁按了播放都只是「向服务器提交一个控制命令」，服务器更新
`timeline.atServerMs = now` 后广播新 `seq`。所有客户端（包括刚按的那个人）都以同一条轴为准。
好处：无选主、掉线无副作用、后进房间的人天然对齐。

房主仍然有意义，但只是**权限角色**（能不能控进度、能不能换源），不是时钟来源。

### 消息协议（JSON over WebSocket）

客户端 → 服务端：

```jsonc
{"t":"join","nickname":"阿黎","token":"..."}          // token 用于重连认领坐位
{"t":"control","seq":12,"action":"play|pause|seek|rate|next|prev|goto","posMs":123456,"rate":1.0,"cursor":3}
{"t":"chat","text":"哈哈","posMs":123456}              // 弹幕/聊天合一，posMs 让后来者能按位置回放
{"t":"buffering","on":true}
{"t":"ping","cts":84123}                               // 单调时钟，非 Date.now()
```

服务端 → 客户端：

```jsonc
{"t":"welcome","you":{...},"room":{...},"members":[...],"chat":[...]}
{"t":"state","seq":13,"posMs":123456,"playing":true,"rate":1,"atServerMs":...,"by":"m_3f2a"}
{"t":"presence","members":[{"id","nickname","color","online","buffering"}]}
{"t":"chat","msg":{...}}
{"t":"pong","cts":84123,"sts":1700000000123}
{"t":"error","code":"seat_full|rate_limited|source_dead"}
```

`seq` 单调递增，客户端丢弃比本地旧的（防乱序）；`control` 带 `seq` 是为了做**乐观更新**：
本地点下去立刻反应，服务器回来的 `state` 若 seq 更新则覆盖。

### 时钟对齐（这是精度的根）

不要用 `Date.now()` 做基准——用户改系统时间、时区切换、NTP 跳变都会让同步崩掉。
用**单调时钟** `performance.now()`：

```
对 join / 每 30s：
  连打 5 次 ping，取 RTT 最小的那次
  offset = sts - (cts + rtt/2)     // 单调时基
  serverNow() = performance.now() + offset
```

拿 5 次最小 RTT 样本，误差通常 < 20ms，对 500ms 的同步目标绰绰有余。

客户端每 1s 算一次：

```
target = playing ? posMs + (serverNow() - atServerMs) * rate : posMs
drift  = target - video.currentTime
```

### 漂移纠正策略（别硬 seek，会看到反复跳动）

| drift | 动作 |
| --- | --- |
| < 200ms | 不动 |
| 200–600ms | `playbackRate = 1 ± 0.04`，收敛后归 1（MSE 支持，看不出画面变化） |
| 600–2000ms | 连续 3 个采样都超阈值才动作，避免抖动误判 |
| > 2000ms | 硬 seek，并提示「已同步到主持人」 |

前置守卫：`video.readyState < 3`、正在 seek、用户在手动拖进度条时**跳过判定**，
否则会和用户抢控制权。手动拖动结束后 2s 内不自动纠正，改为显示「回到同步进度」按钮。

---

## 2. 本项目特有的两个坑（必须处理，否则功能是坏的）

### 2.1 贴片广告 → 内容时间偏移 `contentOffsetMs`

实测很多源正片前有 5–15s 贴片（"本片由 XX 赞助播出"）。A 从 0 秒开始、B 从 20 秒进来，
两人的 `video.currentTime` 天然差 20s，如果直接用 `currentTime` 做时间轴，全员永远对不上。

解法：**房间时间轴定义成「内容时间」**，另存一个房间级偏移：

```
video.currentTime = targetContentMs + contentOffsetMs
```

`contentOffsetMs` 的确定方式（按优先级）：

1. 房主拖到正片真正开始处，点「以此为起点」→ `contentOffsetMs = video.currentTime`
2. 房主手动填秒数
3. 默认 0（无广告的源不受影响）

换源 / 换线路时 `contentOffsetMs` 重置为 0 并要求房主重新标定（不同源广告长度不同）。

### 2.2 多源切换不打破时间轴

房主在房间里点「换源」→ 服务器只改 `media.sourceKey/lineId`，`timeline` 保持不动，
广播 `{"t":"reload","media":{...},"posMs":...}`；所有客户端用新地址重建播放器并 seek 到同一位置。
跟随者遇到源不可用（熔断/404）时房间进入 `degraded`，显示「当前线路不可用，等待房主换源」，
**不要**自作主张去搜别的源——那会直接破坏同步。

---

## 3. 缓冲协同（B 站那种「有人卡了全员暂停」）

客户端上报 `buffering`，服务端规则：

- 任意成员 buffering 持续 > 4000ms 且房间在播放 → 服务端 `pause`，广播
  `reason: "member_buffering", memberId`
- 30s 冷却，避免一个人网络差把房间锁死
- 房间设置里可关（`autoPauseOnBuffer`）

这是体验分水岭：没有它，弱网成员会永远落后并且不断被硬 seek 拉扯。

---

## 4. 抖音式：时间轴对象不同，内核复用

一起看同步的是「一集视频内的位置」；抖音式同步的是「队列里的第几条 + 位置」。
把 `timeline` 抽象成 `{cursor, posMs, playing}` 就统一了。

两种子模式（建议都做，成本很低）：

**A. 同步刷**：`cursor` 由服务端持有，任何人上滑都广播，其他人跟着跳。
适合「一起看个乐子」。

**B. 自由刷**：各自 `cursor` 本地，互不干扰；提供「喊一声」按钮，把自己的 cursor 广播出去，
其他人收到「[昵称] 让你看这个」+ 一键跟上。
这才是抖音式好玩的地方——一个人刷到好笑的，喊大家一起看。

预取要诚实说：**WebView 缓存坏了 + 每段都是独立 m3u8**，跨条预取只能靠隐藏的第二个 hls 实例
（内存代价大）或者干脆不做。**建议不做跨条预取**，改为：

- `maxBufferLength` 调小（如 10s）、`startLevel` 先起最低码率
- 起播 300–800ms，视觉上可接受

---

## 5. 明确不做（成本/收益不成比例）

| 不做 | 原因 |
| --- | --- |
| 服务端转发视频 / 合流 | 2.86 Mbps 上行，一路 1080p 就吃满 |
| WebRTC 视频通话 | 视频必须中转，2.86 Mbps 撑不住，排除 |
| 账号体系 | 现在是单密码鉴权，房间用 6 位邀请码 + 昵称即可 |
| 房间持久化 / 历史回放 | 房间是临时的，30min 空闲回收，存内存即可 |
| 服务端录制 | 无意义 |

如果确实要**语音（纯音频）**：其实可行，不是带宽问题——Opus 每路约 40 kbps，
5 人 ≈ 200 kbps，远低于 2.86 Mbps。真正的成本在 NAT 穿透（需要 TURN 兜底，同样走这台 VPS 的 UDP）。
自托管选 **LiveKit**（Apache-2.0），或 3 人以内直接 mesh P2P（只在你朋友之间，几乎不过服务器）。

---

## 6. 落地拆解

### 后端（新增 `internal/watchparty/`）

```
hub.go       房间注册表 + 房间 goroutine（actor 模式，单协程串行处理，无锁）
room.go      状态机：join/leave/control/chat/buffering/换源/回收
protocol.go  wire 结构体（和前端类型一一对应）
ws.go        升级、读写泵、心跳、per-client 有缓冲发送队列（慢客户端丢消息不阻塞）
*_test.go
```

- 传输选型：**WebSocket**，加一个依赖 `github.com/coder/websocket`（轻量、context 友好）。
  不想加依赖的话可以退成 `SSE(下行) + POST(上行)`，现有 nginx 已经 `proxy_buffering off`，
  但要加 `X-Accel-Buffering: no`，且控制命令多一次 RTT。**推荐 WS。**
- 路由：`GET /api/watch/ws`、`POST /api/watch/rooms`、`GET /api/watch/rooms/{code}`，
  复用现有 `guard()` cookie 鉴权。
- nginx **两处都要改**（这个项目有两个 nginx）：宿主 `/etc/nginx/conf.d/antigravity.conf` 和
  本地 `web/nginx.conf`，都要加 `proxy_set_header Upgrade/Connection` 并把
  `proxy_read_timeout` 拉长，否则 60s 后 WS 被掐。

### 前端

```
web/src/lib/watchparty.ts        WS 客户端 + 时钟对齐 + 漂移控制器（核心，~250 行）
web/src/pages/WatchTogether.tsx  创建/加入房间、房间页
web/src/components/ChatPanel.tsx 聊天 + 成员列表
web/src/components/Danmaku.tsx   弹幕层（按 posMs 回放最近 60s）
web/src/pages/Detail.tsx         加「一起看」按钮，带上当前 sourceKey/vodId/line/episode 建房
```

`Player.tsx` 已经通过 `art.on('video:timeupdate'/'video:pause')` 回调了 `onProgress`，
再加一个 `onReady(handle)` 把 `art` 实例暴露给同步控制器即可，不用改播放内核。

### APK

- JS 桥加 `setKeepScreenOn(bool)`：进房间保持常亮（一起看经常是几十分钟）
- `onPause` 上报 buffering/离开，`onResume` 强制重对齐（已有 `onPause/onResume` 钩子）
- 可选：进房间请求横屏，加 `setRequestedOrientation`
- **不需要新权限**

---

## 7. 验收标准

1. 三端（2 浏览器 + 1 APK）同看 10 分钟：`p95 |drift| < 800ms`，最大不超过 2s
2. 弱网（Chrome 限速 3G）成员加入后 30s 内收敛到同步，且**不出现 seek 风暴**（30s 内硬 seek < 3 次）
3. 服务端重启：客户端 5s 内自动重连并回到正确位置
4. 10 人房间：服务端 CPU 占用可忽略（纯内存广播），无消息积压
5. 换源后全员仍对齐（`contentOffsetMs` 重置逻辑正确）

---

## 8. 工作量估计

| 阶段 | 内容 | 估计 |
| --- | --- | --- |
| P0 | 房间 + WS + 时间轴 + 时钟对齐 + 漂移纠正 + 聊天/弹幕（单视频） | 后端 ~350 行 / 前端 ~500 行，1–1.5 天 |
| P1 | 缓冲协同、控制权限、断线重连、换源、APK 常亮/前后台 | ~0.5 天 |
| P2 | 抖音式 feed（同步刷 + 自由刷 + 喊一声） | ~0.5–1 天 |
| P3 | 语音（LiveKit 自托管：纯音频 / 3 人内 mesh P2P） | 另算，约 0.5 天 + 一个 UDP 端口放行 |

---

## 9. 需要拍板的三件事

1. **「抖音的那种」具体指哪个？**
   (a) 短视频流一起刷（推荐做，成本低）
   (b) 语音/视频连麦（受带宽限制，需外接）
   (c) 两个都要
2. **控制权模型**：所有人可拖进度（B 站默认，热闹但容易被恶搞）／仅房主可控（稳）／可切换（推荐默认仅房主 + 房主可开「自由控」）
3. **语音要不要**：不要 → 纯文字 + 弹幕；要 → 接第三方 RTC，我只做信令与房间同步

---

## 10. 开源方案调研（能不能直接拿来用）

结论先说：**现成的开源一起看项目全是「完整应用」，不是能嵌进 Go + React 的库**。
它们都自带一套前端和自己的媒体解析（YouTube / Vimeo / 直链 / mp4），
而我们这边是「20 多个采集源聚合 + 自签证书 + WebView 壳」，媒体层根本对不上。
换用它们 = 把现有 App 推翻重做，成本远高于自己写那 350 行同步逻辑。

| 项目 | 形态 | 能不能用 | 备注 |
| --- | --- | --- | --- |
| [OpenTogetherTube](https://github.com/dyc3/opentogethertube) | Node/TS 独立站，Docker 自托管 | ❌ 不能嵌入 | 支持 YouTube/Vimeo/mp4/HLS VOD/DASH，带 SponsorBlock。但媒体解析靠它自己的 extractor，[HLS 还要求时长元数据](https://github.com/dyc3/opentogethertube/issues/744)，接不了我们的采集源 |
| [CyTube](https://github.com/JacobJohansen/sync) | Node + MySQL 独立站 | ❌ 不能嵌入 | 频道制，主打 YouTube/Twitch 嵌入；自己的 UI 和权限体系 |
| [Syncplay](https://github.com/Syncplay/syncplay) | Python 桌面客户端 + 服务端 | ❌ 跑不了浏览器 | 但它**控制 mpv/VLC 的协议设计非常成熟（10 年+）**，是最值得抄的对象：状态机、迟到等待、漂移处理 |
| Jellyfin SyncPlay | Jellyfin 内置功能 | ❌ 绑定 Jellyfin 媒体库 | `SyncPlayGroupUpdate` + `SyncPlayCommand` 两条 WS 消息的设计很干净，[协议说明](https://github.com/streamyfin/streamyfin/blob/d22458bd2b5696849309c9c6281e153634e47dba/utils/syncplay/README.md)值得照抄；[OpenWatchParty](https://github.com/mhbxyz/OpenWatchParty) 就是基于它做的 |
| [SyncWatch](https://github.com/xuange6610/SyncWatch) | 自托管服务端 + 多端客户端（含 Android/Web） | ❌ 整套替换 | 最接近的国产方案：Apache-2.0，同步播放 + 聊天 + 语音 + 屏幕共享 + 内网穿透。但它是完整产品，接进来等于不要现在的 App |
| [LiveDRIVE](https://github.com/pheonix14/LiveDRIVE) | 网页版影院 | ❌ 不能嵌入 | 支持 HLS 直播流、亚秒级同步，项目较新、较小 |

### 值得「抄」而不是「引」的部分

1. **Syncplay 的漂移策略**：落后就请求全员等待、超前就自己追，而不是互相硬 seek。
   比我文档里 §1「漂移纠正」的纯 rate-nudge 更适合「有人网很慢」的场景，可以两者结合：
   `落后 > 5s 连续 3 次 → 发起「等我一下」（全员暂停）`，`超前 → 降 rate 追`。
2. **Jellyfin SyncPlay 的「全员就绪」语义**：`Ready/NotReady` 状态，
   所有人都 ready 才真正开播。我们现在的设计里没有这个，加入房间时自动开播，体验会散
   （有人在缓冲、有人还在系安全带）。建议 P0 就带上。
3. **SponsorBlock 的思路**：按「片源 + 时间区间」跳过片头/赞助段。
   我们对应的是 §2.1 的 `contentOffsetMs`，但可以扩展成「区间列表」，
   甚至可以做成一个共享的、按 `sourceKey + vodId` 索引的片段库（自建，不依赖 SB 的数据库）。

### 不值得引入的

- **Yjs / Automerge（CRDT）**：房间状态是「单点权威 + 单调 seq」，用 CRDT 是过度设计，
  还会引入几十 KB 和调试难度。
- **Colyseus / PartyKit / Socket.IO**：都是 Node 生态，我们是 Go；而且房间逻辑就这么点，
  加一层框架反而绕。`github.com/coder/websocket` 一个包足够。
- **mediasoup**：SFU，只解决视频分发，不解决同步，而且带宽不允许。
