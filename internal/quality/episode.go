package quality

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"cilicili/internal/cms"
	"cilicili/internal/model"
	"cilicili/internal/upstream"
)

var (
	// 末尾允许 ?query / #fragment：不少源在 .m3u8 后挂鉴权参数或分片锚点。
	m3u8TargetRE = regexp.MustCompile(`(?i)\.m3u8([?#]|$)`)
	mp4TargetRE  = regexp.MustCompile(`(?i)\.mp4([?#]|$)`)
)

// episodeProbe 是单集探测的中间结果。
type episodeProbe struct {
	height int
	known  bool
	errMsg string
	status int
}

// probeUncached 探测一条「源 + 影片」：取播放地址 → 按集探测 → 汇总。
// 探测不到画质不算失败（Error 为空），只有请求层面失败才写 Error。
func (p *Prober) probeUncached(ctx context.Context, item model.QualityProbeItem) model.QualityProbeResult {
	res := model.QualityProbeResult{SourceKey: item.Source.Key, VodID: item.ID}

	episodes, errMsg := p.fetchEpisodes(ctx, item)
	if len(episodes) == 0 {
		if errMsg == "" {
			errMsg = msgNoEpisodes
		}
		res.Error = errMsg
		return res
	}

	start := time.Now()
	lastError := ""
	for _, target := range ProbeEpisodeOrder(episodes) {
		probe := p.probeEpisode(ctx, target, item.Source)
		if probe.known {
			res.Height = probe.height
			return res
		}
		if probe.errMsg != "" {
			lastError = probe.errMsg
		}
		// 源站级拒绝（401/403）换集也不会变，不必再耗一轮超时。
		if probe.status == http.StatusUnauthorized || probe.status == http.StatusForbidden {
			break
		}
		// 慢源换集重试会把总死线打穿，反而把「未知」变成「探测失败」：
		// 时间过半就不再换集。
		if time.Since(start) > p.timeout/2 {
			break
		}
	}
	res.Error = lastError
	return res
}

// ProbeEpisodeOrder 给出探测顺序：中间集优先，探不出再退到最后一集。
//
// 取中间集是因为部分源第一集是预告/广告片；但采集站常有个别集的分片缺失、
// 加密或换过编码，中间集恰好命中这种集时，整部剧会一直是「未知」，
// 所以电视剧（多集）补一次最后一集。单集影片无集可换。
func ProbeEpisodeOrder(episodes []string) []string {
	if len(episodes) == 0 {
		return nil
	}
	if len(episodes) < 2 {
		return episodes[:1]
	}
	mid := episodes[len(episodes)/2]
	last := episodes[len(episodes)-1]
	if mid == last {
		return []string{mid}
	}
	return []string{mid, last}
}

// fetchEpisodes 取播放地址列表：列表接口优先，拿不到再爬详情页。
func (p *Prober) fetchEpisodes(ctx context.Context, item model.QualityProbeItem) ([]string, string) {
	src := item.Source

	if apiURL := listAPIURL(src.URL, item.ID); apiURL != "" {
		data, _, err := p.fetchBytes(ctx, apiURL, cms.RequestHeaders(p.ua), detailMaxBytes)
		if err == nil {
			if detail, parseErr := cms.ParseDetail(data, src); parseErr == nil && len(detail.Episodes) > 0 {
				return detail.Episodes, ""
			}
		} else if errors.Is(err, upstream.ErrBlocked) {
			// 源地址本身被 SSRF 拦截：详情页大概率同一 host，直接给出明确结论。
			return nil, msgBlocked
		} else if ctx.Err() != nil {
			return nil, p.classify(ctx, err)
		}
	}

	if ctx.Err() != nil {
		return nil, msgTimeout
	}
	root := detailRoot(src)
	if root == "" {
		return nil, msgNoEpisodes
	}

	headers := make(http.Header, 1)
	headers.Set("User-Agent", p.ua)
	pageURL := root + "/index.php/vod/detail/id/" + url.PathEscape(item.ID) + ".html"
	data, _, err := p.fetchBytes(ctx, pageURL, headers, detailPageMaxBytes)
	if err != nil {
		if ctx.Err() != nil {
			return nil, p.classify(ctx, err)
		}
		return nil, msgNoEpisodes
	}
	detail := cms.ParseDetailPageHTML(data, src)
	if detail == nil || len(detail.Episodes) == 0 {
		return nil, msgNoEpisodes
	}
	return detail.Episodes, ""
}

