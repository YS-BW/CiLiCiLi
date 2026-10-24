// Package discover 从公开订阅/接口清单里发现新的点播源，并做存活探测。
//
// 渠道（内置，可用 DISCOVER_SOURCES 覆盖）：
//   - jsDelivr 镜像的 TVBox「纯 API 采集源」配置；
//   - jsDelivr 镜像的苹果CMS 接口清单（base64 文本，一行一个接口）。
//
// raw.githubusercontent.com 在国内（含本项目 VPS）不可达，因此默认全部走 jsDelivr。
package discover

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"cilicili/internal/model"
	"cilicili/internal/probe"
	"cilicili/internal/sources"
	"cilicili/internal/upstream"
)

// DefaultSeeds 是内置的发现渠道。
var DefaultSeeds = []string{
	"https://cdn.jsdelivr.net/gh/muzheya/tvbox-config@main/tvbox-diy.json",
	"https://cdn.jsdelivr.net/gh/YH-Music/vod-sources@main/vod.b64",
}

// SubscriptionFetcher 解析 JSON / TVBox / 接口目录格式的订阅。
type SubscriptionFetcher interface {
	Fetch(ctx context.Context, rawURL string) (*model.SourceListPayload, error)
}

// Candidate 是一个发现到的候选源。
type Candidate struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Alive  bool   `json:"alive"`
	MS     int64  `json:"ms,omitempty"`
	Error  string `json:"error,omitempty"`
	Exists bool   `json:"exists"`
	Seed   string `json:"seed,omitempty"`
}

// SeedResult 是单个渠道的抓取结果。
type SeedResult struct {
	URL   string `json:"url"`
	OK    bool   `json:"ok"`
	Count int    `json:"count"`
	Error string `json:"error,omitempty"`
}

// Result 是一次发现的结果。
type Result struct {
	StartedAt  int64        `json:"startedAt"`
	FinishedAt int64        `json:"finishedAt"`
	Candidates []Candidate  `json:"candidates"`
	Seeds      []SeedResult `json:"seeds"`
	Alive      int          `json:"alive"`
	Total      int          `json:"total"`
	NewAlive   int          `json:"newAlive"`
}

// Discoverer 负责抓取渠道、去重并探测候选源。
type Discoverer struct {
	client  upstream.Doer
	fetcher SubscriptionFetcher
	prober  *probe.Prober
	store   *sources.Store
	log     *slog.Logger
	seeds   []string
	timeout time.Duration

	mu     sync.Mutex
	cached *Result
	at     time.Time
	sync   func() // 自动添加后通知 Registry 热更新

	started bool
	stopCh  chan struct{}
	doneCh  chan struct{}
	running bool
}

