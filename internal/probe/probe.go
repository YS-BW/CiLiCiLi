// Package probe 定期对所有已启用的点播源做静默探活，并记录健康状态。
//
// 目的：源站挂掉后不必等用户搜索才发现；探活失败会走现有的熔断机制
// （连续 2 次失败 → 30 分钟 → 24 小时 → 长期停用），搜索端只使用存活源。
package probe

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"cilicili/internal/cms"
	"cilicili/internal/model"
	"cilicili/internal/source"
	"cilicili/internal/sources"
	"cilicili/internal/upstream"
)

// Config 是探活参数。
type Config struct {
	Timeout     time.Duration
	Concurrency int
	Keyword     string
	UserAgent   string
}

// Item 是单个源的探活结果。
type Item struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	OK    bool   `json:"ok"`
	MS    int64  `json:"ms"`
	Count int    `json:"count,omitempty"`
	Error string `json:"error,omitempty"`
}

// Result 是一轮探活的结果。
type Result struct {
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt"`
	Total      int    `json:"total"`
	Alive      int    `json:"alive"`
	Dead       int    `json:"dead"`
	Items      []Item `json:"items"`
}

// Prober 周期探活器。
type Prober struct {
	client   upstream.Doer
	registry *source.Registry
	health   source.HealthStore
	store    *sources.Store
	log      *slog.Logger
	cfg      Config

	mu      sync.Mutex
	running bool
	last    *Result
	started bool
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
}