// probeEpisode 探测单集地址：m3u8 走「清单 → 分片」两级，mp4 直链直接读 moov。
func (p *Prober) probeEpisode(ctx context.Context, target string, src model.SourceConfig) episodeProbe {
	if !m3u8TargetRE.MatchString(target) {
		// mp4 直链：moov 里有分辨率；未 faststart 的文件 moov 在尾部，前缀拿不到。
		if !mp4TargetRE.MatchString(target) {
			return episodeProbe{}
		}
		buf, err := p.fetchPrefix(ctx, target, src, segmentPrefixBytes)
		if err != nil {
			// 直链拿不到不算探测失败：源站可能要求鉴权，前端显示「未知」即可。
			return episodeProbe{}
		}
		if res, ok := ParseMP4VideoResolution(buf); ok {
			return episodeProbe{height: res.Height, known: true}
		}
		return episodeProbe{}
	}

	text, finalURL, err := p.fetchBytes(ctx, target, p.probeHeaders(src), m3u8MaxBytes)
	if err != nil {
		return episodeProbe{errMsg: p.classify(ctx, err), status: statusOf(err)}
	}
	if height, ok := ParseBestHeight(string(text)); ok {
		return episodeProbe{height: height, known: true}
	}
	// 单码率媒体列表：m3u8 里没有档位信息，下沉到分片挖分辨率。
	// 相对地址必须以重定向后的最终 URL 为基准解析：CDN gslb 调度会 302 到别的目录。
	baseURL := finalURL
	if baseURL == "" {
		baseURL = target
	}
	if height, ok := p.resolveFromMediaPlaylist(ctx, baseURL, string(text), src); ok {
		return episodeProbe{height: height, known: true}
	}
	return episodeProbe{}
}

// resolveFromMediaPlaylist 从媒体列表的首分片/init 里解析分辨率。
// 任何失败都返回 (0,false)：走到这一步说明 m3u8 已经拿到、源是通的，
// 这一阶段失败只算「没探出画质」。
func (p *Prober) resolveFromMediaPlaylist(ctx context.Context, playlistURL, text string, src model.SourceConfig) (int, bool) {
	info := ParseMediaPlaylist(text)
	if info.FirstSegment == "" {
		return 0, false
	}

	segCtx, cancel := context.WithTimeout(ctx, p.segmentBudget)
	defer cancel()

	// fMP4：moov 在 init segment 里，比媒体分片小得多且一定含分辨率，优先走这条。
	if info.InitURI != "" {
		if initURL, ok := resolveURL(playlistURL, info.InitURI); ok {
			if buf, err := p.fetchPrefix(segCtx, initURL, src, initPrefixBytes); err == nil {
				if res, ok := ParseMP4VideoResolution(buf); ok {
					return res.Height, true
				}
			}
		}
	}

	segmentURL, ok := resolveURL(playlistURL, info.FirstSegment)
	if !ok {
		return 0, false
	}
	buf, err := p.fetchPrefix(segCtx, segmentURL, src, segmentPrefixBytes)
	if err != nil || len(buf) == 0 {
		return 0, false
	}

	if info.KeyMethod != "" && !strings.EqualFold(info.KeyMethod, "NONE") {
		// SAMPLE-AES 是按采样加密的，服务端拿不到分片级解密上下文，直接放弃。
		if !strings.EqualFold(info.KeyMethod, "AES-128") || info.KeyURI == "" {
			return 0, false
		}
		keyURL, ok := resolveURL(playlistURL, info.KeyURI)
		if !ok {
			return 0, false
		}
		key, err := p.fetchPrefix(segCtx, keyURL, src, keyMaxBytes)
		if err != nil || len(key) < 16 {
			return 0, false
		}
		iv := info.KeyIV
		if len(iv) < 16 {
			iv = defaultIV(info.MediaSequence)
		}
		plain := DecryptAES128CBCPrefix(buf, key, iv)
		if len(plain) == 0 {
			return 0, false
		}
		buf = plain
	}

	// 按内容判断容器比按扩展名可靠：不少源的分片没有扩展名。
	if LooksLikeMP4(buf) {
		if res, ok := ParseMP4VideoResolution(buf); ok {
			return res.Height, true
		}
		return 0, false
	}
	if res, ok := ParseTSVideoResolution(buf); ok {
		return res.Height, true
	}
	return 0, false
}