// New 创建发现器；seeds 为空时使用 DefaultSeeds。
func New(client upstream.Doer, fetcher SubscriptionFetcher, prober *probe.Prober,
	store *sources.Store, log *slog.Logger, seeds []string) *Discoverer {
	if log == nil {
		log = slog.Default()
	}
	clean := make([]string, 0, len(seeds))
	for _, s := range seeds {
		if s = strings.TrimSpace(s); s != "" {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		clean = append(clean, DefaultSeeds...)
	}
	return &Discoverer{client: client, fetcher: fetcher, prober: prober, store: store, log: log,
		seeds: clean, timeout: 10 * time.Minute, stopCh: make(chan struct{}), doneCh: make(chan struct{})}
}

// Cached 返回最近一次发现结果（用于 apply 校验存活）。
func (d *Discoverer) Cached() *Result {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cached
}

// Run 执行一次发现；refresh=false 且 10 分钟内跑过则直接返回缓存。
func (d *Discoverer) Run(ctx context.Context, refresh bool) *Result {
	d.mu.Lock()
	if !refresh && d.cached != nil && time.Since(d.at) < 10*time.Minute {
		cached := d.cached
		d.mu.Unlock()
		return cached
	}
	d.mu.Unlock()

	started := time.Now()
	result := &Result{StartedAt: started.UnixMilli(), Seeds: make([]SeedResult, 0, len(d.seeds))}

	existing := map[string]bool{}
	if d.store != nil {
		if list, err := d.store.List(); err == nil {
			for _, src := range list {
				existing[src.URL] = true
			}
		}
	}

	seen := map[string]bool{}
	for _, seed := range d.seeds {
		sr := SeedResult{URL: seed}
		items := d.fetchSeed(ctx, seed)
		if items == nil {
			sr.Error = "拉取或解析失败"
		} else {
			sr.OK = true
			sr.Count = len(items)
		}
		result.Seeds = append(result.Seeds, sr)
		for _, it := range items {
			norm, err := sources.NormalizeURL(it.URL)
			if err != nil || seen[norm] {
				continue
			}
			seen[norm] = true
			name := strings.TrimSpace(it.Name)
			if name == "" {
				if u, err := url.Parse(norm); err == nil {
					name = u.Host
				} else {
					name = norm
				}
			}
			result.Candidates = append(result.Candidates, Candidate{
				Name: name, URL: norm, Exists: existing[norm], Seed: seed,
			})
		}
	}
	result.Total = len(result.Candidates)

	// 并发探测（复用探活器，key 为空不写健康状态）。
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range result.Candidates {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c := &result.Candidates[i]
			item := d.prober.Test(ctx, "", c.Name, c.URL)
			c.Alive = item.OK
			c.MS = item.MS
			c.Error = item.Error
		}(i)
	}
	wg.Wait()

	sort.SliceStable(result.Candidates, func(i, j int) bool {
		a, b := result.Candidates[i], result.Candidates[j]
		if a.Alive != b.Alive {
			return a.Alive
		}
		if a.Exists != b.Exists {
			return !a.Exists
		}
		return a.Name < b.Name
	})
	for _, c := range result.Candidates {
		if c.Alive {
			result.Alive++
			if !c.Exists {
				result.NewAlive++
			}
		}
	}
	result.FinishedAt = time.Now().UnixMilli()

	d.mu.Lock()
	d.cached = result
	d.at = time.Now()
	d.mu.Unlock()

	if d.log != nil {
		d.log.Info("source discovery finished", "seeds", len(result.Seeds), "candidates", result.Total,
			"alive", result.Alive, "newAlive", result.NewAlive,
			"durationMs", result.FinishedAt-result.StartedAt)
	}
	return result
}

func (d *Discoverer) fetchSeed(ctx context.Context, seed string) []model.SourceConfig {
	if d.fetcher != nil {
		if payload, err := d.fetcher.Fetch(ctx, seed); err == nil && len(payload.Sources) > 0 {
			return payload.Sources
		}
	}
	// 兜底：按纯文本/base64 接口清单解析（一行一个 CMS 接口）。
	text, err := d.client.GetText(ctx, seed, upstream.Options{Timeout: 15 * time.Second, MaxBytes: 4 << 20, SafeRedirects: true})
	if err != nil {
		return nil
	}
	if items := parseTextList(text); len(items) > 0 {
		return items
	}
	if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text)); err == nil {
		return parseTextList(string(decoded))
	}
	// 再兜底：裸 JSON 数组 [{name,url}]。
	var arr []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal([]byte(text), &arr); err == nil && len(arr) > 0 {
		out := make([]model.SourceConfig, 0, len(arr))
		for _, it := range arr {
			out = append(out, model.SourceConfig{Name: it.Name, URL: it.URL})
		}
		return out
	}
	return nil
}

func parseTextList(text string) []model.SourceConfig {
	out := make([]model.SourceConfig, 0, 16)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if !strings.HasPrefix(line, "http://") && !strings.HasPrefix(line, "https://") {
			continue
		}
		out = append(out, model.SourceConfig{URL: line})
	}
	return out
}

// Start 启动服务端自动发现循环：默认每 7 天跑一次，只把存活的新源写进源列表。
// sync 用于自动添加后热替换 Registry。
func (d *Discoverer) Start(ctx context.Context, sync func()) {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return
	}
	d.started = true
	d.sync = sync
	d.mu.Unlock()
	go d.loop(ctx)
}

