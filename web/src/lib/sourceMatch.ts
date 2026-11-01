import type { SearchResponse, SourceRef } from '../api/types';

// 标题归一化：忽略大小写、空白与常见标点，便于把「庆余年 第一季」和「庆余年第一季」视为同片。
export function normalizeTitle(input: string | undefined | null): string {
  return (input || '')
    .toLowerCase()
    .replace(/第[一1]季/g, '第1季')
    .replace(/[·:：\-_()（）【】[\]\s]/g, '');
}

// 末尾的季/部/版本限定词，例如「 第一季」「 Season 2」「 S2」「 特别版」「 剧场版」。
const TRAILING_QUALIFIER = /\s+(?:第[0-9一二三四五六七八九十百千两零〇]+[季部]|season\s*\d+|s\d+|特别版|剧场版|特别篇|番外篇?|电影版|完整版|加长版|导演剪辑版|国语版|粤语版|双语版)\s*$/i;

// 搜索关键词：先用完整标题，找不到时再退回首标题（去掉季/部/版本等后缀或第一个空格后的部分）。
export function searchKeywords(title: string | undefined | null): string[] {
  const full = (title || '').trim();
  if (!full) return [];

  let base = full;
  while (TRAILING_QUALIFIER.test(base)) {
    const next = base.replace(TRAILING_QUALIFIER, '').trim();
    if (!next || next === base) break;
    base = next;
  }

  // 没有可剥离的限定词时，尝试第一个空格前的部分（前缀至少 2 个字符）。
  if (base === full) {
    const idx = full.search(/\s/);
    if (idx > 0) {
      const prefix = full.slice(0, idx).trim();
      if (prefix.length >= 2) base = prefix;
    }
  }

  return base && base !== full ? [full, base] : [full];
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
