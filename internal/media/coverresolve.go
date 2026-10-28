package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cilicili/internal/upstream"
)

// ErrCoverNotFound 表示 TMDB 与源都没拿到有效封面。
var ErrCoverNotFound = errors.New("封面不可用")

// CoverRequest 是统一封面接口的输入。
// Title/Year/Kind 用于查 TMDB；SrcURL 是已知的原始图（采集源 pic / 豆瓣图），
// 与 TMDB 并发拉取，谁先成功先返回。Big 决定要横版大图还是竖版海报。
type CoverRequest struct {
	Title  string
	Year   string
	Kind   Kind
	SrcURL string
	Big    bool
}

// CoverResult 是统一封面接口的输出。
type CoverResult struct {
	Data        []byte
	ContentType string
	Source      string // tmdb / source
	Hash        string // 内容哈希，用作 ETag
	Variant     CoverVariant
}

type coverCand struct {
	data []byte
	ct   string
	ok   bool
}

const coverStoreSubdir = "store"

// FetchCover 统一封面获取：先查本地封面库；未命中则 TMDB 与源并发，先成功先返回；
// 缓存时 TMDB 优先（若先返回的是源，后台等 TMDB，成功则覆盖缓存）。
func (m *Manager) FetchCover(ctx context.Context, req CoverRequest) (*CoverResult, error) {
	key := coverKey(req.Title, req.Year, req.Kind, req.SrcURL)
	variant := CoverSmall
	if req.Big {
		variant = CoverBig
	}

	// 1) 本地封面库命中：直接返回文件。
	if e, err := m.store.getCoverEntry(key, variant); err == nil && e != nil && e.LocalPath != "" {
		if data, rerr := os.ReadFile(e.LocalPath); rerr == nil && len(data) > 0 {
			return &CoverResult{Data: data, ContentType: e.ContentType, Source: e.Source, Hash: e.Hash, Variant: variant}, nil
		}
	}

	// 2) 并发拉取：small 走 TMDB 与源并发；big 只用 TMDB（大封面只来自 TMDB，
	//    拿不到就 404，由前端用原站封面模糊打底，不返回竖版当大图）。
	tmdbCh := make(chan coverCand, 1)
	srcCh := make(chan coverCand, 1)
	hasSrc := !req.Big && strings.TrimSpace(req.SrcURL) != ""

	// TMDB 用独立后台 ctx，避免请求结束后被取消（用于后台覆盖缓存）。
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		target := strings.TrimSpace(req.Title)
		if target == "" {
			tmdbCh <- coverCand{}
			return
		}
		imgURL := m.resolveTMDBImageURL(bg, req.Title, req.Year, req.Kind, req.Big)
		if imgURL == "" {
			m.log.Debug("cover: tmdb url empty", "title", req.Title, "year", req.Year)
			tmdbCh <- coverCand{}
			return
		}
		data, ct := m.downloadImageBytes(bg, imgURL)
		if len(data) == 0 {
			m.log.Debug("cover: tmdb download failed", "url", imgURL)
			tmdbCh <- coverCand{}
			return
		}
		tmdbCh <- coverCand{data: data, ct: ct, ok: true}
	}()

	if hasSrc {
		go func() {
			bg, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			data, ct := m.downloadImageBytes(bg, req.SrcURL)
			srcCh <- coverCand{data: data, ct: ct, ok: len(data) > 0}
		}()
	} else {
		srcCh <- coverCand{}
	}

	if !hasSrc {
		t := <-tmdbCh
		if !t.ok {
			return nil, ErrCoverNotFound
		}
		return m.persistAndReturn(key, variant, CoverSrcTMDB, reqSrcURL(req), t), nil
	}

	// 3) 竞速：谁先成功先返回。
	for i := 0; i < 2; i++ {
		select {
		case t := <-tmdbCh:
			if t.ok {
				// TMDB 先到：缓存并返回（源仍在跑，忽略）。
				return m.persistAndReturn(key, variant, CoverSrcTMDB, "", t), nil
			}
			// TMDB 失败，继续等源。
			if i == 1 {
				return nil, ErrCoverNotFound
			}
		case s := <-srcCh:
			if s.ok {
				// 源先到：先缓存源并返回；后台等 TMDB，成功则覆盖缓存。
				res := m.persistAndReturn(key, variant, CoverSrcSource, req.SrcURL, s)
				go func() {
					t := <-tmdbCh
					if t.ok {
						m.persistCover(key, variant, CoverSrcTMDB, "", t.data, t.ct)
					}
				}()
				return res, nil
			}
			if i == 1 {
				return nil, ErrCoverNotFound
			}
		}
	}
	return nil, ErrCoverNotFound
}

func reqSrcURL(req CoverRequest) string { return strings.TrimSpace(req.SrcURL) }

// persistAndReturn 落盘 + 记录并返回结果。
func (m *Manager) persistAndReturn(key string, variant CoverVariant, source, url string, c coverCand) *CoverResult {
	hash := m.persistCover(key, variant, source, url, c.data, c.ct)
	return &CoverResult{Data: c.data, ContentType: c.ct, Source: source, Hash: hash, Variant: variant}
}

