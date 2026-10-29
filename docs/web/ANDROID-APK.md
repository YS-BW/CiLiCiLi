# CiLiCiLi Android APK（Web 客户端外壳）

不是「套一层网址」的浏览器壳，也不是把 Web 重写成原生。做法是**混合壳体**：

| 关注点 | 做法 |
| --- | --- |
| 页面本体（index.html / JS / CSS / 字体） | 全部打进 APK `assets/web/`，由 `shouldInterceptRequest` 从本地读，秒开、断网也能渲染骨架 |
| 数据（`/api/*`） | 不拦截，交给系统网络栈直连真实服务器 |
| 封面图片 | 原生 `ImageCache` 自建磁盘缓存（见下文「封面缓存」），不依赖 WebView 的 HTTP 缓存 |
| 站点 origin | 仍然是真实服务器地址（如 `https://124.221.121.195`），所以同源、Cookie 登录态、hls.js 拉分片、Range 视频请求全部原生可用，**没有 CORS 问题** |
| 自签证书 | TOFU：首次连接记住证书 SHA-256，指纹变化时弹窗让用户确认 |
| 视频全屏 | `WebChromeClient.onShowCustomView`，进入沉浸式全屏并保持常亮 |
| 服务器地址 | 首启默认 `https://124.221.121.195`；在 App 内「设置 → 服务器 → 修改」可改，也可以在浏览器里改（浏览器不显示该入口） |
| 登录态 | `onPause` 里 `CookieManager.flush()`，否则被系统回收会掉登录 |

## 目录

```
android-webview/            # 独立 Gradle 工程，与原生 Kotlin 客户端 android/ 互不影响
  app/src/main/java/com/cilicili/webview/
    MainActivity.java        # 外壳：WebView 配置、资源拦截、证书 TOFU、全屏、返回键、JS 桥
    ImageCache.java          # 封面图片磁盘缓存（含负缓存与自签证书 TLS）
  app/src/main/assets/web/   # 构建时从 web/dist 同步（不入库）
scripts/build-apk.sh         # npm run build → 同步 assets → gradle assembleRelease
```

## 下载（已部署到 VPS）

`https://124.221.121.195/app/cilicili-webapp.apk`（自签证书，浏览器会提示不安全，选择继续即可）

APK 放在 VPS 的 `/srv/cilicili-downloads/`，由宿主 Nginx 的 `location /app/` 提供，
**不进后端镜像**，换版本时直接覆盖文件即可。

## 构建

```sh
# 需要 JDK 17、Android SDK（platform 37 / build-tools 37）
./scripts/build-apk.sh              # release，产出 6.7MB APK
./scripts/build-apk.sh debug        # debug（包名带 .debug，可共存；开了 WebView 远程调试）
```

产物：`android-webview/app/build/outputs/apk/release/app-release.apk`

签名用仓库内 `android-webview/release.keystore`（store/key 口令均为 `cilicili`，alias `cilicili`）。
**这个 keystore 不要丢**，否则新包无法覆盖安装旧包（签名不一致）。换机器构建时把它一起带走。

包名 `com.cilicili.webapp`，debug 变体是 `com.cilicili.webapp.debug`。

## 状态栏 / 手势条适配

targetSdk 35+ 会强制 edge-to-edge，WebView 直接画到状态栏和手势条下面，顶栏「返回」会被电量图标盖住。

两个坑：

1. 内边距**必须加在 root（FrameLayout）上**，`webView.setPadding()` 不生效——
   WebView 内部的 `window.innerHeight` 仍然是整屏高度，顶栏照样压在状态栏下。
2. `setOnApplyWindowInsetsListener` 在内容视图上不一定触发，所以布局完成后还要用
   `root.getRootWindowInsets()` 兜一次。

全屏播放时系统栏隐藏、insets 归零，自动恢复满屏。

## 前端的配合

- `web/src/lib/native.ts`：检测 `window.AndroidBridge`，`isNativeApp()` / `siteOrigin()`。
- `web/src/components/Nav.tsx`：设置面板里，只有检测到桥接对象时才显示「服务器」区块。
- 原生侧注入的方法：`serverUrl()` / `setServer`（原生弹窗负责）、`appVersion()`、`isApp()`、`openServerSettings()`。

## 封面缓存（1.0.1 修的重要问题）

**现象**：每次冷启动，首页 16 张封面（约 1.3–5MB）全部重新下载，字体/CSS/JS 也一样。

**排查结论**：不是响应头问题（`public, max-age=2592000, immutable` + ETag 都对），也不是
`shouldInterceptRequest` 的锅（去掉拦截照样不缓存）。是 **Chromium 的 WebView 磁盘缓存后端初始化就失败了**：

```
E chromium: [ERROR:simple_version_upgrade.cc(151)] Failed to write a new fake index.
```

`pm clear` 之后依然复现，`Cache_Data` 里永远只有索引没有条目，`getCacheMode()` 是 `LOAD_DEFAULT`(-1)。
也就是说：**WebView 自带的 HTTP 缓存不能依赖**。

**试过但不行**：Service Worker + Cache Storage。方案本身对，但自签证书下 SW 自己的网络栈不走
`onReceivedSslError`，注册直接报 `An SSL certificate error occurred when fetching the script.`。

**最终做法**（`ImageCache.java`）：

- 在 `shouldInterceptRequest` 里拦「同源 + GET + `Accept` 含 `image/`」的 `/api/cover`、
  `/api/proxy`、`/api/media/*/cover`；`Accept` 判断保证不会误伤 `/api/proxy` 上的视频和 HLS 分片。
- **命中**：直接从 `filesDir/imgcache/` 读文件返回，不发网络请求。
- **未命中**：先返回 `null` 让 WebView 正常走网络（首屏速度不变），同时后台线程（并发 3）
  下载一份存盘，下次冷启动就命中。
- **负缓存**：404（TMDB 没有横版图时 `big=1` 会 404）记一个 `.miss` 文件，12 小时内不再重试。
- TLS 用自己的 `PinTrustManager`，和 WebView 的 TOFU 共用同一份 `pin:<host>` 指纹；域名校验跳过
  （自签证书签的是 IP）。
- 200MB 上限，按 mtime 做 LRU 淘汰。

实测：同一台设备冷启动，`/api/cover` 请求 **16 个 / 1.37MB → 0 个 / 0 字节**，
59 张图 57 张正常渲染、0 张破图。

浏览器端另有一份 `web/public/sw.js`（Service Worker + Cache Storage），在 App 里检测到
`AndroidBridge` 就跳过注册，避免白试一次。

## 验证要点

1. 首屏资源来自 assets：断网启动也能看到骨架与导航（接口报错是预期的）。
2. 登录一次后重启 App 仍是登录态（`CookieManager.flush()` 保证）。
3. 播放：`<video>` 能拿到 1920×1080、`currentTime` 连续推进、buffer 增长。
4. 设置面板显示当前服务器地址，改完自动重载。
5. 自签证书首次弹「信任」后不再打扰；换证书会重新询问。
6. **封面缓存**：第一次进首页会下载封面；之后杀进程重开，`nginx access.log` 里
   `/api/cover` 应为 0 条，且封面正常显示。

## 为什么不用 Capacitor

Capacitor 的 `CapacitorHttp` 会把响应整体缓冲成 base64，视频分片会被拖死；而把资源放 `capacitor://` 再跨域拉 `/api` 又要处理 CORS 和 Cookie 域。
本地资产 + 真实 origin 的方案把这三个问题一次绕开，代码量也更小（无第三方依赖，纯 framework API）。
