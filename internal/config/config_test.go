package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"PASSWORD", "PROXY_SECRET", "LISTEN", "DATA_DIR", "CACHE_DB", "SOURCES_FILE",
		"DEFAULT_SOURCES", "DEFAULT_LIVE_SOURCES", "DEFAULT_SUBSCRIPTIONS",
		"SEARCH_MAX_PAGES", "SEARCH_SOURCE_TIMEOUT_MS", "SEARCH_SOFT_DEADLINE_MS",
		"SEARCH_HARD_DEADLINE_MS", "REQUEST_TIMEOUT", "TOKEN_TTL", "APP_VERSION", "USER_DB",
		"USER_AGENT", "LIVE_ALLOW_PRIVATE", "CORS_ORIGINS", "LOG_LEVEL", "DEFAULT_RECOMMEND_SOURCE",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.SearchMaxPages != 5 {
		t.Errorf("SearchMaxPages = %d", cfg.SearchMaxPages)
	}
	if cfg.SearchSourceTimeout != 6*time.Second {
		t.Errorf("SearchSourceTimeout = %v", cfg.SearchSourceTimeout)
	}
	if cfg.SearchSoftDeadline != 1200*time.Millisecond {
		t.Errorf("SearchSoftDeadline = %v", cfg.SearchSoftDeadline)
	}
	if cfg.SearchHardDeadline != 6*time.Second {
		t.Errorf("SearchHardDeadline = %v", cfg.SearchHardDeadline)
	}
	if cfg.CachePath != filepath.Join("./data", "cache.db") {
		t.Errorf("CachePath = %q", cfg.CachePath)
	}
	if cfg.UserDB != filepath.Join("./data", "userdata.db") {
		t.Errorf("UserDB = %q", cfg.UserDB)
	}
	if len(cfg.DefaultSources) != 0 {
		t.Errorf("DefaultSources = %v", cfg.DefaultSources)
	}
}

func TestLoadParsesSources(t *testing.T) {
	clearEnv(t)
	t.Setenv("PASSWORD", "pw")
	t.Setenv("DEFAULT_SOURCES", `[
		{"name":"甲","url":"https://a.example.com/api.php/provide/vod/"},
		{"name":"乙","url":"http://b.example.com/api.php/provide/vod"},
		{"name":"坏","url":"ftp://nope"}
	]`)
	t.Setenv("DEFAULT_LIVE_SOURCES", `[{"name":"直播","url":"https://l.example.com/list.m3u"}]`)
	t.Setenv("SEARCH_MAX_PAGES", "999")
	t.Setenv("SEARCH_SOURCE_TIMEOUT_MS", "100")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.DefaultSources) != 2 {
		t.Fatalf("sources = %d, want 2: %+v", len(cfg.DefaultSources), cfg.DefaultSources)
	}
	if got := cfg.DefaultSources[0].URL; got != "https://a.example.com/api.php/provide/vod" {
		t.Errorf("trailing slash not trimmed: %q", got)
	}
	if cfg.DefaultSources[0].Key != "env_0" || cfg.DefaultSources[1].Key != "env_1" {
		t.Errorf("keys = %q,%q", cfg.DefaultSources[0].Key, cfg.DefaultSources[1].Key)
	}
	if cfg.SearchMaxPages != 50 {
		t.Errorf("SearchMaxPages clamp = %d, want 50", cfg.SearchMaxPages)
	}
	if cfg.SearchSourceTimeout != time.Second {
		t.Errorf("SearchSourceTimeout clamp = %v, want 1s", cfg.SearchSourceTimeout)
	}
	if len(cfg.DefaultLiveSources) != 1 {
		t.Errorf("live sources = %d", len(cfg.DefaultLiveSources))
	}
	if cfg.ProxySecret == "" {
		t.Error("ProxySecret should be derived from PASSWORD")
	}
}

func TestLoadSourcesFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sources.json")
	if err := os.WriteFile(path, []byte(`{
		"name":"test",
		"sources":[{"key":"k1","name":"n1","url":"https://x.example.com/api.php/provide/vod"}],
		"liveSources":[{"name":"l1","url":"https://l.example.com/list.m3u","epg":"https://l.example.com/epg.xml"}]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOURCES_FILE", path)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.DefaultSources) != 1 || cfg.DefaultSources[0].Key != "k1" {
		t.Fatalf("sources = %+v", cfg.DefaultSources)
	}
	if len(cfg.DefaultLiveSources) != 1 || cfg.DefaultLiveSources[0].EPG == "" {
		t.Fatalf("live = %+v", cfg.DefaultLiveSources)
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	clearEnv(t)
	t.Setenv("DEFAULT_SOURCES", `not-json`)
	if _, err := Load(); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadHardDeadlineBelowSoft(t *testing.T) {
	clearEnv(t)
	t.Setenv("SEARCH_SOFT_DEADLINE_MS", "3000")
	t.Setenv("SEARCH_HARD_DEADLINE_MS", "500")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SearchHardDeadline < cfg.SearchSoftDeadline {
		t.Fatalf("hard %v < soft %v", cfg.SearchHardDeadline, cfg.SearchSoftDeadline)
	}
}
