# 全内嵌方案：把前后端一起打进 APK

结论：**技术上完全可行，已实测跑通**。但要做成产品级，需要解决 4 个 Android 工程问题，
并且会失去 3 个能力（一起看、多设备同步、远程访问）。

**推荐不是二选一，而是「双模式」**：同一个后端二进制，既能内嵌进 APK，也能跑在 VPS 上。

---

## 0. 实测结果（不是推演）

```
$ CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -buildvcs=false ...
real  0m6.530s
-rwxr-xr-x  15M  /tmp/cilicili-android-arm64
file: ELF 64-bit LSB pie executable, ARM aarch64, interpreter /system/bin/linker64
```

推到 Android 模拟器（arm64）的 `/data/local/tmp` 直接执行，日志：

```json
{"msg":"media library enabled","db":"/data/local/tmp/data/media.db","tmdb":false}
{"msg":"cilici-core starting","version":"android-test","listen":"127.0.0.1:8099","sources":19}
{"msg":"source probe finished","total":19,"alive":1,"dead":18,"durationMs":1367}
```

接口全部正常：

| 检查项 | 结果 |
| --- | --- |
| `GET /api/health` | `{"ok":true,"version":"android-test"}` |
| `POST /api/auth` | 返回 82 字符 token |
| `GET /api/sources` | 19 个源 |
| `POST /api/search` | **真实返回数据**（`jianan` 源返回了实际影片） |
| SQLite（modernc 纯 Go） | `media.db` / `media.db-wal` 正常创建 |
| bbolt | `cache.db` / `health.db` / `sources.db` 正常 |

**为什么能这么顺**：后端是**零 cgo** 的（`go list -deps` 里没有任何 `runtime/cgo`），
`modernc.org/sqlite` 是纯 Go 实现，bbolt 也是纯 Go。唯一的平台相关调用是
`signal.Notify(SIGINT, SIGTERM)`，Android 上照常工作。

---

## 1. 收益（比直觉更大）

| 收益 | 说明 |
| --- | --- |
| **彻底摆脱 VPS 带宽** | 手机直连 CDN，2.86 Mbps 不再是天花板，1080p/更高码率随便看 |
| **自签证书问题消失** | `http://127.0.0.1` 没有证书、没有 TOFU、没有 `onReceivedSslError` |
| **Service Worker 突然能用** | `127.0.0.1` 是安全上下文 → 封面缓存不用再写原生 `ImageCache`（那个 200 行可以删） |
| **地区封锁缓解** | 手机在国内运营商网络下，比境外出口更可能被 CDN 放行 |
| **延迟骤降** | 搜索/详情/封面都在本机，省掉每一跳网络往返 |
| **零运维** | 不用管 VPS 续费、证书过期、部署脚本 |

**复用资产是最大价值**：20+ 源适配、CMS 解析、TMDB、封面服务、SSRF 防护、熔断、缓存层
—— 全部原样复用。如果改成 Android 原生实现（`android/` 里那套 Kotlin），等于把这些重写一遍。

---

## 2. 必须解决的 4 个工程问题

### ① exec 限制（Android 10+）

app 数据目录里的文件**不允许执行**（W^X）。标准解法：

```
app/src/main/jniLibs/arm64-v8a/libcilicili.so   ← 其实就是那个 Go 二进制改个名
```

系统会把它解压到 `context.getApplicationInfo().nativeLibraryDir`，那个目录是可执行的。
（Termux 系、内嵌 Node 的 App 都用这个办法。）

### ② 后台保活

Go 服务必须跑在**前台服务**里（`FOREGROUND_SERVICE` + 常驻通知），否则 Doze / 后台限制会杀。

国产 ROM（MIUI / EMUI / ColorOS / OriginOS）更激进，前台服务也可能被杀。需要：
- `START_STICKY` + 启动时自愈检查
- 引导用户加「电池优化白名单」
- 应用回到前台时检查端口是否还活着，不活就重启

**这是整个方案最大的不确定性，必须在真机上验证，模拟器测不出来。**

### ③ 架构与体积

- 至少 `arm64-v8a`（覆盖 2020 年后的机器）
- 要兼容老设备再加 `armeabi-v7a`（二进制 ×2，APK 再大 ~15MB）
- 体积：Go 二进制 15MB + Web 资源 7MB ≈ **APK 22–25MB**（现在 6.7MB）

### ④ 首次启动

- 二进制解压 + 建库 + 源探活，首次启动有几秒延迟 → 需要启动页
- 端口选择：固定一个高位端口（如 47821）还是动态？动态更安全，但要处理 WebView 与服务的端口同步

---

## 3. 会失去什么（必须承认）

| 失去 | 影响 |
| --- | --- |
| **一起看** | 需要共享服务器。除非做 P2P（复杂度和 NAT 问题都不划算） |
| **多设备同步** | 历史/收藏只在这台手机，换设备就没了 |
| **远程访问** | 手机不在身边（比如在客厅）就看不了 |
| **后端热更新** | 改后端 bug 要重新发 APK，不能像现在这样推个镜像 |
| **Google Play 上架** | 禁止下载/执行代码，内嵌二进制会被拒 → 只能侧载 |

---

## 4. 推荐方案：双模式，而不是二选一

关键洞察：**后端还是同一个二进制**，只是打包方式不同。所以：

```
APK 启动
  ├─ 本机模式：启动内嵌 Go 服务 → WebView 指向 http://127.0.0.1:<port>
  └─ 远程模式：不启动内嵌服务 → WebView 指向 https://<VPS>
```

App 里**已经有「服务器地址」设置了**（`serverUrl()` / 设置面板里的「服务器 → 修改」），
只需要加一个「本机模式」的选项，逻辑分支很小。

这样：
- 单人快速看片 → 本机模式（无带宽限制、无证书、无运维）
- 一起看 / 多设备 / 远程 → 远程模式

代价只是多一个启动分支和一个设置项。

---

## 5. 工作量

| 阶段 | 内容 | 估计 |
| --- | --- | --- |
| P0 | Go 编 `android/arm64` + jniLibs 打包 + 首次解压 | 1 天 |
| P1 | 前台服务 + 通知 + 生命周期 + 自愈 | 1 天 |
| P2 | WebView 指向 127.0.0.1，去掉证书/TOFU 相关代码 | 0.5 天 |
| P3 | 数据目录规划（`filesDir`）+ 首次初始化 + 启动页 | 0.5 天 |
| P4 | 双模式开关 + 设置项 | 0.5 天 |
| P5 | **真机测试（国产 ROM 保活）** | 1–2 天 |
| 合计 | | **4–6 天** |

---

## 6. 和另外几个功能的关系

| 功能 | 内嵌后的变化 |
| --- | --- |
| **下载** | 更容易：分片走本机 127.0.0.1，没有 CORS，也没有 Referer 缺失问题（后端代理会补） |
| **超分/补帧** | 不受影响（本地后处理） |
| **DLNA** | 更容易：本地代理已经在跑，电视从手机 LAN 地址拉流 |
| **一起看** | **只能走远程模式**（除非做 P2P） |
| **封面缓存** | 原生 `ImageCache` 可以删掉，SW 或后端缓存都能用 |

---

## 7. 需要拍板

1. **要不要做双模式**（我强烈建议做，否则一起看就没法用）
2. **只支持 arm64 还是要兼容 32 位老机器**
3. **能不能接受 22–25MB 的 APK**
4. **能不能接受侧载**（不上架应用商店）
5. **有没有真机可以反复测保活**（这是唯一模拟器测不出来的部分）
