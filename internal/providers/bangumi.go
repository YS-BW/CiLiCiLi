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

// Bangumi 是 Bangumi（bgm.tv）公开 API 客户端，动漫优先使用。
// 该接口免 key，但只覆盖动漫/书籍/游戏等；电影剧集目前只用采集站数据。
type Bangumi struct {
	client  upstream.Doer
	baseURL string
	timeout time.Duration
}

// NewBangumi 创建 Bangumi provider。
func NewBangumi(client upstream.Doer, baseURL string) *Bangumi {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.bgm.tv"
	}
	return &Bangumi{client: client, baseURL: strings.TrimRight(baseURL, "/"), timeout: 10 * time.Second}
}

// Name 实现 Provider。
func (b *Bangumi) Name() string { return "bangumi" }

// Enabled 实现 Provider。
func (b *Bangumi) Enabled() bool { return b.client != nil }

type bangumiSearchResponse struct {
	Results []bangumiItem `json:"results"`
}

type bangumiItem struct {
	ID      int           `json:"id"`
	Name    string        `json:"name"`
	NameCN  string        `json:"name_cn"`
	Summary string        `json:"summary"`
	Date    string        `json:"air_date"`
	Images  bangumiImages `json:"images"`
}

type bangumiImages struct {
	Large  string `json:"large"`
	Common string `json:"common"`
	Medium string `json:"medium"`
}

// Search 实现 Provider。使用免 key 的旧版搜索接口，避免 POST body 与新版鉴权。
func (b *Bangumi) Search(ctx context.Context, req media.Request) ([]media.Candidate, error) {
	if !b.Enabled() {
		return nil, nil
	}
	// Bangumi 只覆盖动漫；剧集/电影交给采集站数据，避免同名动画误匹配。
	if req.Kind != media.KindAnime {
		return nil, nil
	}
	keyword := strings.TrimSpace(req.Title)
	if keyword == "" {
		return nil, nil
	}
	q := url.Values{}
	q.Set("type", "2") // 动画
	q.Set("responseGroup", "large")
	q.Set("max_results", "10")
	target := b.baseURL + "/search/subject/" + url.PathEscape(keyword) + "?" + q.Encode()

	res, err := b.client.Do(ctx, target, upstream.Options{
		Timeout:       b.timeout,
		MaxBytes:      4 << 20,
		Retries:       1,
		SafeRedirects: true,
		Headers: http.Header{
			"Accept":     []string{"application/json"},
			"User-Agent": []string{"cilicili/1.0"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("bangumi request: %w", err)
	}
	defer res.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("bangumi http %d", res.StatusCode)
	}
	body, err := res.ReadAll(4 << 20)
	if err != nil {
		return nil, err
	}
	var resp bangumiSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("bangumi decode: %w", err)
	}

	out := make([]media.Candidate, 0, len(resp.Results))
	for _, item := range resp.Results {
		title := item.NameCN
		if title == "" {
			title = item.Name
		}
		year := ""
		if len(item.Date) >= 4 {
			year = item.Date[:4]
		}
		cover := item.Images.Large
		if cover == "" {
			cover = item.Images.Common
		}
		c := media.Candidate{
			ID:         "bangumi:" + strconv.Itoa(item.ID),
			Provider:   "bangumi",
			ExternalID: strconv.Itoa(item.ID),
			Title:      title,
			Original:   item.Name,
			Year:       year,
			Kind:       media.KindAnime,
			Overview:   item.Summary,
			CoverURL:   cover,
		}
		out = append(out, c)
	}
	return out, nil
}
