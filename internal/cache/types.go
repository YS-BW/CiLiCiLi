// Package cache 提供「进程内 LRU + 本地持久化 KV」两级 TTL 缓存。
// 持久化只保存可再生成的派生数据（搜索结果/详情/画质/健康状态），不保存用户数据。
package cache

import (
	"log/slog"
	"time"
)

// Config 是缓存构造配置。
type Config struct {
	// Path 是持久化文件路径；为空表示纯内存缓存。
	Path string
	// MaxMemoryEntries 是内存 LRU 的条目上限。
	MaxMemoryEntries int
	Logger           *slog.Logger
}

// Stats 是缓存运行统计。
type Stats struct {
	MemoryEntries int    `json:"memoryEntries"`
	Hits          uint64 `json:"hits"`
	Misses        uint64 `json:"misses"`
	StaleHits     uint64 `json:"staleHits"`
}

// Store 是缓存接口。
type Store interface {
	// Get 返回仍然新鲜的条目。
	Get(key string) (value []byte, found bool)
	// GetStale 返回未超过 stale 期限的条目（可能已不新鲜）。
	GetStale(key string) (value []byte, fresh bool, found bool)
	// Set 写入条目；ttl 是新鲜期，staleTTL 是之后可继续兜底的时间。
	Set(key string, value []byte, ttl, staleTTL time.Duration)
	Delete(key string)
	Stats() Stats
	Close() error
}
