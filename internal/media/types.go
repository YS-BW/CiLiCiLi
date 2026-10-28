// Package media 把多个采集站条目聚合成唯一媒体实体，并为它匹配/缓存第三方元数据。
//
// 设计边界：
//   - 采集站条目（sourceKey + vodId）是“源引用”，永远保留；
//   - 聚合键 = 类型 + 归一化标题 + 年份（+ 季数限定词），缺年份时默认不合并；
//   - 第三方元数据只做补充，不覆盖采集源已有的有效标题/简介/演职员；
//   - 所有联网刮削都在后台队列执行，页面只读已存结果；
//   - 封面一律由服务端下载、校验、落盘，前端只请求 /api/media/{id}/cover。
package media

// Kind 是媒体类型。
type Kind string

// 支持的类型。
const (
	KindMovie Kind = "movie"
	KindTV    Kind = "tv"
	KindAnime Kind = "anime"
	KindOther Kind = "other"
)

// MatchStatus 是第三方匹配状态。
type MatchStatus string

// 匹配状态。
const (
	MatchPending   MatchStatus = "pending"
	MatchSuggested MatchStatus = "suggested"
	MatchConfirmed MatchStatus = "confirmed"
	MatchRejected  MatchStatus = "rejected"
)

// MetadataStatus 是元数据刮削状态。
type MetadataStatus string

// 刮削状态。
const (
	MetaIdle     MetadataStatus = "idle"
	MetaQueued   MetadataStatus = "queued"
	MetaScraping MetadataStatus = "scraping"
	MetaDone     MetadataStatus = "done"
	MetaFailed   MetadataStatus = "failed"
)

// SourceRef 是采集站里的一条原始条目。
type SourceRef struct {
	SourceKey  string `json:"sourceKey"`
	SourceName string `json:"sourceName"`
	VodID      string `json:"vodId"`
	Name       string `json:"name"`
	Pic        string `json:"pic,omitempty"`
	TypeName   string `json:"typeName,omitempty"`
	Year       string `json:"year,omitempty"`
	Area       string `json:"area,omitempty"`
	Remarks    string `json:"remarks,omitempty"`
}

// Metadata 是聚合后的展示元数据，并标明每个字段的来源。
type Metadata struct {
	Title    string `json:"title"`
	Year     string `json:"year,omitempty"`
	Overview string `json:"overview,omitempty"`
	Cover    string `json:"cover,omitempty"`
	// Backdrop 横版剧照 URL（来自第三方元数据），Web 端主推/详情背景用；可能为空。
	Backdrop string `json:"backdrop,omitempty"`
	Director string `json:"director,omitempty"`
	Actors   string `json:"actors,omitempty"`
	Area     string `json:"area,omitempty"`
	Genres   string `json:"genres,omitempty"`
	// Source 标明元数据主体来源：manual / source / bangumi / none。
	Source string `json:"source"`
	// Confidence 仅第三方匹配时有意义。
	Confidence float64 `json:"confidence,omitempty"`
}

// Candidate 是一个第三方元数据候选（未确认）。
type Candidate struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"` // bangumi
	ExternalID string `json:"externalId"`
	Title      string `json:"title"`
	Original   string `json:"originalTitle,omitempty"`
	Year       string `json:"year,omitempty"`
	Kind       Kind   `json:"kind"`
	Overview   string `json:"overview,omitempty"`
	CoverURL   string `json:"coverUrl,omitempty"`
	// BackdropURL 是第三方（TMDB）返回的横版剧照（16:9），用于 Web 主推/详情背景大图；
	// 为空表示该候选没有横版图，前端回退竖版封面或占位。
	BackdropURL string  `json:"backdropUrl,omitempty"`
	Score       float64 `json:"score"`
	// Reasons 记录打分依据，便于人工判断（标题/年份/类型命中情况）。
	Reasons []string `json:"reasons,omitempty"`
	// Selected 表示当前绑定到该媒体的匹配。
	Selected bool `json:"selected"`
}

// CoverInfo 是本地封面缓存信息。
type CoverInfo struct {
	URL       string `json:"url"`
	LocalPath string `json:"localPath,omitempty"`
	Hash      string `json:"hash,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Source    string `json:"source,omitempty"`
	// Reachable 表示最近一次校验结果；未校验时为 nil。
	Reachable *bool `json:"reachable,omitempty"`
	UpdatedAt int64 `json:"updatedAt,omitempty"`
}

// Media 是对外返回的聚合实体。
type Media struct {
	ID          string      `json:"id"`
	Kind        Kind        `json:"kind"`
	Title       string      `json:"title"`
	Year        string      `json:"year,omitempty"`
	Metadata    Metadata    `json:"metadata"`
	Sources     []SourceRef `json:"sources"`
	SourceCount int         `json:"sourceCount"`
	Cover       CoverInfo   `json:"cover"`
	// Backdrop 横版剧照 URL（来自第三方元数据），Web 端详情/主推背景用；可能为空。
	Backdrop       string         `json:"backdrop,omitempty"`
	Candidates     []Candidate    `json:"candidates,omitempty"`
	MatchStatus    MatchStatus    `json:"matchStatus"`
	MetadataStatus MetadataStatus `json:"metadataStatus"`
	// Provider / ExternalID 是当前绑定的第三方条目（已确认或最高分候选）。
	Provider   string `json:"provider,omitempty"`
	ExternalID string `json:"externalId,omitempty"`
	UpdatedAt  int64  `json:"updatedAt"`
}

// SearchMedia 是搜索结果里的聚合卡片。
type SearchMedia struct {
	ID    string `json:"id"`
	Kind  Kind   `json:"kind"`
	Title string `json:"title"`
	Year  string `json:"year,omitempty"`
	Cover string `json:"cover,omitempty"`
	// Backdrop 横版剧照 URL（来自第三方元数据），Web 首页主推/详情背景用；可能为空。
	Backdrop string `json:"backdrop,omitempty"`
	TypeName string `json:"typeName,omitempty"`
	Remarks  string `json:"remarks,omitempty"`
	// Rating 展示用评分串（首页推荐来自豆瓣榜单评分；搜索卡片通常为空）。
	Rating      string      `json:"rating,omitempty"`
	SourceCount int         `json:"sourceCount"`
	MatchStatus MatchStatus `json:"matchStatus"`
	// Sources 是该聚合下可用的源引用，前端可据此直接进详情/换源。
	Sources []SourceRef `json:"sources"`
}
