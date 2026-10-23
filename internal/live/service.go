package live

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"cilicili/internal/cache"
	"cilicili/internal/model"
	"cilicili/internal/upstream"
)

const (
	// defaultUserAgent 与 upstream/代理侧保持同一浏览器 UA，避免源站按 UA 拒绝。
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"
	// defaultPlaylistTimeout 是拉取 M3U 订阅的总超时（含 body）。
	defaultPlaylistTimeout = 10 * time.Second
	// defaultEPGTimeout 是拉取 XMLTV 的总超时：节目单文件大，需要更宽裕。
	defaultEPGTimeout = 30 * time.Second
	// defaultEPGWindow 是 EPG 默认时间窗（前后各 24 小时）。
	defaultEPGWindow = 24 * time.Hour

	// playlistCacheFresh/Stale：订阅缓存 10 分钟，过期后最多再兜底 6 小时。
	playlistCacheFresh = 10 * time.Minute
	playlistCacheStale = 6 * time.Hour
	// epgCacheFresh/Stale：节目单缓存 6 小时（LibreTV 同款），过期后兜底 24 小时。
	epgCacheFresh = 6 * time.Hour
	epgCacheStale = 24 * time.Hour

	// playlistMaxBytes / epgMaxBytes 是单次抓取的大小上限。
	playlistMaxBytes = 8 << 20
	epgMaxBytes      = 32 << 20

	// maxURLLength 限制用户输入的订阅地址长度，避免超长串进缓存键/日志。
	maxURLLength = 4096
)

// 直播服务的错误。错误文案不含内网地址，可直接回给客户端。
var (
	// ErrInvalidURL 表示订阅/节目单地址不是合法的 http(s) URL。
	ErrInvalidURL = errors.New("直播地址无效")
	// ErrMissingChannel 表示缺少 channel 参数。
	ErrMissingChannel = errors.New("缺少 channel 参数")
	// ErrNoClient 表示没有配置出网客户端。
	ErrNoClient = errors.New("未配置出网客户端")
	// ErrInvalidPlaylist 表示内容不是有效的 M3U 播放列表。
	ErrInvalidPlaylist = errors.New("不是有效的 M3U 播放列表")
	// ErrInvalidXMLTV 表示内容不是有效的 XMLTV 节目单。
	ErrInvalidXMLTV = errors.New("不是有效的 XMLTV 节目单")
)

// Service 提供直播订阅、EPG 与频道测活。并发安全。
type Service struct {
	client       upstream.Doer
	cache        cache.Store
	log          *slog.Logger
	ua           string
	allowPrivate bool
	timeout      time.Duration
	epgTimeout   time.Duration

	probeConcurrency int
	probeBudget      time.Duration
	limiter          *hostLimiter

	// now 是时间源：测活结果缓存 TTL 依赖它，测试可注入。
	now func() time.Time

	probeMu      sync.Mutex
	probeEntries map[string]probeCacheEntry

	sf singleflight.Group
}

