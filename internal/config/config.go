package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cilicili/internal/model"
)

// Config 是服务端的全部运行配置。所有字段都可以通过环境变量覆盖，
// 环境变量名与 LibreTV 保持兼容，便于平滑迁移。
type Config struct {
	Listen    string
	DataDir   string
	CachePath string

	Version     string
	Password    string
	ProxySecret string
	TokenTTL    time.Duration

	UserAgent      string
	RequestTimeout time.Duration

	SearchMaxPages      int
	SearchFastSources   int
	SearchSourceTimeout time.Duration
	SearchPageTimeout   time.Duration
	SearchSoftDeadline  time.Duration
	SearchHardDeadline  time.Duration
	SearchCacheFresh    time.Duration
	SearchCacheStale    time.Duration

	DetailCacheFresh time.Duration
	DetailCacheStale time.Duration

	QualityCacheFresh time.Duration
	QualityCacheFail  time.Duration

	DefaultSources         []model.SourceConfig
	DefaultLiveSources     []model.LiveSourceConfig
	DefaultSubscriptions   []model.SubscriptionRef
	DefaultRecommendSource string
	DefaultImageMode       string

	LiveAllowPrivate bool
	CorsOrigins      []string
	LogLevel         string
}

// Load 从环境变量读取配置。任何格式错误都会返回 error，而不是静默降级。
func Load() (*Config, error) {
	dataDir := envString("DATA_DIR", "./data")
	cfg := &Config{
		Listen:      envString("LISTEN", ":8080"),
		DataDir:     dataDir,
		CachePath:   envString("CACHE_DB", filepath.Join(dataDir, "cache.db")),
		Version:     envString("APP_VERSION", "dev"),
		Password:    os.Getenv("PASSWORD"),
		ProxySecret: os.Getenv("PROXY_SECRET"),
		TokenTTL:    envDuration("TOKEN_TTL", 90*24*time.Hour),
		UserAgent: envString("USER_AGENT",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"),
		RequestTimeout: envDuration("REQUEST_TIMEOUT", 8*time.Second),

		SearchMaxPages:      envInt("SEARCH_MAX_PAGES", 5),
		SearchFastSources:   envInt("SEARCH_FAST_SOURCES", 8),
		SearchSourceTimeout: envDuration("SEARCH_SOURCE_TIMEOUT_MS", 6*time.Second),
		SearchPageTimeout:   envDuration("SEARCH_PAGE_TIMEOUT_MS", 5*time.Second),
		SearchSoftDeadline:  envDuration("SEARCH_SOFT_DEADLINE_MS", 1200*time.Millisecond),
		SearchHardDeadline:  envDuration("SEARCH_HARD_DEADLINE_MS", 6*time.Second),
		SearchCacheFresh:    envDuration("SEARCH_CACHE_TTL_MS", 5*time.Minute),
		SearchCacheStale:    envDuration("SEARCH_CACHE_STALE_MS", time.Hour),

		DetailCacheFresh: envDuration("DETAIL_CACHE_TTL_MS", 10*time.Minute),
		DetailCacheStale: envDuration("DETAIL_CACHE_STALE_MS", 24*time.Hour),

		QualityCacheFresh: envDuration("QUALITY_CACHE_TTL_MS", 30*24*time.Hour),
		QualityCacheFail:  envDuration("QUALITY_CACHE_FAIL_MS", 2*time.Minute),

		DefaultRecommendSource: envString("DEFAULT_RECOMMEND_SOURCE", ""),
		DefaultImageMode:       envString("DEFAULT_IMAGE_MODE", ""),
		LiveAllowPrivate:       envBool("LIVE_ALLOW_PRIVATE", false),
		CorsOrigins:            splitCSV(envString("CORS_ORIGINS", "")),
		LogLevel:               envString("LOG_LEVEL", "info"),
	}

	// 数值边界：防止环境变量把服务拖垮（例如 10000 页、0 超时）。
	cfg.SearchMaxPages = clamp(cfg.SearchMaxPages, 1, 50)
	cfg.SearchFastSources = clamp(cfg.SearchFastSources, 0, 50)
	cfg.SearchSourceTimeout = clampDuration(cfg.SearchSourceTimeout, time.Second, 30*time.Second)
	cfg.SearchPageTimeout = clampDuration(cfg.SearchPageTimeout, 500*time.Millisecond, 15*time.Second)
	cfg.SearchSoftDeadline = clampDuration(cfg.SearchSoftDeadline, 200*time.Millisecond, 10*time.Second)
	cfg.SearchHardDeadline = clampDuration(cfg.SearchHardDeadline, cfg.SearchSoftDeadline, 30*time.Second)
	cfg.RequestTimeout = clampDuration(cfg.RequestTimeout, time.Second, 60*time.Second)

	if cfg.ProxySecret == "" && cfg.Password != "" {
		// 与 LibreTV 一致：未显式配置时从 PASSWORD 派生；仍建议生产环境显式设置。
		cfg.ProxySecret = "derived:" + cfg.Password
	}

	sources, liveSources, err := loadSources(cfg)
	if err != nil {
		return nil, err
	}
	cfg.DefaultSources = sources
	cfg.DefaultLiveSources = liveSources

	if raw := os.Getenv("DEFAULT_SUBSCRIPTIONS"); strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.DefaultSubscriptions); err != nil {
			return nil, fmt.Errorf("DEFAULT_SUBSCRIPTIONS 不是合法 JSON: %w", err)
		}
	}
	return cfg, nil
}

