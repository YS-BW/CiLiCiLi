# CiLiCiLi Android APK（Web 客户端外壳）

不是「套一层网址」的浏览器壳，也不是把 Web 重写成原生。做法是**混合壳体**：

| 关注点 | 做法 |
| --- | --- |
| 页面本体（index.html / JS / CSS / 字体） | 全部打进 APK `assets/web/`，由 `shouldInterceptRequest` 从本地读，秒开、断网也能渲染骨架 |
| 数据（`/api/*`） | 不拦截，交给系统网络栈直连真实服务器 |
| 站点 origin | 仍然是真实服务器地址（如 `https://124.221.121.195`），所以同源、Cookie 登录态、hls.js 拉分片、Range 视频请求全部原生可用，**没有 CORS 问题** |
| 自签证书 | TOFU：首次连接记住证书 SHA-256，指纹变化时弹窗让用户确认 |
| 视频全屏 | `WebChromeClient.onShowCustomView`，进入沉浸式全屏并保持常亮 |
| 服务器地址 | 首启默认 `https://124.221.121.195`；在 App 内「设置 → 服务器 → 修改」可改，也可以在浏览器里改（浏览器不显示该入口） |

## 目录

```
android-webview/            # 独立 Gradle 工程，与原生 Kotlin 客户端 android/ 互不影响
  app/src/main/java/com/cilicili/webview/MainActivity.java   # 全部逻辑，约 470 行
  app/src/main/assets/web/  # 构建时从 web/dist 同步（不入库）
scripts/build-apk.sh        # npm run build → 同步 assets → gradle assembleRelease
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

## 验证要点

1. 首屏资源来自 assets：断网启动也能看到骨架与导航（接口报错是预期的）。
2. 登录一次后重启 App 仍是登录态（Cookie 由 WebView 持久化）。
3. 播放：`<video>` 能拿到 1920×1080、`currentTime` 连续推进、buffer 增长。
4. 设置面板显示当前服务器地址，改完自动重载。
5. 自签证书首次弹「信任」后不再打扰；换证书会重新询问。

## 为什么不用 Capacitor

Capacitor 的 `CapacitorHttp` 会把响应整体缓冲成 base64，视频分片会被拖死；而把资源放 `capacitor://` 再跨域拉 `/api` 又要处理 CORS 和 Cookie 域。
本地资产 + 真实 origin 的方案把这三个问题一次绕开，代码量也更小（无第三方依赖，纯 framework API）。
