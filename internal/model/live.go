package model

// LiveChannel 是一个直播频道。
type LiveChannel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Logo    string `json:"logo,omitempty"`
	Group   string `json:"group,omitempty"`
	TvgID   string `json:"tvgId,omitempty"`
	RawName string `json:"rawName,omitempty"`
}

// LivePlaylistResponse 是 M3U 订阅解析结果。
type LivePlaylistResponse struct {
	Name     string        `json:"name,omitempty"`
	Channels []LiveChannel `json:"channels"`
	Groups   []string      `json:"groups"`
}

// EpgProgram 是 XMLTV 节目单的一条节目。
type EpgProgram struct {
	ChannelID string `json:"channelId"`
	Start     int64  `json:"start"`
	Stop      int64  `json:"stop"`
	Title     string `json:"title"`
	Desc      string `json:"desc,omitempty"`
}

// LiveEpgResponse 是单频道的节目单。
type LiveEpgResponse struct {
	ChannelID string       `json:"channelId"`
	Current   *EpgProgram  `json:"current,omitempty"`
	Next      *EpgProgram  `json:"next,omitempty"`
	Programs  []EpgProgram `json:"programs"`
}

// LiveProbeResult 是单个直播频道的测活结果。
type LiveProbeResult struct {
	URL      string `json:"url"`
	OK       bool   `json:"ok"`
	Status   int    `json:"status,omitempty"`
	MS       int64  `json:"ms,omitempty"`
	Level    string `json:"level,omitempty"` // segment / manifest / head
	Error    string `json:"error,omitempty"`
	Codec    string `json:"codec,omitempty"`
	TimedOut bool   `json:"timedOut,omitempty"`
	Kbps     int    `json:"kbps,omitempty"`
}

// BangumiCalendarDay 是 Bangumi 每日放送的一天。
type BangumiCalendarDay struct {
	Weekday int          `json:"weekday"`
	Items   []DoubanItem `json:"items"`
}

// SubscriptionSkipReason 是订阅条目被跳过的原因。
type SubscriptionSkipReason string

// 订阅跳过原因。
const (
	SkipSpider       SubscriptionSkipReason = "spider"
	SkipXML          SubscriptionSkipReason = "xml"
	SkipUnsearchable SubscriptionSkipReason = "unsearchable"
	SkipNonM3U       SubscriptionSkipReason = "nonM3uLive"
	SkipDisabled     SubscriptionSkipReason = "disabled"
	SkipInvalidURL   SubscriptionSkipReason = "invalidUrl"
)

// SubscriptionParseStats 说明订阅解析中的跳过与截断。
type SubscriptionParseStats struct {
	Format          string                         `json:"format"`
	Skipped         int                            `json:"skipped"`
	SkippedByReason map[SubscriptionSkipReason]int `json:"skippedByReason,omitempty"`
	SkippedSamples  []string                       `json:"skippedSamples,omitempty"`
	Truncated       int                            `json:"truncated"`
}

// SourceListPayload 是订阅解析结果。
type SourceListPayload struct {
	Name        string                  `json:"name,omitempty"`
	Sources     []SourceConfig          `json:"sources"`
	LiveSources []LiveSourceConfig      `json:"liveSources"`
	Stats       *SubscriptionParseStats `json:"stats,omitempty"`
}
