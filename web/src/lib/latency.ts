import { api } from '../api/client';
import type { Episode, LatencyProbeItem, SourceLatencyResult } from '../api/types';

// 前端也做一层短缓存/并发合并：详情页和一起看都会探测同一批片源，
// 避免切页或 React StrictMode 重复请求后端。
const CACHE_TTL_MS = 30_000;
const cache = new Map<string, { value: SourceLatencyResult; at: number }>();
const inflight = new Map<string, Promise<Record<string, SourceLatencyResult>>>();

function cacheKey(item: LatencyProbeItem): string {
  return `${item.key}|${item.url || ''}|${item.sourceKey || ''}|${item.vodId || ''}`;
}

export function sourceLatencyKey(sourceKey: string, vodId: string): string {
  return `source:${sourceKey}:${vodId}`;
}

export function lineLatencyKey(sourceKey: string, vodId: string, lineId: string): string {
  return `line:${sourceKey}:${vodId}:${lineId}`;
}

// 优先测 m3u8，拿不到再退回第一条播放地址。
export function pickPlaylistURL(episodes: Episode[] | undefined): string {
  const list = episodes || [];
  const m3u8 = list.find(ep => /\.m3u8(\?|#|$)/i.test(ep.url || ''));
  return (m3u8 || list[0])?.url || '';
}

export async function probeLatency(items: LatencyProbeItem[]): Promise<Record<string, SourceLatencyResult>> {
  const result: Record<string, SourceLatencyResult> = {};
  const missing: LatencyProbeItem[] = [];
  const seen = new Set<string>();
  const now = Date.now();

  for (const item of items) {
    if (!item.key || seen.has(item.key)) continue;
    seen.add(item.key);
    const hit = cache.get(cacheKey(item));
    if (hit && now - hit.at < CACHE_TTL_MS) {
      result[item.key] = hit.value;
    } else {
      missing.push(item);
    }
  }
  if (missing.length === 0) return result;

  const batchKey = missing.map(cacheKey).sort().join('||');
  let pending = inflight.get(batchKey);
  if (!pending) {
    pending = api.latency(missing)
      .then(results => {
        const byKey = new Map(results.map(res => [res.key, res]));
        const fetched: Record<string, SourceLatencyResult> = {};
        for (const item of missing) {
          const value = byKey.get(item.key) || { key: item.key, ok: false, error: '探测失败' };
          cache.set(cacheKey(item), { value, at: Date.now() });
          fetched[item.key] = value;
        }
        return fetched;
      })
      .catch(() => {
        const failed: Record<string, SourceLatencyResult> = {};
        for (const item of missing) failed[item.key] = { key: item.key, ok: false, error: '探测失败' };
        return failed;
      })
      .finally(() => { inflight.delete(batchKey); });
    inflight.set(batchKey, pending);
  }

  const fetched = await pending;
  return { ...result, ...fetched };
}

export function latencyLabel(result: SourceLatencyResult | undefined): string {
  if (!result) return '测速中…';
  if (result.timedOut) return '超时';
  if (!result.ok) return '失败';
  const ms = result.ms ?? result.ttfbMs ?? 0;
  return `${Math.max(0, Math.round(ms))}ms`;
}

export function latencyTitle(result: SourceLatencyResult | undefined): string {
  if (!result) return '正在测量播放列表延迟';
  if (result.timedOut) return '探测超时';
  if (!result.ok) return result.error ? `探测失败：${result.error}` : '探测失败';
  const ttfb = result.ttfbMs ?? 0;
  const total = result.ms ?? ttfb;
  return `首字节 ${Math.round(ttfb)}ms · 完成 ${Math.round(total)}ms`;
}

export function latencyClass(result: SourceLatencyResult | undefined): string {
  if (!result) return 'pending';
  if (result.timedOut || !result.ok) return 'bad';
  return (result.ms ?? result.ttfbMs ?? 0) > 1500 ? 'slow' : 'good';
}