func (d *Discoverer) loop(ctx context.Context) {
	defer close(d.doneCh)
	interval := d.interval()
	last := int64(0)
	if d.store != nil {
		last = d.store.DiscoverLastAt()
	}
	if last == 0 || time.Since(time.UnixMilli(last)) >= interval {
		d.runOnce(ctx)
	}
	for {
		wait := d.interval()
		if last := d.store.DiscoverLastAt(); last > 0 {
			if remain := time.Until(time.UnixMilli(last).Add(wait)); remain > 0 {
				wait = remain
			} else {
				wait = 0
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-d.stopCh:
			timer.Stop()
			return
		case <-timer.C:
			d.runOnce(ctx)
		}
	}
}

func (d *Discoverer) interval() time.Duration {
	if d.store == nil {
		return 7 * 24 * time.Hour
	}
	if v, err := d.store.DiscoverInterval(); err == nil {
		return v
	}
	return 7 * 24 * time.Hour
}

// Stop 停止自动发现循环。
func (d *Discoverer) Stop() {
	d.mu.Lock()
	started := d.started
	d.started = false
	d.mu.Unlock()
	if !started {
		return
	}
	close(d.stopCh)
	<-d.doneCh
}

// runOnce 执行一次发现，并自动添加存活且不在黑名单里的新源。
func (d *Discoverer) runOnce(ctx context.Context) *Result {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return nil
	}
	d.running = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.running = false
		d.mu.Unlock()
	}()

	result := d.Run(ctx, true)
	if result == nil {
		return nil
	}
	alive := make([]Candidate, 0, len(result.Candidates))
	for _, c := range result.Candidates {
		if c.Alive && !c.Exists {
			alive = append(alive, c)
		}
	}
	const maxAutoAdd = 20
	if len(alive) > maxAutoAdd {
		alive = alive[:maxAutoAdd]
	}
	added, skipped, err := d.Apply(ctx, alive)
	if err != nil {
		if d.log != nil {
			d.log.Warn("auto discover apply failed", "err", err)
		}
		return result
	}
	if d.store != nil {
		_ = d.store.SetDiscoverResult(time.Now().UnixMilli(), added)
	}
	d.mu.Lock()
	sync := d.sync
	d.mu.Unlock()
	if added > 0 && sync != nil {
		sync()
	}
	if d.log != nil {
		d.log.Info("auto discover finished", "candidates", result.Total, "alive", result.Alive,
			"added", added, "skipped", skipped, "intervalHours", int(d.interval().Hours()))
	}
	return result
}

// Status 返回自动发现状态（供 API/排障）。
type Status struct {
	IntervalHours int   `json:"intervalHours"`
	LastAt        int64 `json:"lastAt"`
	LastAdded     int   `json:"lastAdded"`
	Running       bool  `json:"running"`
}

// Status 返回自动发现状态。
func (d *Discoverer) Status() Status {
	st := Status{IntervalHours: int(d.interval().Hours())}
	if d.store != nil {
		st.LastAt = d.store.DiscoverLastAt()
		st.LastAdded = d.store.DiscoverLastAdded()
	}
	d.mu.Lock()
	st.Running = d.running
	d.mu.Unlock()
	return st
}

// Apply 把存活且不存在的候选写入源列表。
func (d *Discoverer) Apply(ctx context.Context, candidates []Candidate) (added, skipped int, err error) {
	if d.store == nil {
		return 0, 0, errors.New("源配置未启用")
	}
	list, err := d.store.List()
	if err != nil {
		return 0, 0, err
	}
	existing := make(map[string]bool, len(list))
	for _, src := range list {
		existing[src.URL] = true
	}
	blocked := map[string]bool{}
	if d.store != nil {
		if b, err := d.store.BlockedURLs(); err == nil {
			blocked = b
		}
	}
	for _, c := range candidates {
		norm, normErr := sources.NormalizeURL(c.URL)
		if normErr != nil || existing[norm] || blocked[norm] {
			skipped++
			continue
		}
		name := strings.TrimSpace(c.Name)
		if name == "" {
			name = norm
		}
		if _, err := d.store.Create(sources.Source{SourceConfig: model.SourceConfig{Name: name, URL: norm}}); err != nil {
			skipped++
			continue
		}
		existing[norm] = true
		added++
	}
	return added, skipped, nil
}
