// 原生外壳（Android WebView APK）注入的桥接对象。浏览器里为 undefined。
export interface CiliAndroidBridge {
  serverUrl(): string;
  appVersion(): string;
  isApp(): boolean;
  openServerSettings(): void;
}

declare global {
  interface Window {
    AndroidBridge?: CiliAndroidBridge;
  }
}

export function androidBridge(): CiliAndroidBridge | undefined {
  if (typeof window === 'undefined') return undefined;
  const b = window.AndroidBridge;
  return b && typeof b.serverUrl === 'function' ? b : undefined;
}

export function isNativeApp(): boolean {
  return !!androidBridge();
}

/** 当前站点地址；浏览器里返回当前 origin。 */
export function siteOrigin(): string {
  const b = androidBridge();
  if (b) {
    try { return b.serverUrl(); } catch { /* ignore */ }
  }
  return typeof window === 'undefined' ? '' : window.location.origin;
}
