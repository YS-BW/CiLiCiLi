import type { SourceRef } from '../api/types';
import { normalizeTitle } from './sourceMatch';

// 记录「某部影片有哪些采集源」，供详情页做「换源」。
// 用 sessionStorage 按归一化标题存，避免「庆余年 第一季」和「庆余年第一季」互相找不到。
const KEY = 'cilicili.srcByTitle';

function readAll(): Record<string, SourceRef[]> {
  try { return JSON.parse(sessionStorage.getItem(KEY) || '{}'); } catch { return {}; }
}
function writeAll(m: Record<string, SourceRef[]>) {
  try { sessionStorage.setItem(KEY, JSON.stringify(m)); } catch { /* ignore */ }
}

export function rememberSources(title: string, sources: SourceRef[] | undefined) {
  if (!title || !sources || sources.length === 0) return;
  const key = normalizeTitle(title);
  if (!key) return;
  const all = readAll();
  all[key] = sources;
  writeAll(all);
}

export function getSources(title: string): SourceRef[] {
  if (!title) return [];
  const all = readAll();
  // 兼容旧版本以原始标题为 key 写入的缓存。
  return all[normalizeTitle(title)] || all[title] || [];
}
