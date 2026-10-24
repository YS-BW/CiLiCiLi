// Command cilicili 是 LibreTV 重写版的后端服务。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cilicili/internal/api"
	"cilicili/internal/auth"
	"cilicili/internal/cache"
	"cilicili/internal/config"
	"cilicili/internal/detail"
	"cilicili/internal/live"
	"cilicili/internal/media"
	"cilicili/internal/providers"
	"cilicili/internal/proxy"
	"cilicili/internal/quality"
	"cilicili/internal/recommend"
	"cilicili/internal/search"
	"cilicili/internal/server"
	"cilicili/internal/source"
	"cilicili/internal/subscribe"
	"cilicili/internal/upstream"
	"cilicili/internal/userdata"
)

// version 由 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Version == "dev" && version != "dev" {
		cfg.Version = version
	}
	log := newLogger(cfg.LogLevel)

	if cfg.Password == "" {
		log.Warn("PASSWORD 未设置：所有需要登录的接口会返回 503")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	if cfg.CachePath != "" {
		if dir := filepath.Dir(cfg.CachePath); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("创建缓存目录失败: %w", err)
			}
		}
	}
	if cfg.UserDB != "" && filepath.Clean(cfg.UserDB) == filepath.Clean(cfg.CachePath) {
		return fmt.Errorf("USER_DB 不能与 CACHE_DB 相同（bbolt 单进程独占）")
	}
	if cfg.MediaDB != "" {
		for _, other := range []string{cfg.CachePath, cfg.UserDB} {
			if other != "" && filepath.Clean(cfg.MediaDB) == filepath.Clean(other) {
				return fmt.Errorf("MEDIA_DB 不能与其他数据库文件相同")
			}
		}
	}

	store, err := cache.New(cache.Config{
		Path:             cfg.CachePath,
		MaxMemoryEntries: 8192,
		Logger:           log,
	})
	if err != nil {
		return fmt.Errorf("打开缓存失败: %w", err)
	}
	defer store.Close()

	userStore, err := userdata.New(cfg.UserDB)
	if err != nil {
		return fmt.Errorf("打开用户数据失败: %w", err)
	}
	defer userStore.Close()

	health, err := source.NewHealthStore(source.HealthConfig{
		Path:      filepath.Join(cfg.DataDir, "health.db"),
		Threshold: 2,
		Ladder:    []time.Duration{30 * time.Minute, 24 * time.Hour},
		Logger:    log,
	})
	if err != nil {
		return fmt.Errorf("打开源健康存储失败: %w", err)
	}
	defer health.Close()

	registry := source.NewRegistry(cfg.DefaultSources, health)

	client, err := upstream.New(upstream.Config{
		UserAgent:             cfg.UserAgent,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ConnectTimeout:        3 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		AllowPrivate:          cfg.LiveAllowPrivate,
		Logger:                log,
	})
	if err != nil {
		return fmt.Errorf("创建出网客户端失败: %w", err)
	}

	searcher := search.New(client, store, health, log, search.Config{
		MaxPages:      cfg.SearchMaxPages,
		SourceTimeout: cfg.SearchSourceTimeout,
		PageTimeout:   cfg.SearchPageTimeout,
		SoftDeadline:  cfg.SearchSoftDeadline,
		HardDeadline:  cfg.SearchHardDeadline,
		CacheFresh:    cfg.SearchCacheFresh,
		CacheStale:    cfg.SearchCacheStale,
		SourceFresh:   30 * time.Minute,
		SourceStale:   6 * time.Hour,
		FailCacheTTL:  time.Minute,
		UserAgent:     cfg.UserAgent,
		FastSources:   cfg.SearchFastSources,
	})
	resolver := detail.New(client, store, log, detail.Config{
		UserAgent:  cfg.UserAgent,
		Timeout:    cfg.RequestTimeout,
		CacheFresh: cfg.DetailCacheFresh,
		CacheStale: cfg.DetailCacheStale,
	})
	qualityProber := quality.New(quality.Options{
		Client:          client,
		Cache:           store,
		Logger:          log,
		UA:              cfg.UserAgent,
		Timeout:         cfg.QualityTimeout,
		Concurrency:     cfg.QualityConcurrency,
		HostConcurrency: 3,
	})
	liveService := live.New(live.Options{
		Client:           client,
		Cache:            store,
		Logger:           log,
		UA:               cfg.UserAgent,
		AllowPrivate:     cfg.LiveAllowPrivate,
		Timeout:          cfg.LiveTimeout,
		ProbeConcurrency: cfg.LiveProbeConcurrency,
	})
	recommendFetcher := recommend.New(recommend.Options{
		Client:       client,
		Cache:        store,
		Logger:       log,
		UA:           cfg.UserAgent,
		SixtyAPIBase: cfg.SixtyAPIBase,
		Timeout:      cfg.RecommendTimeout,
	})
	subscribeFetcher := subscribe.New(subscribe.Options{
		Client:  client,
		Logger:  log,
		Timeout: cfg.RequestTimeout,
	})

	var mediaMgr *media.Manager
	if cfg.MediaEnabled {
		mediaStore, err := media.NewStore(cfg.MediaDB)
		if err != nil {
			return fmt.Errorf("打开媒体库失败: %w", err)
		}
		defer mediaStore.Close()
		var metadataProviders []media.Provider
		tmdb := providers.NewTMDB(client, cfg.TMDBAPIKey, cfg.TMDBReadToken, cfg.TMDBAPIBase)
		if tmdb.Enabled() {
			metadataProviders = append(metadataProviders, tmdb)
		}
		mediaMgr, err = media.NewManager(media.Config{
			Store:       mediaStore,
			Client:      client,
			Providers:   metadataProviders,
			CoverDir:    cfg.MediaCoverDir,
			Logger:      log,
			CoverBase:   cfg.MediaCoverBase,
			AutoConfirm: cfg.MediaAutoConfirm,
		})
		if err != nil {
			return fmt.Errorf("初始化媒体库失败: %w", err)
		}
		mediaMgr.Start(context.Background())
		defer mediaMgr.Stop()
		log.Info("media library enabled", "db", cfg.MediaDB, "covers", cfg.MediaCoverDir, "tmdb", tmdb.Enabled())
	}

	authMgr := auth.New(cfg.Password, cfg.ProxySecret, cfg.TokenTTL)

	proxyHandler := proxy.New(proxy.Options{
		Client:              client,
		UA:                  cfg.UserAgent,
		Auth:                authMgr,
		Logger:              log,
		AnonymousImageHosts: []string{"douban.com", "doubanio.com", "doubanio.viki.moe", "bgm.tv"},
		DoubanReferer:       true,
		LiveAllowPrivate:    cfg.LiveAllowPrivate,
		HeaderTimeout:       cfg.RequestTimeout,
	})
	liveProxy := proxy.NewLive(proxy.Options{
		Client:           client,
		UA:               cfg.UserAgent,
		Auth:             authMgr,
		Logger:           log,
		LiveAllowPrivate: cfg.LiveAllowPrivate,
		HeaderTimeout:    15 * time.Second,
	})

	apiHandler := api.New(api.Deps{
		Cfg:       cfg,
		Log:       log,
		Auth:      authMgr,
		Searcher:  searcher,
		Detail:    resolver,
		Registry:  registry,
		Cache:     store,
		Client:    client,
		Proxy:     proxyHandler,
		LiveProxy: liveProxy,
		Quality:   qualityProber,
		Live:      liveService,
		Recommend: recommendFetcher,
		Subscribe: subscribeFetcher,
		UserData:  userStore,
		Media:     mediaMgr,
		Version:   cfg.Version,
	})

	handler := server.New(server.Deps{
		API:  apiHandler,
		Cfg:  cfg,
		Log:  log,
		Auth: authMgr,
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	log.Info("cilici-core starting",
		"version", cfg.Version,
		"listen", cfg.Listen,
		"sources", len(cfg.DefaultSources),
		"liveSources", len(cfg.DefaultLiveSources),
		"cache", cfg.CachePath,
		"passwordConfigured", cfg.Password != "",
	)

	stopGC := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				authMgr.GC(time.Now())
			case <-stopGC:
				return
			}
		}
	}()

	// 启动后后台预热源：填充健康状态 + 复用 TCP/TLS 连接，降低首个用户搜索的尾延迟。
	go func() {
		warmCtx, warmCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer warmCancel()
		searcher.Warm(warmCtx, registry.List())
		log.Info("source warmup finished")
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		log.Info("shutting down", "signal", sig.String())
	case err := <-errCh:
		close(stopGC)
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
	close(stopGC)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}