// fetchBytes 拉取一段完整响应（受 maxBytes 截断），非 2xx 返回 StatusError。
// 第二个返回值是重定向后的最终 URL，供解析相对地址使用（可能为空）。
func (p *Prober) fetchBytes(ctx context.Context, rawURL string, headers http.Header, maxBytes int64) ([]byte, string, error) {
	res, err := p.client.Do(ctx, rawURL, upstream.Options{
		Headers:       headers,
		Timeout:       requestTimeout,
		SafeRedirects: true,
		MaxBytes:      maxBytes,
	})
	if err != nil {
		return nil, "", err
	}
	defer res.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, res.FinalURL, &upstream.StatusError{StatusCode: res.StatusCode, URL: res.FinalURL}
	}
	data, readErr := res.ReadAll(maxBytes)
	return data, res.FinalURL, readErr
}

// fetchPrefix 只读取响应体前 maxBytes 字节：分片动辄几十 MB，全量拉取纯属浪费。
func (p *Prober) fetchPrefix(ctx context.Context, rawURL string, src model.SourceConfig, maxBytes int64) ([]byte, error) {
	data, _, err := p.fetchBytes(ctx, rawURL, p.probeHeaders(src), maxBytes)
	return data, err
}

// probeHeaders 是探测阶段的请求头：与播放时的表现尽量一致，避免被防盗链拦下。
func (p *Prober) probeHeaders(src model.SourceConfig) http.Header {
	headers := make(http.Header, 3)
	headers.Set("User-Agent", p.ua)
	headers.Set("Accept", "*/*")
	if origin := originOf(src.URL); origin != "" {
		headers.Set("Referer", origin+"/")
	}
	return headers
}

// listAPIURL 构造 Apple CMS 列表接口地址；源地址非法时返回空串。
func listAPIURL(base, id string) string {
	base = strings.TrimSpace(base)
	if !isHTTPURL(base) {
		return ""
	}
	base = strings.TrimRight(base, "/")
	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	return base + separator + "ac=videolist&ids=" + url.QueryEscape(id)
}

// detailRoot 推导详情页根地址：优先 source.detail，缺失时用接口地址的 origin。
func detailRoot(src model.SourceConfig) string {
	root := strings.TrimRight(strings.TrimSpace(src.Detail), "/")
	if root != "" {
		if isHTTPURL(root) {
			return root
		}
		return ""
	}
	if origin := originOf(src.URL); origin != "" {
		return origin
	}
	return ""
}

// originOf 返回 URL 的 scheme://host；解析失败返回空串。
func originOf(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// resolveURL 以 base 为基准解析 ref（兼容相对路径）；非 http(s) 返回 false。
func resolveURL(base, ref string) (string, bool) {
	baseURL, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", false
	}
	refURL, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", false
	}
	if refURL.Scheme != "" && refURL.Scheme != "http" && refURL.Scheme != "https" {
		return "", false
	}
	resolved := baseURL.ResolveReference(refURL)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return "", false
	}
	return resolved.String(), true
}