// New 创建探活器。
func New(client upstream.Doer, registry *source.Registry, health source.HealthStore,
	store *sources.Store, log *slog.Logger, cfg Config) *Prober {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 8 * time.Second
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.Keyword == "" {
		cfg.Keyword = "test"
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Prober{
		client: client, registry: registry, health: health, store: store, log: log, cfg: cfg,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// Start 启动后台循环：按库里的间隔（默认 12h）探活，可被 Trigger 提前唤醒。
func (p *Prober) Start(ctx context.Context) {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	p.started = true
	p.mu.Unlock()
	go p.loop(ctx)
}

func (p *Prober) loop(ctx context.Context) {
	defer close(p.done)
	// 启动后先跑一次：把上次运行遗留的坏源尽快标记出来。
	p.wakeOnce()
	for {
		interval := 12 * time.Hour
		if p.store != nil {
			if d, err := p.store.ProbeInterval(); err == nil {
				interval = d
			}
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-p.stop:
			timer.Stop()
			return
		case <-p.wake:
			timer.Stop()
		case <-timer.C:
		}
		p.Run(ctx)
	}
}

// Trigger 立即触发一轮探活（异步）。
func (p *Prober) Trigger() {
	p.wakeOnce()
}

func (p *Prober) wakeOnce() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Stop 停止后台循环。
func (p *Prober) Stop() {
	p.mu.Lock()
	started := p.started
	p.started = false
	p.mu.Unlock()
	if !started {
		return
	}
	close(p.stop)
	<-p.done
}

// Run 同步探活全部启用中的源，返回结果。
func (p *Prober) Run(ctx context.Context) Result {
	p.mu.Lock()
	if p.running {
		last := p.last
		p.mu.Unlock()
		if last != nil {
			return *last
		}
		return Result{}
	}
	p.running = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()

	list := p.registry.List()
	started := time.Now()
	result := Result{StartedAt: started.UnixMilli(), Total: len(list), Items: make([]Item, 0, len(list))}

	type outcome struct {
		index int
		item  Item
	}
	ch := make(chan outcome, len(list))
	sem := make(chan struct{}, p.cfg.Concurrency)
	var wg sync.WaitGroup
	for i, src := range list {
		wg.Add(1)
		go func(i int, src model.SourceConfig) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			item := p.Test(ctx, src.Key, src.Name, src.URL)
			ch <- outcome{index: i, item: item}
		}(i, src)
	}
	go func() { wg.Wait(); close(ch) }()

	items := make([]Item, len(list))
	for r := range ch {
		items[r.index] = r.item
	}
	for _, item := range items {
		if item.OK {
			result.Alive++
		} else {
			result.Dead++
		}
	}
	result.Items = items
	result.FinishedAt = time.Now().UnixMilli()

	p.mu.Lock()
	p.last = &result
	p.mu.Unlock()

	if p.log != nil {
		p.log.Info("source probe finished", "total", result.Total, "alive", result.Alive, "dead", result.Dead,
			"durationMs", result.FinishedAt-result.StartedAt)
	}
	return result
}

// Test 探测单个源：请求一次搜索接口，成功则更新健康状态。
func (p *Prober) Test(ctx context.Context, key, name, rawURL string) Item {
	item := Item{Key: key, Name: name, URL: rawURL}
	start := time.Now()
	res, err := p.client.Do(ctx, searchURL(rawURL, p.cfg.Keyword), upstream.Options{
		Headers:       cms.RequestHeaders(p.cfg.UserAgent),
		Timeout:       p.cfg.Timeout,
		MaxBytes:      8 << 20,
		Retries:       1,
		SafeRedirects: true,
	})
	if err != nil {
		item.MS = time.Since(start).Milliseconds()
		item.Error = probeError(err)
		p.recordFailure(key, item.Error)
		return item
	}
	defer res.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		item.MS = time.Since(start).Milliseconds()
		item.Error = fmt.Sprintf("HTTP %d", res.StatusCode)
		p.recordFailure(key, item.Error)
		return item
	}
	data, err := res.ReadAll(8 << 20)
	if err != nil {
		item.MS = time.Since(start).Milliseconds()
		item.Error = "读取响应失败"
		p.recordFailure(key, item.Error)
		return item
	}
	list, _, err := cms.ParseSearchList(data, model.SourceConfig{Key: key, Name: name, URL: rawURL})
	if err != nil {
		item.MS = time.Since(start).Milliseconds()
		item.Error = "接口返回格式不正确"
		p.recordFailure(key, item.Error)
		return item
	}
	item.MS = time.Since(start).Milliseconds()
	item.OK = true
	item.Count = len(list)
	p.recordSuccess(key, item.MS)
	return item
}

func (p *Prober) recordSuccess(key string, ms int64) {
	if p.health != nil && key != "" {
		p.health.RecordSuccess(key, ms, time.Now())
	}
}

func (p *Prober) recordFailure(key, msg string) {
	if p.health != nil && key != "" {
		p.health.RecordFailure(key, false, time.Now())
	}
}

// Status 返回当前配置、运行状态与最近一轮结果。
type Status struct {
	IntervalHours int     `json:"intervalHours"`
	Running       bool    `json:"running"`
	Last          *Result `json:"last,omitempty"`
}

// Status 返回探活状态（间隔从库里读）。
func (p *Prober) Status() Status {
	hours := 12
	if p.store != nil {
		if d, err := p.store.ProbeInterval(); err == nil {
			hours = int(d.Hours())
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return Status{IntervalHours: hours, Running: p.running, Last: p.last}
}

// SetInterval 修改探活间隔并重启计时。
func (p *Prober) SetInterval(d time.Duration) error {
	if p.store == nil {
		return fmt.Errorf("探活未启用")
	}
	if err := p.store.SetProbeInterval(d); err != nil {
		return err
	}
	p.wakeOnce()
	return nil
}

func searchURL(rawURL, keyword string) string {
	api := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	sep := "?"
	if strings.Contains(api, "?") {
		sep = "&"
	}
	return api + sep + "ac=videolist&wd=" + keyword
}

func probeError(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return "请求超时"
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "name resolution"):
		return "域名解析失败"
	case strings.Contains(msg, "connection refused"):
		return "连接被拒绝"
	case strings.Contains(msg, "tls"):
		return "TLS 握手失败"
	default:
		return "请求失败"
	}
}
