import type {
  AuthStatus, AuthResponse, SearchResponse, StreamEvent, VideoDetail,
  RecommendItem, WatchProgress, FavoriteItem, SearchMedia,
} from './types';

// 同源部署：所有请求走相对路径 /api，cookie 由浏览器自动携带（httpOnly cilici_session）。
// 未登录返回 401，调用方统一处理跳转登录。

export class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) { super(message); this.status = status; }
}

const BASE = ''; // 同源

async function parseError(resp: Response): Promise<ApiError> {
  let msg = `请求失败 (HTTP ${resp.status})`;
  try {
    const body = await resp.json();
    if (body?.error) msg = body.error;
  } catch { /* ignore */ }
  if (resp.status === 401) msg = msg === `请求失败 (HTTP 401)` ? '未登录' : msg;
  if (resp.status === 503) msg = '服务器未配置 PASSWORD (503)';
  return new ApiError(msg, resp.status);
}

async function jsonFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(BASE + path, {
    credentials: 'include',
    headers: { 'Content-Type': 'application/json', ...(init?.headers || {}) },
    ...init,
  });
  if (!resp.ok) throw await parseError(resp);
  const text = await resp.text();
  if (!text) return undefined as unknown as T;
  return JSON.parse(text) as T;
}

export const api = {
  health: () => fetch(BASE + '/api/health', { credentials: 'include' }).then(r => r.json()),

  status: () => jsonFetch<AuthStatus>('/api/status'),

  login: (password: string) =>
    jsonFetch<AuthResponse>('/api/auth', { method: 'POST', body: JSON.stringify({ password }) }),

  logout: () => jsonFetch<{ success: boolean }>('/api/auth', { method: 'DELETE' }),

  authState: () => jsonFetch<{ success: boolean; verified: boolean }>('/api/auth'),

  async search(keyword: string, opts?: { fast?: boolean; filterAdult?: boolean }): Promise<SearchResponse> {
    const resp = await jsonFetch<SearchResponse>('/api/search', {
      method: 'POST',
      body: JSON.stringify({ wd: keyword.trim(), fast: opts?.fast ?? true, filterAdult: opts?.filterAdult ?? true, includeMedia: true }),
    });
    if ((!resp.media || resp.media.length === 0) && resp.list.length > 0) {
      resp.media = aggregateItemsLocally(resp.list);
    }
    return resp;
  },

  // 流式搜索：NDJSON 逐行解析；onEvent 实时回调。
  async searchStream(
    keyword: string,
    onEvent: (e: StreamEvent) => void,
    signal?: AbortSignal,
  ): Promise<void> {
    const resp = await fetch(BASE + '/api/search?stream=1', {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ wd: keyword.trim(), fast: true, filterAdult: true, includeMedia: true }),
      signal,
    });
    if (!resp.ok) throw await parseError(resp);
    if (!resp.body) throw new ApiError('空流响应', 0);

    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let idx: number;
      while ((idx = buffer.indexOf('\n')) >= 0) {
        const line = buffer.slice(0, idx).trim();
        buffer = buffer.slice(idx + 1);
        if (!line) continue;
        try { onEvent(JSON.parse(line) as StreamEvent); } catch { /* skip bad line */ }
      }
    }
    const tail = buffer.trim();
    if (tail) { try { onEvent(JSON.parse(tail) as StreamEvent); } catch { /* ignore */ } }
  },

  detail: (sourceKey: string, vodId: string) => {
    const q = new URLSearchParams({ id: vodId, sourceKey });
    return jsonFetch<VideoDetail>(`/api/detail?${q.toString()}`);
  },

  douban: (type: 'movie' | 'tv' = 'movie', tag = '热门', pageStart = 0, pageSize = 25) =>
    jsonFetch<{ items: RecommendItem[] }>(
      `/api/douban?type=${type}&tag=${encodeURIComponent(tag)}&pageStart=${pageStart}&pageSize=${pageSize}`)
      .then(r => r.items || []),

  hotList: (id: string) =>
    jsonFetch<{ items: RecommendItem[] }>(`/api/hot-list?id=${encodeURIComponent(id)}`).then(r => r.items || []),

  // 按标题查 TMDB 横版剧照（首页主推大图用）
  backdrop: (title: string, type: 'movie' | 'tv' = 'movie', year = '') =>
    jsonFetch<{ backdrop: string }>(`/api/backdrop?title=${encodeURIComponent(title)}&type=${type}${year ? `&year=${encodeURIComponent(year)}` : ''}`)
      .then(r => r.backdrop || '').catch(() => ''),

  history: (unfinished = false, limit = 50) =>
    jsonFetch<{ items: WatchProgress[] }>(
      `/api/me/history?unfinished=${unfinished ? 1 : 0}&limit=${limit}`).then(r => r.items || []),

  saveProgress: (payload: Record<string, unknown>) =>
    jsonFetch<WatchProgress>('/api/me/progress', { method: 'PUT', body: JSON.stringify(payload) }),

  favorites: (limit = 100) =>
    jsonFetch<{ items: FavoriteItem[] }>(`/api/me/favorites?limit=${limit}`).then(r => r.items || []),

  addFavorite: (p: { sourceKey: string; sourceName: string; vodId: string; title: string; cover: string }) =>
    jsonFetch<{ success: boolean }>('/api/me/favorites', { method: 'PUT', body: JSON.stringify(p) }),

  removeFavorite: (id: string) =>
    jsonFetch<{ success: boolean }>(`/api/me/favorites/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  getAggregate(items: SearchResponse['list']): SearchMedia[] {
    return aggregateItemsLocally(items);
  },

  // 所有外部图片统一走同源 /api/proxy：
  // - 规避采集源站/豆瓣/TMDB 的跨域与防盗链；
  // - 后端对图片统一设 public,max-age=30天(+ETag)，浏览器稳定命中缓存，不再每次重拉；
  // - 同源请求，无 CORS 问题。
  proxyUrl(raw: string): string {
    const t = (raw || '').trim();
    if (!t) return '';
    if (t.startsWith('/')) return t;                 // 已是站内地址（如 /api/media/xxx/cover）
    if (t.startsWith('data:')) return t;
    return `/api/proxy?url=${encodeURIComponent(t)}`;
  },

  // 豆瓣海报升级到高清：s_ratio_poster(小) / m(中) → raw(原图 1080)。仅用于首屏主推大图。
  hiResCover(raw: string): string {
    const t = (raw || '').trim();
    if (!t) return '';
    if (t.includes('doubanio.com')) {
      // 统一替换尺寸段为 raw；l/m/s_ratio_poster 都可能是原 id
      return t.replace(/\/(s|l|m|xl|r)_ratio_poster\//, '/raw/');
    }
    return t;
  },
};

const TITLE_RE = /[\s·•:：,，.。!！?？()（）\[\]【】《》<>"']+/g;

function normTitle(raw: string): string {
  return (raw || '').trim().toLowerCase().replace(new RegExp(`(第二季|第2季|season \\d+|s\\d{1,2})`, 'gi'), '')
    .replace(TITLE_RE, '');
}

function inferKind(typeName: string, remarks: string): string {
  const c = `${typeName} ${remarks}`;
  if (/动漫|番剧|动画/.test(c)) return 'anime';
  if (/剧|连续|集|综艺/.test(c)) return 'tv';
  if (/片|电影|正片/.test(c)) return 'movie';
  return 'tv';
}

export function aggregateItemsLocally(items: SearchResponse['list']): SearchMedia[] {
  if (!items.length) return [];
  const groups = new Map<string, SearchResponse['list']>();
  for (const it of items) {
    const nt = normTitle(it.name);
    const year = (it.year || '').trim();
    const kind = inferKind(it.typeName, it.remarks);
    const key = (year.length === 4 && kind !== 'other' && nt) ? `${kind}:${nt}:${year}` : `src:${it.sourceKey}:${it.vodId}`;
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key)!.push(it);
  }
  const out: SearchMedia[] = [];
  for (const [key, g] of groups) {
    const first = g[0];
    const coverItem = g.find(x => x.pic) || first;
    out.push({
      id: key.startsWith('src:') ? key : `local_${hash16(key)}`,
      kind: inferKind(first.typeName, first.remarks),
      title: first.name,
      year: first.year,
      cover: coverItem.pic,
      typeName: first.typeName,
      remarks: first.remarks,
      sourceCount: g.length,
      matchStatus: 'pending',
      sources: g.map(x => ({
        sourceKey: x.sourceKey, sourceName: x.sourceName, vodId: x.vodId, name: x.name,
        pic: x.pic, typeName: x.typeName, year: x.year, area: x.area, remarks: x.remarks,
      })),
    });
  }
  return out;
}

function hash16(s: string): string {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (Math.imul(31, h) + s.charCodeAt(i)) | 0;
  return (h >>> 0).toString(16);
}
