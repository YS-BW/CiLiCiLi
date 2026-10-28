// 与后端 docs/API.md 对齐的类型定义（只列前端用到的字段）。

export interface HealthResponse { ok: boolean; version: string; uptimeMs: number }

export interface SourceConfig { key: string; name: string; url: string; detail?: string }

export interface AuthStatus {
  passwordRequired: boolean;
  verified: boolean;
  version: string;
  defaultSources?: SourceConfig[];
  capabilities?: string[];
}

export interface AuthResponse { success: boolean; token: string; expiresAt: number; verified?: boolean; error?: string }

export interface SearchResultItem {
  sourceKey: string; sourceName: string; vodId: string; name: string;
  pic: string; typeName: string; year: string; area: string; remarks: string; sourceUrl: string;
}

export interface SearchFailure { sourceKey: string; error: string; timedOut: boolean }

export interface SourceRef {
  sourceKey: string; sourceName: string; vodId: string; name: string;
  pic?: string; typeName?: string; year?: string; area?: string; remarks?: string;
}

export interface SearchMedia {
  id: string; kind: string; title: string; year?: string;
  cover?: string; backdrop?: string; typeName?: string; remarks?: string;
  rating?: string;
  sourceCount: number; matchStatus: string; sources: SourceRef[];
}

export interface SearchResponse { list: SearchResultItem[]; failures: SearchFailure[]; media?: SearchMedia[] }

// 流式事件
export type StreamEvent =
  | { type: 'source'; sourceKey: string; ok: boolean; ms: number; list: SearchResultItem[]; error?: string; timedOut?: boolean }
  | { type: 'partial_done'; list: SearchResultItem[]; failures: SearchFailure[]; partial: true }
  | { type: 'done'; list: SearchResultItem[]; failures: SearchFailure[]; media?: SearchMedia[] };

export interface Episode { id: string; name: string; index: number; url: string }
export interface PlayLine { id: string; name: string; index: number; preferred: boolean; episodes: Episode[] }
export interface VideoInfo {
  title: string; cover: string; desc: string; typeName: string; year: string; area: string;
  director: string; actor: string; remarks: string; sourceKey: string; sourceName: string; sourceUrl: string;
}
export interface VideoDetail {
  episodes: string[];
  playLines: PlayLine[];
  videoInfo: VideoInfo;
}

export interface RecommendItem { id: string; title: string; cover?: string; pic?: string; score?: string; type?: string; year?: string }

export interface WatchProgress {
  id: string; sourceKey: string; vodId: string; title?: string; cover?: string;
  episodeId?: string; episodeName?: string; episodeIndex?: number;
  position: number; duration: number; completed: boolean; updatedAt: number;
}

export interface FavoriteItem {
  id: string; sourceKey: string; vodId: string; title: string; cover?: string;
  createdAt: number; updatedAt: number;
}

export interface SourceAdminItem {
  key: string; name: string; url: string; enabled: boolean;
  sortOrder?: number; isAdult?: boolean; createdAt?: number; updatedAt?: number;
}
export interface SourceHealth {
  key: string; ewmaMs?: number; failStreak?: number; level?: number; openUntil?: number; disabled?: boolean;
}
export interface SourcesAdminResponse { sources: SourceAdminItem[]; health: SourceHealth[] }