// SourcesFile 的结构：既可以是 {sources, liveSources} 对象，也可以是裸 sources 数组。
type sourcesFile struct {
	Sources     []model.SourceConfig     `json:"sources"`
	LiveSources []model.LiveSourceConfig `json:"liveSources"`
}

func loadSources(cfg *Config) ([]model.SourceConfig, []model.LiveSourceConfig, error) {
	if p := strings.TrimSpace(os.Getenv("SOURCES_FILE")); p != "" {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, fmt.Errorf("读取 SOURCES_FILE 失败: %w", err)
		}
		trimmed := strings.TrimSpace(string(data))
		if strings.HasPrefix(trimmed, "{") {
			var sf sourcesFile
			if err := json.Unmarshal(data, &sf); err != nil {
				return nil, nil, fmt.Errorf("SOURCES_FILE 格式不正确: %w", err)
			}
			return normalizeSources(sf.Sources), normalizeLiveSources(sf.LiveSources), nil
		}
		var bare []model.SourceConfig
		if err := json.Unmarshal(data, &bare); err != nil {
			return nil, nil, fmt.Errorf("SOURCES_FILE 格式不正确: %w", err)
		}
		return normalizeSources(bare), nil, nil
	}

	var sources []model.SourceConfig
	if raw := strings.TrimSpace(os.Getenv("DEFAULT_SOURCES")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &sources); err != nil {
			return nil, nil, fmt.Errorf("DEFAULT_SOURCES 不是合法 JSON: %w", err)
		}
	}
	var live []model.LiveSourceConfig
	if raw := strings.TrimSpace(os.Getenv("DEFAULT_LIVE_SOURCES")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &live); err != nil {
			return nil, nil, fmt.Errorf("DEFAULT_LIVE_SOURCES 不是合法 JSON: %w", err)
		}
	}
	return normalizeSources(sources), normalizeLiveSources(live), nil
}

func normalizeSources(in []model.SourceConfig) []model.SourceConfig {
	out := make([]model.SourceConfig, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, s := range in {
		s.Name = strings.TrimSpace(s.Name)
		s.URL = strings.TrimRight(strings.TrimSpace(s.URL), "/")
		s.Detail = strings.TrimRight(strings.TrimSpace(s.Detail), "/")
		if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
			continue
		}
		if s.Key == "" {
			s.Key = fmt.Sprintf("env_%d", i)
		}
		if _, ok := seen[s.Key]; ok {
			continue
		}
		seen[s.Key] = struct{}{}
		if s.Name == "" {
			s.Name = hostOf(s.URL)
		}
		out = append(out, s)
	}
	return out
}

func normalizeLiveSources(in []model.LiveSourceConfig) []model.LiveSourceConfig {
	out := make([]model.LiveSourceConfig, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, s := range in {
		s.Name = strings.TrimSpace(s.Name)
		s.URL = strings.TrimSpace(s.URL)
		s.EPG = strings.TrimSpace(s.EPG)
		if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
			continue
		}
		if s.Key == "" {
			s.Key = fmt.Sprintf("live_%d", i)
		}
		if _, ok := seen[s.Key]; ok {
			continue
		}
		seen[s.Key] = struct{}{}
		if s.Name == "" {
			s.Name = hostOf(s.URL)
		}
		out = append(out, s)
	}
	return out
}

func hostOf(raw string) string {
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := envString(key, "")
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

func envBool(key string, fallback bool) bool {
	raw := strings.ToLower(envString(key, ""))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

// envDuration 支持两种写法：纯数字按毫秒解释（兼容 LibreTV 的 *_MS 变量），
// 带单位则按 time.ParseDuration 解析。
func envDuration(key string, fallback time.Duration) time.Duration {
	raw := envString(key, "")
	if raw == "" {
		return fallback
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return time.Duration(n) * time.Millisecond
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return d
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampDuration(v, lo, hi time.Duration) time.Duration {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