// persistCover 把封面数据写入文件并登记到 cover_store，返回内容哈希。
func (m *Manager) persistCover(key string, variant CoverVariant, source, srcURL string, data []byte, ct string) string {
	if len(data) == 0 {
		return ""
	}
	ext := coverExt(ct)
	if ext == "" {
		ext = ".img"
	}
	dir := filepath.Join(m.coverDir, coverStoreSubdir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return ""
	}
	path := filepath.Join(dir, key+"-"+string(variant)+ext)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		_ = os.Remove(tmp)
		return ""
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return ""
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])[:32]
	w, h := 0, 0
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		w, h = cfg.Width, cfg.Height
	}
	if err := m.store.saveCoverEntry(CoverStoreEntry{
		Key: key, Variant: variant, Source: source, URL: srcURL, LocalPath: path,
		ContentType: ct, Hash: hash, Width: w, Height: h,
	}, time.Now()); err != nil {
		m.log.Warn("save cover entry failed", "err", err, "variant", variant, "key", key)
	}
	return hash
}

// resolveTMDBImageURL 查 TMDB 拿图 URL：big→横版剧照(backdrop)，否则竖版海报(poster)。
// 必须标题匹配（避免同名误配），年份差 ≤1。
func (m *Manager) resolveTMDBImageURL(ctx context.Context, title, year string, kind Kind, big bool) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	if kind != KindTV && kind != KindMovie {
		kind = KindMovie
	}
	wantBase, _ := NormalizeTitle(title)
	year = strings.TrimSpace(year)
	for _, p := range m.providers {
		if p == nil || !p.Enabled() {
			continue
		}
		cands, err := p.Search(ctx, Request{Kind: kind, Title: title, Year: year})
		if err != nil {
			m.log.Debug("cover tmdb search err", "title", title, "year", year, "err", err)
			continue
		}
		for _, c := range cands {
			u := c.CoverURL
			if big {
				u = c.BackdropURL
			}
			if strings.TrimSpace(u) == "" {
				continue
			}
			gotBase, _ := NormalizeTitle(c.Title)
			if !titlesMatch(wantBase, gotBase) {
				continue
			}
			if year != "" && c.Year != "" && absYearDiff(year, c.Year) > 1 {
				continue
			}
			return u
		}
	}
	return ""
}

// downloadImageBytes 下载图片字节，校验是图片。豆瓣图带 Referer + 镜像兜底。
func (m *Manager) downloadImageBytes(ctx context.Context, rawURL string) ([]byte, string) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, ""
	}
	data, ct, err := m.fetchImageOnce(ctx, rawURL)
	if err == nil && len(data) > 0 {
		return data, ct
	}
	// 豆瓣图床反爬（418 / HTML 挑战页）：换镜像再取一次。
	if u, perr := url.Parse(rawURL); perr == nil && isDoubanHost(u.Hostname()) {
		if mirror := doubanMirrorURL(u); mirror != "" {
			if d2, ct2, e2 := m.fetchImageOnce(ctx, mirror); e2 == nil && len(d2) > 0 {
				return d2, ct2
			}
		}
	}
	return nil, ""
}

func (m *Manager) fetchImageOnce(ctx context.Context, rawURL string) ([]byte, string, error) {
	opts := upstream.Options{
		Timeout:       coverFetchTimeout,
		MaxBytes:      maxCoverBytes,
		Retries:       0,
		SafeRedirects: true,
		Headers:       http.Header{"Accept": []string{"image/*"}},
	}
	// TMDB 官方图域是可信来源；在其 DNS 被 TUN 解析为保留地址段时，跳过 IP 段校验
	// （否则本地 TUN 环境会 SSRF 拦截，VPS 正常）。仅对硬编码可信 host 放行。
	if u, err := url.Parse(rawURL); err == nil && isTMDBImageHost(u.Hostname()) {
		opts.AllowPrivate = true
	}
	if u, err := url.Parse(rawURL); err == nil && isDoubanHost(u.Hostname()) {
		opts.Headers.Set("Referer", "https://movie.douban.com/")
	}
	res, err := m.client.Do(ctx, rawURL, opts)
	if err != nil {
		m.log.Debug("cover: image do err", "url", rawURL, "allowPrivate", opts.AllowPrivate, "err", err)
		return nil, "", err
	}
	defer res.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, "", errors.New("HTTP " + strconv.Itoa(res.StatusCode))
	}
	ct := res.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(ct), "image/") {
		m.log.Debug("cover: not image", "url", rawURL, "ct", ct, "status", res.StatusCode)
		return nil, "", ErrCoverType
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxCoverBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 || len(data) > maxCoverBytes {
		return nil, "", ErrCoverTooLarge
	}
	return data, ct, nil
}

func isTMDBImageHost(host string) bool {
	h := strings.ToLower(host)
	return h == "image.tmdb.org" || h == "tmdb.org" || strings.HasSuffix(h, ".tmdb.org")
}

func isDoubanHost(host string) bool {
	h := strings.ToLower(host)
	return h == "douban.com" || strings.HasSuffix(h, ".douban.com") ||
		h == "doubanio.com" || strings.HasSuffix(h, ".doubanio.com")
}

func doubanMirrorURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.Host = "doubanio.viki.moe"
	return c.String()
}
