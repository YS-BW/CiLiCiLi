package model

// SourceConfig 描述一个点播采集源（Apple CMS）。
type SourceConfig struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Detail  string `json:"detail,omitempty"`
	IsAdult bool   `json:"isAdult,omitempty"`
}

// LiveSourceConfig 描述一个直播源（M3U 订阅）。
type LiveSourceConfig struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	URL  string `json:"url"`
	EPG  string `json:"epg,omitempty"`
}

// SubscriptionRef 是部署者预置的数据源订阅。
type SubscriptionRef struct {
	URL         string `json:"url"`
	Name        string `json:"name,omitempty"`
	RefreshDays int    `json:"refreshDays,omitempty"`
}

// SearchResultItem 是一条跨源统一的搜索结果。
type SearchResultItem struct {
	SourceKey  string `json:"sourceKey"`
	SourceName string `json:"sourceName"`
	VodID      string `json:"vodId"`
	Name       string `json:"name"`
	Pic        string `json:"pic,omitempty"`
	TypeName   string `json:"typeName,omitempty"`
	Year       string `json:"year,omitempty"`
	Area       string `json:"area,omitempty"`
	Remarks    string `json:"remarks,omitempty"`
	SourceURL  string `json:"sourceUrl,omitempty"`
	IsAdult    bool   `json:"isAdult,omitempty"`
}

// SearchFailure 是单个源失败的原因。
type SearchFailure struct {
	SourceKey string `json:"sourceKey"`
	Error     string `json:"error"`
	TimedOut  bool   `json:"timedOut,omitempty"`
}

// SourceSearchOutcome 是单个源的搜索结果。
type SourceSearchOutcome struct {
	SourceKey string             `json:"sourceKey"`
	OK        bool               `json:"ok"`
	List      []SearchResultItem `json:"list"`
	Error     string             `json:"error,omitempty"`
	TimedOut  bool               `json:"timedOut,omitempty"`
	MS        int64              `json:"ms,omitempty"`
}

// SearchResponse 是非流式搜索的响应。
type SearchResponse struct {
	List     []SearchResultItem `json:"list"`
	Failures []SearchFailure    `json:"failures"`
}

// SourceEvent 是 NDJSON 流里的单源事件：{"type":"source", ...outcome}。
type SourceEvent struct {
	Type string `json:"type"`
	SourceSearchOutcome
}

// PartialDoneEvent 是软截止时下发的“大致完成”事件；之后仍可能继续收到 source 事件。
type PartialDoneEvent struct {
	Type     string             `json:"type"`
	List     []SearchResultItem `json:"list"`
	Failures []SearchFailure    `json:"failures"`
	Partial  bool               `json:"partial"`
}

// DoneEvent 是最终聚合结果。
type DoneEvent struct {
	Type     string             `json:"type"`
	List     []SearchResultItem `json:"list"`
	Failures []SearchFailure    `json:"failures"`
}

// VideoInfo 是详情页的元信息。
type VideoInfo struct {
	// 以下字段始终返回（未知时为空字符串），方便客户端统一渲染，不做推测。
	Title      string `json:"title"`
	Cover      string `json:"cover"`
	Desc       string `json:"desc"`
	TypeName   string `json:"typeName"`
	Year       string `json:"year"`
	Area       string `json:"area"`
	Director   string `json:"director"`
	Actor      string `json:"actor"`
	Remarks    string `json:"remarks"`
	SourceKey  string `json:"sourceKey"`
	SourceName string `json:"sourceName"`
	SourceURL  string `json:"sourceUrl"`
}

// Episode 是一集播放地址。ID 是来源内的稳定标识（线路序号 + 集序号），
// 不使用可能过期的播放 URL，便于客户端保存观看进度。
type Episode struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Index int    `json:"index"`
	URL   string `json:"url"`
}

// PlayLine 是采集站的一条播放线路。Name 来自 vod_play_from，缺失时由服务端
// 生成「线路N」；Episodes 保留原始集名与顺序。
type PlayLine struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Index int    `json:"index"`
	// Preferred 表示该线路包含 m3u8 直链（采集站常见的“分享页”线路不含），
	// 客户端可默认选它；服务端不探测线路是否可播放。
	Preferred bool      `json:"preferred"`
	Episodes  []Episode `json:"episodes"`
}

// VideoDetail 是详情接口的响应。
// Episodes 是向后兼容的扁平播放地址（优先 m3u8 线路）；新客户端应使用 PlayLines。
type VideoDetail struct {
	Episodes  []string   `json:"episodes"`
	PlayLines []PlayLine `json:"playLines"`
	VideoInfo VideoInfo  `json:"videoInfo"`
}

// QualityProbeItem 是一次画质探测的输入。
type QualityProbeItem struct {
	Source SourceConfig `json:"source"`
	ID     string       `json:"id"`
}

// QualityProbeResult 是单条画质探测结果。Height 为 0 表示“测不出”。
type QualityProbeResult struct {
	SourceKey string `json:"sourceKey"`
	VodID     string `json:"vodId"`
	Height    int    `json:"height,omitempty"`
	Error     string `json:"error,omitempty"`
}

// DoubanItem 是首页推荐卡片。
type DoubanItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Cover  string `json:"cover"`
	Rating string `json:"rating,omitempty"`
	IsTV   bool   `json:"isTv,omitempty"`
}

// AuthStatusResponse 是 /api/status 的响应。
type AuthStatusResponse struct {
	PasswordRequired       bool               `json:"passwordRequired"`
	Verified               bool               `json:"verified"`
	Version                string             `json:"version"`
	DefaultSources         []SourceConfig     `json:"defaultSources"`
	DefaultLiveSources     []LiveSourceConfig `json:"defaultLiveSources"`
	DefaultSubscriptions   []SubscriptionRef  `json:"defaultSubscriptions"`
	DefaultRecommendSource string             `json:"defaultRecommendSource,omitempty"`
	DefaultImageMode       string             `json:"defaultImageMode,omitempty"`
	Capabilities           []string           `json:"capabilities"`
}
