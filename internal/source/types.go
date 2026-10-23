// Package source 维护点播源注册表与服务端全局健康/熔断状态。
package source

import (
	"log/slog"
	"sync"
	"time"

	"cilicili/internal/model"
)

// HealthState 是单个源的健康快照（持久化友好，时间用 Unix 毫秒）。
type HealthState struct {
	Key        string  `json:"key"`
	EWMAms     float64 `json:"ewmaMs"`
	FailStreak int     `json:"failStreak"`
	Level      int     `json:"level"`
	LastOK     int64   `json:"lastOk"`
	LastFail   int64   `json:"lastFail"`
	OpenUntil  int64   `json:"openUntil"`
	Disabled   bool    `json:"disabled"`
	Successes  int64   `json:"successes"`
	Failures   int64   `json:"failures"`
}

// HealthConfig 是健康存储的配置。
type HealthConfig struct {
	Path      string
	Threshold int
	// Ladder 是连续失败后的停用阶梯；用尽后进入长期停用。
	Ladder []time.Duration
	Logger *slog.Logger
}

// HealthStore 是线程安全的健康状态存储。Record* / IsOpen 供搜索热路径使用。
type HealthStore interface {
	Get(key string) (HealthState, bool)
	// IsOpen 表示该源当前应被跳过（熔断中或长期停用）。
	IsOpen(key string, now time.Time) bool
	RecordSuccess(key string, ms int64, now time.Time)
	RecordFailure(key string, timedOut bool, now time.Time) HealthState
	Reset(key string)
	Snapshot(now time.Time) []HealthState
	Close() error
}

// Registry 维护部署预置源 + 后续订阅导入源。
type Registry struct {
	mu      sync.RWMutex
	sources []model.SourceConfig
	byKey   map[string]model.SourceConfig
	health  HealthStore
}

// NewRegistry 创建注册表。
func NewRegistry(defaults []model.SourceConfig, health HealthStore) *Registry {
	r := &Registry{
		sources: append([]model.SourceConfig(nil), defaults...),
		byKey:   make(map[string]model.SourceConfig, len(defaults)),
		health:  health,
	}
	for _, s := range defaults {
		r.byKey[s.Key] = s
	}
	return r
}

// List 返回当前源列表的副本。
func (r *Registry) List() []model.SourceConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]model.SourceConfig(nil), r.sources...)
}

// Get 按 key 查询源。
func (r *Registry) Get(key string) (model.SourceConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byKey[key]
	return s, ok
}

// Resolve 将请求里的 source 列表补全为注册表中的规范配置；
// 未在注册表中的源（用户自定义）原样保留，交给 SSRF 校验兜底。
func (r *Registry) Resolve(in []model.SourceConfig) []model.SourceConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]model.SourceConfig, 0, len(in))
	for _, s := range in {
		if canonical, ok := r.byKey[s.Key]; ok && canonical.URL == s.URL {
			out = append(out, canonical)
			continue
		}
		out = append(out, s)
	}
	return out
}

// Replace 整体替换源列表（供订阅同步/管理接口使用）。
func (r *Registry) Replace(sources []model.SourceConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sources = append([]model.SourceConfig(nil), sources...)
	r.byKey = make(map[string]model.SourceConfig, len(sources))
	for _, s := range sources {
		r.byKey[s.Key] = s
	}
}

// Health 返回健康存储。
func (r *Registry) Health() HealthStore { return r.health }
