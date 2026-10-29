// 注册封面缓存 Service Worker（见 public/sw.js）。
//
// 目的：Android WebView 的 HTTP 磁盘缓存不可靠，封面每次冷启动都会重新下载。
// SW + Cache Storage 把封面存到 app_webview 里，第二次打开直接命中。
// 浏览器里同样生效，重复访问不再走网络。
export function registerCoverCache(): void {
  if (typeof navigator === 'undefined' || !('serviceWorker' in navigator)) return;
  // Android 壳里由原生 ImageCache 接管（自签证书下 SW 的网络栈注册必然失败），不再白试
  if (typeof window !== 'undefined' && (window as any).AndroidBridge) return;
  // file:// 等非 https/localhost 环境注册会直接失败，静默跳过
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch(() => { /* 忽略 */ });
  }, { once: true });
}
