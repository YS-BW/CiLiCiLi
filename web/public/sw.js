// 封面图片缓存（Service Worker）。
//
// 为什么要自己缓存：Android WebView 的 HTTP 磁盘缓存并不可靠——实测在部分设备/系统上
// Chromium 的 SimpleBackend 初始化失败（logcat: simple_version_upgrade "Failed to write
// a new fake index"），此后所有请求都回源，即使响应头是 immutable。Cache Storage 走的是
// app_webview 里的另一套存储，不受这个问题影响。
//
// 只拦「同源 + GET + 图片」的请求：
//   - <img> 和 CSS background-image 的 request.destination === 'image'
//   - 视频（destination='video'）、hls.js 的分片（destination=''）、NDJSON 搜索流都不动
const CACHE = 'covers-v1';
const MAX_ENTRIES = 600;

self.addEventListener('install', () => self.skipWaiting());

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    const names = await caches.keys();
    await Promise.all(names.filter((n) => n !== CACHE).map((n) => caches.delete(n)));
    await self.clients.claim();
  })());
});

function isCacheable(request, url) {
  if (request.method !== 'GET') return false;
  if (url.origin !== self.location.origin) return false;
  if (request.destination !== 'image') return false;
  // 只缓存封面类接口，其它接口即使被当成图片请求也不碰
  return url.pathname === '/api/cover' || url.pathname === '/api/proxy'
    || url.pathname.startsWith('/api/media/');
}

async function trim(cache) {
  const keys = await cache.keys();
  if (keys.length <= MAX_ENTRIES) return;
  // 先进先出：Cache Storage 没有时间戳，按插入顺序淘汰最老的
  for (const key of keys.slice(0, keys.length - MAX_ENTRIES)) {
    await cache.delete(key);
  }
}

self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url);
  if (!isCacheable(event.request, url)) return; // 交给浏览器默认处理

  event.respondWith((async () => {
    const cache = await caches.open(CACHE);
    const hit = await cache.match(event.request, { ignoreVary: true });
    if (hit) return hit;
    try {
      const resp = await fetch(event.request);
      // 只存成功且是图片的响应（404 之类不存，下次还会再试）
      if (resp.ok && (resp.headers.get('content-type') || '').startsWith('image/')) {
        await cache.put(event.request, resp.clone());
        trim(cache);
      }
      return resp;
    } catch (err) {
      // 离线时别让图片请求直接抛错，返回一个透明占位
      return new Response('', { status: 504, statusText: 'offline' });
    }
  })());
});
