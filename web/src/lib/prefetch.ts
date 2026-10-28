// 大封面预加载：列表卡片渲染小封面时，顺带把该片大封面(big=1)加载进浏览器缓存，
// 点进详情页时大图已就绪、秒显示。用队列限并发，避免一次打爆 TMDB/带宽。
const seen = new Set<string>();
const queue: string[] = [];
let active = 0;
const MAX_CONCURRENCY = 1;

function pump() {
  while (active < MAX_CONCURRENCY && queue.length > 0) {
    const url = queue.shift()!;
    active++;
    const img = new Image();
    const done = () => { active--; pump(); };
    img.onload = done;
    img.onerror = done;
    img.src = url;
  }
}

/** 预加载 URL（去重 + 限并发）。 */
export function prefetchImage(url: string) {
  if (!url || seen.has(url)) return;
  seen.add(url);
  queue.push(url);
  pump();
}
