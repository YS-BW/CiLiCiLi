// Package detail 解析单个影片详情：优先标准列表接口，失败时回退详情页 HTML。
package detail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"cilicili/internal/cache"
	"cilicili/internal/cms"
	"cilicili/internal/model"
	"cilicili/internal/upstream"
)

var (
	// ErrInvalidID 表示影片 ID 不合法。
	ErrInvalidID = errors.New("无效的视频ID")
	// ErrInvalidSource 表示源配置不合法。
	ErrInvalidSource = errors.New("无效的点播源配置")
	// ErrNotFound 表示上游没有可用播放地址。
	ErrNotFound = errors.New("未找到播放资源")
)

// Config 是详情解析器的参数。
type Config struct {
	UserAgent  string
	Timeout    time.Duration
	CacheFresh time.Duration
	CacheStale time.Duration
}

// Resolver 负责详情解析与缓存。
type Resolver struct {
	client upstream.Doer
	cache  cache.Store
	log    *slog.Logger
	cfg    Config
	sf     singleflight.Group
}

// New 创建详情解析器。
func New(client upstream.Doer, store cache.Store, log *slog.Logger, cfg Config) *Resolver {
	if cfg.UserAgent == "" {
		cfg.UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 6 * time.Second
	}
	if cfg.CacheFresh <= 0 {
		cfg.CacheFresh = 10 * time.Minute
	}
	if cfg.CacheStale <= 0 {
		cfg.CacheStale = 24 * time.Hour
	}
	return &Resolver{client: client, cache: store, log: log, cfg: cfg}
}

// Resolve 解析详情。成功结果会写缓存；同一 key 的并发请求会被合并。
func (r *Resolver) Resolve(ctx context.Context, id string, src model.SourceConfig) (*model.VideoDetail, error) {
	id = strings.TrimSpace(id)
	if !validID(id) {
		return nil, ErrInvalidID
	}
	if !strings.HasPrefix(src.URL, "http://") && !strings.HasPrefix(src.URL, "https://") {
		return nil, ErrInvalidSource
	}
	key := cacheKey(id, src)

	if raw, ok := r.cache.Get(key); ok {
		if d, err := decode(raw); err == nil {
			return d, nil
		}
	}
	if raw, _, found := r.cache.GetStale(key); found {
		if d, err := decode(raw); err == nil {
			return d, nil
		}
	}

	base := context.WithoutCancel(ctx)
	v, err, _ := r.sf.Do(key, func() (any, error) {
		if raw, ok := r.cache.Get(key); ok {
			if d, err := decode(raw); err == nil {
				return d, nil
			}
		}
		runCtx, cancel := context.WithTimeout(base, r.cfg.Timeout*2)
		defer cancel()
		d, err := r.fetch(runCtx, id, src)
		if err != nil {
			return nil, err
		}
		if raw, mErr := json.Marshal(d); mErr == nil {
			r.cache.Set(key, raw, r.cfg.CacheFresh, r.cfg.CacheStale)
		}
		return d, nil
	})
	if err != nil {
		return nil, err
	}
	d, _ := v.(*model.VideoDetail)
	return normalize(d), nil
}

func (r *Resolver) fetch(ctx context.Context, id string, src model.SourceConfig) (*model.VideoDetail, error) {
	if d := r.fetchFromAPI(ctx, id, src); d != nil {
		return d, nil
	}

	// 回退：爬详情页 HTML。
	root := strings.TrimRight(src.Detail, "/")
	if root == "" {
		if u, uErr := url.Parse(src.URL); uErr == nil && u.Host != "" {
			root = u.Scheme + "://" + u.Host
		}
	}
	if root == "" {
		return nil, ErrNotFound
	}
	detailURL := root + "/index.php/vod/detail/id/" + id + ".html"
	headers := http.Header{}
	headers.Set("User-Agent", r.cfg.UserAgent)
	htmlRes, hErr := r.client.Do(ctx, detailURL, upstream.Options{
		Headers:       headers,
		Timeout:       r.cfg.Timeout,
		MaxBytes:      8 << 20,
		SafeRedirects: true,
	})
	if hErr != nil {
		return nil, hErr
	}
	defer htmlRes.Close()
	if htmlRes.StatusCode < 200 || htmlRes.StatusCode >= 300 {
		return nil, fmt.Errorf("详情页返回 HTTP %d", htmlRes.StatusCode)
	}
	html, readErr := htmlRes.ReadAll(8 << 20)
	if readErr != nil {
		return nil, readErr
	}
	d := cms.ParseDetailPageHTML(html, src)
	if d == nil || len(d.Episodes) == 0 {
		return nil, ErrNotFound
	}
	return d, nil
}

// fetchFromAPI 尝试标准列表接口。任何失败都返回 nil，由调用方回退详情页。
func (r *Resolver) fetchFromAPI(ctx context.Context, id string, src model.SourceConfig) *model.VideoDetail {
	api := strings.TrimRight(src.URL, "/")
	sep := "?"
	if strings.Contains(api, "?") {
		sep = "&"
	}
	api += sep + "ac=videolist&ids=" + url.QueryEscape(id)

	res, err := r.client.Do(ctx, api, upstream.Options{
		Headers:       cms.RequestHeaders(r.cfg.UserAgent),
		Timeout:       r.cfg.Timeout,
		MaxBytes:      16 << 20,
		Retries:       1,
		SafeRedirects: true,
	})
	if err != nil {
		return nil
	}
	defer res.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil
	}
	data, err := res.ReadAll(16 << 20)
	if err != nil {
		return nil
	}
	d, err := cms.ParseDetail(data, src)
	if err != nil || len(d.Episodes) == 0 {
		return nil
	}
	return d
}

func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func cacheKey(id string, src model.SourceConfig) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s", strings.TrimRight(src.URL, "/"), strings.TrimRight(src.Detail, "/"), id)
	return "detail:v4:" + hex.EncodeToString(h.Sum(nil))[:32]
}

func decode(raw []byte) (*model.VideoDetail, error) {
	var d model.VideoDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	if d.Episodes == nil {
		d.Episodes = []string{}
	}
	return normalize(&d), nil
}

// normalize 修正缓存/上游数据里的派生字段：PlayLines 非空、preferred 只标第一条 m3u8 线路。
// 旧缓存没有 preferred 字段，读出来时在这里补齐。
func normalize(d *model.VideoDetail) *model.VideoDetail {
	if d == nil {
		return nil
	}
	if d.Episodes == nil {
		d.Episodes = []string{}
	}
	if d.PlayLines == nil {
		d.PlayLines = []model.PlayLine{}
	}
	// 旧缓存可能是 l1e1 形式的 ID，按当前「名称优先」规则重建。
	cms.RebuildIDs(d.PlayLines)
	for i := range d.PlayLines {
		d.PlayLines[i].Preferred = false
	}
	for i := range d.PlayLines {
		for _, ep := range d.PlayLines[i].Episodes {
			if strings.Contains(strings.ToLower(ep.URL), ".m3u8") {
				d.PlayLines[i].Preferred = true
				return d
			}
		}
	}
	return d
}