// New 创建直播服务。除 Client 外的零值字段都会落到推荐默认值。
func New(opts Options) *Service {
	ua := strings.TrimSpace(opts.UA)
	if ua == "" {
		ua = defaultUserAgent
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultPlaylistTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	concurrency := opts.ProbeConcurrency
	if concurrency <= 0 {
		concurrency = defaultProbeConcurrency
	}
	return &Service{
		client:       opts.Client,
		cache:        opts.Cache,
		log:          log,
		ua:           ua,
		allowPrivate: opts.AllowPrivate,
		timeout:      timeout,
		// EPG 文件通常是订阅的数十倍，超时按同一比例放宽（默认 10s → 30s）。
		epgTimeout:       timeout * 3,
		probeConcurrency: concurrency,
		probeBudget:      defaultProbeBudget,
		limiter:          newHostLimiter(probeHostLimit),
		now:              time.Now,
		probeEntries:     make(map[string]probeCacheEntry),
	}
}

// Playlist 拉取并解析 M3U 订阅。
//
// 缓存 10 分钟（force 跳过缓存强制刷新）；同一地址的并发请求只回源一次。
// 拉取失败时若有未超过 stale 期限的旧结果则返回旧结果（避免上游抖动放大成故障）。
func (s *Service) Playlist(ctx context.Context, rawURL string, force bool) (*model.LivePlaylistResponse, error) {
	rawURL = strings.TrimSpace(rawURL)
	if !isHTTPURL(rawURL) {
		return nil, ErrInvalidURL
	}
	if s.client == nil {
		return nil, ErrNoClient
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := cacheKey("live:playlist:", rawURL)

	if !force {
		if hit := s.loadPlaylist(key, false); hit != nil {
			return hit, nil
		}
	}

	// 与 detail/search 一致：共享 in-flight 抓取不跟随单个请求的取消，
	// 否则首个调用方断开会让同 key 的其它等待者一起失败。
	base := context.WithoutCancel(ctx)
	value, err, _ := s.sf.Do(key, func() (any, error) {
		if !force {
			if hit := s.loadPlaylist(key, false); hit != nil {
				return hit, nil
			}
		}
		runCtx, cancel := context.WithTimeout(base, s.timeout)
		defer cancel()
		res, fetchErr := s.fetchPlaylist(runCtx, rawURL)
		if fetchErr != nil {
			return nil, fetchErr
		}
		s.storeJSON(key, res, playlistCacheFresh, playlistCacheStale)
		return res, nil
	})
	if err != nil {
		if stale := s.loadPlaylist(key, true); stale != nil {
			s.log.WarnContext(ctx, "直播订阅拉取失败，返回过期缓存", "url", redactURL(rawURL), "err", err)
			return stale, nil
		}
		return nil, err
	}
	res, _ := value.(*model.LivePlaylistResponse)
	if res == nil {
		return nil, ErrInvalidPlaylist
	}
	return res, nil
}

// EPG 拉取并解析 XMLTV 节目单，返回指定频道的当前/下一节目与窗口内全部节目。
//
// 整份节目单缓存 6 小时（force 跳过缓存）；按频道查询不重复回源。
func (s *Service) EPG(ctx context.Context, epgURL, channel string, force bool) (*model.LiveEpgResponse, error) {
	epgURL = strings.TrimSpace(epgURL)
	channel = strings.TrimSpace(channel)
	if !isHTTPURL(epgURL) {
		return nil, ErrInvalidURL
	}
	if channel == "" {
		return nil, ErrMissingChannel
	}
	if s.client == nil {
		return nil, ErrNoClient
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := cacheKey("live:epg:", epgURL)

	programs, err := s.loadEPGPrograms(ctx, key, epgURL, force)
	if err != nil {
		return nil, err
	}
	list := programs[channel]
	if list == nil {
		list = []model.EpgProgram{}
	}
	current, next := CurrentAndNext(list, s.now())
	return &model.LiveEpgResponse{
		ChannelID: channel,
		Current:   current,
		Next:      next,
		Programs:  list,
	}, nil
}

func (s *Service) loadEPGPrograms(ctx context.Context, key, rawURL string, force bool) (map[string][]model.EpgProgram, error) {
	if !force {
		if hit := s.loadPrograms(key, false); hit != nil {
			return hit, nil
		}
	}
	base := context.WithoutCancel(ctx)
	value, err, _ := s.sf.Do(key, func() (any, error) {
		if !force {
			if hit := s.loadPrograms(key, false); hit != nil {
				return hit, nil
			}
		}
		runCtx, cancel := context.WithTimeout(base, s.epgTimeout)
		defer cancel()
		body, fetchErr := s.fetch(runCtx, rawURL, epgMaxBytes)
		if fetchErr != nil {
			return nil, fmt.Errorf("节目单地址请求失败: %w", fetchErr)
		}
		parsed, parseErr := ParseXMLTV(body, defaultEPGWindow, s.now())
		if parseErr != nil {
			return nil, parseErr
		}
		s.storeJSON(key, parsed, epgCacheFresh, epgCacheStale)
		return parsed, nil
	})
	if err != nil {
		if stale := s.loadPrograms(key, true); stale != nil {
			s.log.WarnContext(ctx, "节目单拉取失败，返回过期缓存", "url", redactURL(rawURL), "err", err)
			return stale, nil
		}
		return nil, err
	}
	programs, _ := value.(map[string][]model.EpgProgram)
	if programs == nil {
		programs = map[string][]model.EpgProgram{}
	}
	return programs, nil
}

// fetchPlaylist 抓取并解析 M3U 文本（相对地址按订阅地址解析）。
func (s *Service) fetchPlaylist(ctx context.Context, rawURL string) (*model.LivePlaylistResponse, error) {
	body, err := s.fetch(ctx, rawURL, playlistMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("订阅地址请求失败: %w", err)
	}
	res, err := parseM3U(body, rawURL)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// fetch 抓取上游字节。非 2xx 与超限都返回错误；gzip 由解析层按魔数处理。
func (s *Service) fetch(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	if s.client == nil {
		return nil, ErrNoClient
	}
	headers := http.Header{}
	headers.Set("User-Agent", s.ua)
	headers.Set("Accept", "*/*")
	res, err := s.client.Do(ctx, rawURL, upstream.Options{
		Headers:       headers,
		AllowPrivate:  s.allowPrivate,
		SafeRedirects: true,
	})
	if err != nil {
		return nil, safeError(err)
	}
	defer func() { _ = res.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("上游返回 HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("上游响应超过 %d 字节", maxBytes)
	}
	return body, nil
}

// loadPlaylist 读取订阅缓存；allowStale=true 时也接受已过新鲜期、仍在 stale 期内的条目。
func (s *Service) loadPlaylist(key string, allowStale bool) *model.LivePlaylistResponse {
	raw, ok := s.getCached(key, allowStale)
	if !ok {
		return nil
	}
	var res model.LivePlaylistResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil
	}
	if res.Channels == nil {
		res.Channels = []model.LiveChannel{}
	}
	if res.Groups == nil {
		res.Groups = []string{}
	}
	return &res
}

// loadPrograms 读取节目单缓存（map[channelID][]EpgProgram）。
func (s *Service) loadPrograms(key string, allowStale bool) map[string][]model.EpgProgram {
	raw, ok := s.getCached(key, allowStale)
	if !ok {
		return nil
	}
	var programs map[string][]model.EpgProgram
	if err := json.Unmarshal(raw, &programs); err != nil {
		return nil
	}
	if programs == nil {
		programs = map[string][]model.EpgProgram{}
	}
	return programs
}

func (s *Service) getCached(key string, allowStale bool) ([]byte, bool) {
	if s.cache == nil {
		return nil, false
	}
	if raw, ok := s.cache.Get(key); ok {
		return raw, true
	}
	if allowStale {
		if raw, _, found := s.cache.GetStale(key); found {
			return raw, true
		}
	}
	return nil, false
}

func (s *Service) storeJSON(key string, value any, fresh, stale time.Duration) {
	if s.cache == nil {
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		s.log.Warn("直播缓存序列化失败", "key", key, "err", err)
		return
	}
	s.cache.Set(key, raw, fresh, stale)
}

// cacheKey 把用户输入收敛成定长缓存键，避免超长 URL 进缓存/持久化。
func cacheKey(prefix, rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return prefix + hex.EncodeToString(sum[:])[:32]
}

// isHTTPURL 只接受 http(s) 绝对地址；SSRF 校验仍由 upstream.Doer 负责。
func isHTTPURL(raw string) bool {
	if raw == "" || len(raw) > maxURLLength {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Host != ""
}

// redactURL 只保留协议/主机/路径，避免日志泄露订阅地址里的 token。
func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<非法 URL>"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	out := parsed.String()
	if len(out) > 200 {
		out = out[:200] + "…"
	}
	return out
}

// safeError 把出网错误转成可安全回给客户端的错误：
// 已是固定文案/标准哨兵的错误保持原样（errors.Is 可用），其余抹掉 URL/IP 后重建。
func safeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, upstream.ErrBlocked), errors.Is(err, upstream.ErrTooLarge),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return errors.New(safeErrorText(err))
}

// safeErrorText 生成不含完整 URL / IP 的错误文案（探测结果与订阅错误共用）。
func safeErrorText(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, upstream.ErrBlocked):
		return "目标地址不可用"
	case errors.Is(err, upstream.ErrTooLarge):
		return "上游响应体超过大小限制"
	case errors.Is(err, context.Canceled):
		return "请求已取消"
	case errors.Is(err, context.DeadlineExceeded):
		return "请求超时"
	}
	// url.Error 的 Error() 带完整目标地址（可能含 token），只保留底层原因。
	msg := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		msg = urlErr.Err.Error()
	}
	msg = probeIPv4Re.ReplaceAllString(msg, "<ip>")
	msg = probeIPv6Re.ReplaceAllString(msg, "<ip>")
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "请求失败"
	}
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}
