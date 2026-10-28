import type { SourceRef } from '../api/types';

// 记录「某部影片有哪些采集源」，供详情页做「换源」。
// 用 sessionStorage 按标题存（标题是聚合卡片的稳定展示名，跨页面复用）。
const KEY = 'cilicili.srcByTitle';

function readAll(): Record<string, SourceRef[]> {
  try { return JSON.parse(sessionStorage.getItem(KEY) || '{}'); } catch { return {}; }
}
function writeAll(m: Record<string, SourceRef[]>) {
  try { sessionStorage.setItem(KEY, JSON.stringify(m)); } catch { /* ignore */ }
}

export function rememberSources(title: string, sources: SourceRef[] | undefined) {
  if (!title || !sources || sources.length === 0) return;
  const all = readAll();
  all[title] = sources;
  writeAll(all);
}

export function getSources(title: string): SourceRef[] {
  if (!title) return [];
  return readAll()[title] || [];
}
