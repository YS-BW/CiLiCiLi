// Package userdata 保存观看进度与收藏。
//
// 这些数据是用户数据，不是可再生成的派生数据：使用独立的 bbolt 文件持久化，
// 不走 cache 包（缓存可能被清理/淘汰），也不加 TTL。
//
// 第一版只有默认用户；表结构不包含 user 维度，后续加账号时再迁移。
package userdata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	progressBucket  = []byte("progress")
	favoritesBucket = []byte("favorites")
)

// Progress 是一条观看进度。ID 固定为 sourceKey:vodId，因此同一影片同一时间只保留一条当前进度。
type Progress struct {
	ID           string  `json:"id"`
	SourceKey    string  `json:"sourceKey"`
	SourceName   string  `json:"sourceName,omitempty"`
	VodID        string  `json:"vodId"`
	Title        string  `json:"title,omitempty"`
	Cover        string  `json:"cover,omitempty"`
	LineID       string  `json:"lineId,omitempty"`
	LineName     string  `json:"lineName,omitempty"`
	EpisodeID    string  `json:"episodeId"`
	EpisodeName  string  `json:"episodeName,omitempty"`
	EpisodeIndex int     `json:"episodeIndex,omitempty"`
	Position     float64 `json:"position"`
	Duration     float64 `json:"duration"`
	Completed    bool    `json:"completed"`
	UpdatedAt    int64   `json:"updatedAt"`
}

// Favorite 是一条收藏。保存标题/封面快照，源失效后仍可展示。
type Favorite struct {
	ID         string `json:"id"`
	SourceKey  string `json:"sourceKey"`
	SourceName string `json:"sourceName,omitempty"`
	VodID      string `json:"vodId"`
	Title      string `json:"title,omitempty"`
	Cover      string `json:"cover,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  int64  `json:"updatedAt"`
}

// Store 是观看进度/收藏的持久化存储。
type Store struct {
	db *bolt.DB
}

// New 打开（或创建）用户数据文件。Path 不能为空，且不能与缓存文件相同。
func New(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("用户数据路径不能为空")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建用户数据目录: %w", err)
		}
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("打开用户数据文件 %q: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(progressBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(favoritesBucket)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化用户数据 bucket: %w", err)
	}
	return &Store{db: db}, nil
}

// MediaID 是影片的稳定标识：sourceKey + ":" + vodId。
func MediaID(sourceKey, vodID string) string {
	return strings.TrimSpace(sourceKey) + ":" + strings.TrimSpace(vodID)
}

// PutProgress 写入/覆盖一条观看进度。
func (s *Store) PutProgress(p Progress, now time.Time) (Progress, error) {
	p.ID = MediaID(p.SourceKey, p.VodID)
	p.UpdatedAt = now.UnixMilli()
	if p.Position < 0 {
		p.Position = 0
	}
	if p.Duration < 0 {
		p.Duration = 0
	}
	if p.Duration > 0 && p.Position > p.Duration {
		p.Position = p.Duration
	}
	if err := s.put(progressBucket, p.ID, p); err != nil {
		return Progress{}, err
	}
	return p, nil
}

// History 返回观看历史，按最近更新时间倒序。unfinished 为 true 时只返回
// 「看了一点但没看完」的条目，用于首页继续观看。
func (s *Store) History(unfinished bool, limit int) ([]Progress, error) {
	items := make([]Progress, 0)
	if err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(progressBucket).ForEach(func(_, v []byte) error {
			var p Progress
			if err := json.Unmarshal(v, &p); err != nil {
				return nil // 跳过损坏条目，不影响其他数据
			}
			if unfinished && !isUnfinished(p) {
				return nil
			}
			items = append(items, p)
			return nil
		})
	}); err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].UpdatedAt != items[j].UpdatedAt {
			return items[i].UpdatedAt > items[j].UpdatedAt
		}
		return items[i].ID < items[j].ID
	})
	return truncateProgress(items, limit), nil
}

// isUnfinished 判断一条进度是否适合出现在「继续观看」。
func isUnfinished(p Progress) bool {
	if p.Completed {
		return false
	}
	if p.Position <= 0 {
		return false
	}
	if p.Duration > 0 && p.Position >= p.Duration*0.95 {
		return false
	}
	return true
}

// DeleteProgress 删除一条历史；返回是否存在。
func (s *Store) DeleteProgress(id string) (bool, error) {
	return s.delete(progressBucket, strings.TrimSpace(id))
}

// Favorites 返回收藏列表，按最近更新时间倒序。
func (s *Store) Favorites(limit int) ([]Favorite, error) {
	items := make([]Favorite, 0)
	if err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(favoritesBucket).ForEach(func(_, v []byte) error {
			var f Favorite
			if err := json.Unmarshal(v, &f); err != nil {
				return nil
			}
			items = append(items, f)
			return nil
		})
	}); err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].UpdatedAt != items[j].UpdatedAt {
			return items[i].UpdatedAt > items[j].UpdatedAt
		}
		return items[i].ID < items[j].ID
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

// PutFavorite 写入/更新收藏；已有记录保留 CreatedAt。
func (s *Store) PutFavorite(f Favorite, now time.Time) (Favorite, error) {
	f.ID = MediaID(f.SourceKey, f.VodID)
	f.UpdatedAt = now.UnixMilli()
	// 读旧值 + 写入放在同一事务里，避免并发收藏同一影片时 CreatedAt 被覆盖。
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(favoritesBucket)
		if raw := b.Get([]byte(f.ID)); raw != nil {
			var old Favorite
			if err := json.Unmarshal(raw, &old); err == nil {
				f.CreatedAt = old.CreatedAt
			}
		}
		if f.CreatedAt == 0 {
			f.CreatedAt = f.UpdatedAt
		}
		raw, err := json.Marshal(f)
		if err != nil {
			return err
		}
		return b.Put([]byte(f.ID), raw)
	}); err != nil {
		return Favorite{}, err
	}
	return f, nil
}

// DeleteFavorite 取消收藏；返回是否存在。
func (s *Store) DeleteFavorite(id string) (bool, error) {
	return s.delete(favoritesBucket, strings.TrimSpace(id))
}

// Close 关闭数据库。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) put(bucket []byte, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put([]byte(key), raw)
	})
}

func (s *Store) delete(bucket []byte, key string) (bool, error) {
	found := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b.Get([]byte(key)) != nil {
			found = true
		}
		return b.Delete([]byte(key))
	})
	return found, err
}

func truncateProgress(items []Progress, limit int) []Progress {
	if limit > 0 && len(items) > limit {
		return items[:limit]
	}
	return items
}
