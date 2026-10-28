// 前端本地设置（localStorage）。
export type SearchMode = 'fast' | 'full';

const KEY_SEARCH_MODE = 'cilicili.searchMode';

export function getSearchMode(): SearchMode {
  try { return (localStorage.getItem(KEY_SEARCH_MODE) as SearchMode) || 'fast'; } catch { return 'fast'; }
}
export function setSearchMode(m: SearchMode) {
  try { localStorage.setItem(KEY_SEARCH_MODE, m); } catch { /* ignore */ }
}
