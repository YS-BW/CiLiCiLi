import type { SearchResponse, SourceRef } from '../api/types';

// 标题归一化：忽略大小写、空白与常见标点，便于把「庆余年 第一季」和「庆余年第一季」视为同片。
export function normalizeTitle(input: string | undefined | null): string {
  return (input || '')
    .toLowerCase()
    .replace(/第[一1]季/g, '第1季')
    .replace(/[·:：\-_()（）【】[\]\s]/g, '');
}

function yearMatches(a: string | undefined, b: string | undefined): boolean {
  const ya = (a || '').replace(/\D/g, '');
  const yb = (b || '').replace(/\D/g, '');
  return !ya || !yb || ya === yb;
}

// 按 sourceKey+vodId 去重，保留先出现的条目。
export function mergeSources(...lists: (SourceRef[] | null | undefined)[]): SourceRef[] {
  const out: SourceRef[] = [];
  const seen = new Set<string>();
  for (const list of lists) {
    if (!list) continue;
    for (const s of list) {
      if (!s || !s.sourceKey || !s.vodId) continue;
      const key = `${s.sourceKey}:${s.vodId}`;
      if (seen.has(key)) continue;
      seen.add(key);
      out.push(s);
    }
  }
  return out;
}

// 只收集标题（必要时年份）匹配的搜索结果；优先当前源所在的聚合条目。
export function collectMatchingSources(
  resp: SearchResponse | null | undefined,
  title: string,
  year: string | undefined,
  currentSourceKey: string,
  currentVodId: string,
): SourceRef[] {
  const wanted = normalizeTitle(title);
  if (!wanted || !resp) return [];

  const media = (resp.media || []).filter(m =>
    normalizeTitle(m.title) === wanted && yearMatches(year, m.year)
  );
  const preferred = media.filter(m =>
    (m.sources || []).some(s => s.sourceKey === currentSourceKey && s.vodId === currentVodId)
  );
  const rest = media.filter(m => !preferred.includes(m));

  const out: SourceRef[] = [];
  for (const m of [...preferred, ...rest]) {
    for (const s of m.sources || []) out.push(s);
  }

  for (const item of resp.list || []) {
    if (normalizeTitle(item.name) !== wanted || !yearMatches(year, item.year)) continue;
    out.push({
      sourceKey: item.sourceKey,
      sourceName: item.sourceName,
      vodId: item.vodId,
      name: item.name,
      pic: item.pic,
      typeName: item.typeName,
      year: item.year,
      area: item.area,
      remarks: item.remarks,
    });
  }
  return out;
}
