// Package providers 封装第三方元数据源。
//
// 约定：
//   - 只返回候选，不直接修改媒体库；
//   - 所有请求走 upstream.Doer（SSRF、超时、重定向由它统一处理）；
//   - 未配置凭据时 Enabled=false，直接跳过。
package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cilicili/internal/media"
	"cilicili/internal/upstream"
)

const (
	// defaultTMDBBase 用 api.tmdb.org：国内网络下 api.themoviedb.org 会被
	// DNS 污染，而 api.tmdb.org 是同一套官方 API、可直连。
	defaultTMDBBase = "https://api.tmdb.org"
	tmdbImageBase   = "https://image.tmdb.org/t/p/w500"
	// tmdbBackdropBase 横版剧照（16:9），供 Web 主推/详情背景大图使用；w1280 比 w500 清晰。
	tmdbBackdropBase = "https://image.tmdb.org/t/p/w1280"
	tmdbTimeout      = 8 * time.Second
	tmdbMaxBytes     = 4 << 20
)

// TMDB 是 The Movie Database 元数据源，支持 v3 API key 或 v4 读访问令牌。
type TMDB struct {
	client upstream.Doer
	apiKey string
	token  string
	base   string
}

// NewTMDB 构造 TMDB provider；apiKey 与 readToken 任一可用即可。
func NewTMDB(client upstream.Doer, apiKey, readToken, base string) *TMDB {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = defaultTMDBBase
	}
	return &TMDB{
		client: client,
		apiKey: strings.TrimSpace(apiKey),
		token:  strings.TrimSpace(readToken),
		base:   base,
	}
}

// Name 返回 provider 标识。
func (p *TMDB) Name() string { return "tmdb" }

// Enabled 表示是否配置了可用凭据。
func (p *TMDB) Enabled() bool {
	return p != nil && p.client != nil && (p.apiKey != "" || p.token != "")
}

type tmdbSearchResponse struct {
	Results []struct {
		ID            int    `json:"id"`
		Title         string `json:"title"`
		Name          string `json:"name"`
		OriginalTitle string `json:"original_title"`
		OriginalName  string `json:"original_name"`
		Overview      string `json:"overview"`
		PosterPath    string `json:"poster_path"`
		BackdropPath  string `json:"backdrop_path"`
		ReleaseDate   string `json:"release_date"`
		FirstAirDate  string `json:"first_air_date"`
	} `json:"results"`
}

// Search 按标题搜索 TMDB，返回最多 10 个候选。电影查 movie，剧集/动漫查 tv。
func (p *TMDB) Search(ctx context.Context, req media.Request) ([]media.Candidate, error) {
	if !p.Enabled() || strings.TrimSpace(req.Title) == "" {
		return nil, nil
	}
	endpoint := ""
	switch req.Kind {
	case media.KindMovie:
		endpoint = "movie"
	case media.KindTV, media.KindAnime:
		endpoint = "tv"
	default:
		return nil, nil
	}

	q := url.Values{}
	q.Set("query", strings.TrimSpace(req.Title))
	q.Set("language", "zh-CN")
	q.Set("include_adult", "false")
	// 不传 year：采集站年份常不准，传了会漏召回；年份只在本地评分里用。
	if p.token == "" {
		q.Set("api_key", p.apiKey)
	}

	headers := make(http.Header, 2)
	headers.Set("Accept", "application/json")
	if p.token != "" {
		headers.Set("Authorization", "Bearer "+p.token)
	}
	opts := upstream.Options{
		Method:        http.MethodGet,
		Headers:       headers,
		Timeout:       tmdbTimeout,
		SafeRedirects: true,
		MaxBytes:      tmdbMaxBytes,
		Retries:       1,
	}
	raw, err := p.client.GetJSON(ctx, p.base+"/3/search/"+endpoint+"?"+q.Encode(), opts)
	if err != nil {
		return nil, fmt.Errorf("tmdb request: %w", err)
	}
	var resp tmdbSearchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("tmdb decode: %w", err)
	}

	out := make([]media.Candidate, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.ID <= 0 {
			continue
		}
		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = strings.TrimSpace(r.Name)
		}
		if title == "" {
			continue
		}
		original := strings.TrimSpace(r.OriginalTitle)
		if original == "" {
			original = strings.TrimSpace(r.OriginalName)
		}
		date := r.ReleaseDate
		kind := media.KindMovie
		if endpoint == "tv" {
			date = r.FirstAirDate
			kind = media.KindTV
		}
		c := media.Candidate{
			ID:         "tmdb:" + strconv.Itoa(r.ID),
			Provider:   "tmdb",
			ExternalID: strconv.Itoa(r.ID),
			Title:      title,
			Original:   original,
			Year:       yearOf(date),
			Kind:       kind,
			Overview:   strings.TrimSpace(r.Overview),
		}
		if pp := strings.TrimSpace(r.PosterPath); pp != "" {
			c.CoverURL = tmdbImageBase + pp
		}
		if bp := strings.TrimSpace(r.BackdropPath); bp != "" {
			c.BackdropURL = tmdbBackdropBase + bp
		}
		out = append(out, c)
		if len(out) >= 10 {
			break
		}
	}
	return out, nil
}

// yearOf 取日期前四位；无法识别时返回空串。
func yearOf(date string) string {
	date = strings.TrimSpace(date)
	if len(date) < 4 {
		return ""
	}
	year := date[:4]
	if _, err := strconv.Atoi(year); err != nil {
		return ""
	}
	return year
}
