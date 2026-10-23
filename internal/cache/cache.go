package cache

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	// defaultMaxMemoryEntries 是 MaxMemoryEntries 未设置时的内存条目上限。
	defaultMaxMemoryEntries = 4096
	// cleanupInterval 是后台清理过期条目的间隔。
	cleanupInterval = time.Minute
	// boltOpenTimeout 是等待 bbolt 文件锁的时长；同一文件只允许单进程打开。
	boltOpenTimeout = 2 * time.Second
	// recordHeaderSize 是磁盘记录头长度：expiresAt + staleUntil（UnixNano，各 8 字节）。
	recordHeaderSize = 16
)

// cacheBucket 是 bbolt 中保存缓存条目的 bucket。
var cacheBucket = []byte("cache")

// entry 是缓存条目。now < expiresAt 时新鲜；now <= staleUntil 时可作 stale 兜底。
// 条目一旦创建就不可变，因此可以在锁外读取。
type entry struct {
	key        string
	value      []byte
	expiresAt  int64 // UnixNano
	staleUntil int64 // UnixNano
}

// fresh 报告条目在 now 时刻是否仍在新鲜期内。
func (e *entry) fresh(now int64) bool { return now < e.expiresAt }

// live 报告条目在 now 时刻是否还未超过 staleUntil。
func (e *entry) live(now int64) bool { return now <= e.staleUntil }

// cacheStore 是 Store 的实现：进程内 LRU（热）+ bbolt（冷，可选的读穿）。
//
// 锁约定：
//   - mu 保护内存 LRU 与 closed 标志；
//   - dbMu 保护 db 指针的生命周期，读写磁盘时持有，避免磁盘 fsync 阻塞热路径；
//   - 加锁顺序固定为 mu -> dbMu，绝不反向。
type cacheStore struct {
	mu         sync.Mutex
	maxEntries int
	entries    map[string]*list.Element // key -> LRU 元素（*entry）
	lru        *list.List               // 首部最近使用

	dbMu   sync.RWMutex
	db     *bolt.DB
	logger *slog.Logger

	hits      atomic.Uint64
	misses    atomic.Uint64
	staleHits atomic.Uint64

	closed bool
	stop   chan struct{}
	done   chan struct{}
}

// New 创建缓存。Path 为空时是纯内存缓存；否则附带 bbolt 持久化，
// 启动时会把未过 staleUntil 的条目恢复到内存。
func New(cfg Config) (Store, error) {
	maxEntries := cfg.MaxMemoryEntries
	if maxEntries <= 0 {
		maxEntries = defaultMaxMemoryEntries
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	s := &cacheStore{
		maxEntries: maxEntries,
		entries:    make(map[string]*list.Element),
		lru:        list.New(),
		logger:     logger,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}

	if cfg.Path != "" {
		if dir := filepath.Dir(cfg.Path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return nil, fmt.Errorf("创建缓存目录 %q: %w", dir, err)
			}
		}
		db, err := s.openDB(cfg.Path)
		if err != nil {
			return nil, err
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists(cacheBucket)
			return err
		}); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("初始化缓存 bucket: %w", err)
		}
		s.db = db
		if err := s.restore(time.Now()); err != nil {
			_ = db.Close()
			return nil, err
		}
	}

	go s.cleanupLoop()
	return s, nil
}

// openDB 打开缓存文件。缓存是可再生成的派生数据，因此：
//   - 用 NoSync 跳过每次写的 fdatasync（在本机实测可把 Set 从 ~14ms 降到亚毫秒级），
//     进程正常 Close 时再 Sync 一次；进程被杀不会丢数据（数据已写进内核页缓存），
//     只有掉电/系统崩溃可能丢失最近写入，极端情况下文件损坏；
//   - 文件损坏时自动丢弃重建，而不是让服务起不来。
func (s *cacheStore) openDB(path string) (*bolt.DB, error) {
	opts := &bolt.Options{Timeout: boltOpenTimeout, NoSync: true}
	db, err := bolt.Open(path, 0o600, opts)
	if err == nil {
		return db, nil
	}
	if !isCorruptDBError(err) {
		return nil, fmt.Errorf("打开缓存文件 %q: %w", path, err)
	}
	s.logger.Warn("缓存文件损坏，正在重建", "path", path, "err", err)
	if rmErr := os.Remove(path); rmErr != nil {
		return nil, fmt.Errorf("打开缓存文件 %q: %w（重建失败: %v）", path, err, rmErr)
	}
	db, retryErr := bolt.Open(path, 0o600, opts)
	if retryErr != nil {
		return nil, fmt.Errorf("重建缓存文件 %q: %w", path, retryErr)
	}
	return db, nil
}

// isCorruptDBError 判断 bbolt 打开失败是否属于文件损坏（而不是权限/文件锁问题）。
func isCorruptDBError(err error) bool {
	return errors.Is(err, bolt.ErrInvalid) ||
		errors.Is(err, bolt.ErrChecksum) ||
		errors.Is(err, bolt.ErrVersionMismatch)
}

// restore 把磁盘中仍在 stale 期内的条目加载进内存。
// 按 staleUntil 升序插入，使越新鲜的条目在 LRU 中越靠前，超出内存上限时自然淘汰最旧的。
func (s *cacheStore) restore(now time.Time) error {
	nowN := now.UnixNano()
	var items []*entry
	var broken [][]byte

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(cacheBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			e, err := decodeRecord(string(k), v)
			if err != nil || !e.live(nowN) {
				broken = append(broken, append([]byte(nil), k...))
				return nil
			}
			items = append(items, e)
			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("读取缓存文件: %w", err)
	}

	sort.Slice(items, func(i, j int) bool { return items[i].staleUntil < items[j].staleUntil })
	for _, e := range items {
		s.insertLocked(e)
	}
	s.logger.Debug("缓存恢复完成", "restored", len(items), "dropped", len(broken))

	if len(broken) > 0 {
		// 过期/损坏记录顺手清掉，避免磁盘无限增长。
		if err := s.deleteDiskKeys(broken); err != nil {
			s.logger.Warn("清理失效缓存记录失败", "count", len(broken), "err", err)
		}
	}
	return nil
}

// Get 只返回仍在新鲜期内的条目。内存未命中时会读穿磁盘。
func (s *cacheStore) Get(key string) (value []byte, found bool) {
	now := time.Now().UnixNano()

	s.mu.Lock()
	defer s.mu.Unlock()

	if el, ok := s.entries[key]; ok {
		e := el.Value.(*entry)
		if !e.live(now) {
			s.removeElementLocked(el)
			s.misses.Add(1)
			return nil, false
		}
		if e.fresh(now) {
			s.lru.MoveToFront(el)
			s.hits.Add(1)
			return cloneBytes(e.value), true
		}
		// 已过新鲜期：Get 不返回 stale 数据（由 GetStale 负责兜底）。
		s.misses.Add(1)
		return nil, false
	}

	if e, ok := s.loadFromDiskLocked(key); ok {
		if !e.live(now) {
			s.deleteDiskLocked(key)
			s.misses.Add(1)
			return nil, false
		}
		if e.fresh(now) {
			s.insertLocked(e) // 冷数据提升为热数据
			s.hits.Add(1)
			return cloneBytes(e.value), true
		}
	}
	s.misses.Add(1)
	return nil, false
}

// GetStale 返回未超过 staleUntil 的条目；fresh 表示该条目是否仍在新鲜期内。
// 内存未命中时会读穿磁盘。stats 口径：新鲜命中计入 Hits，stale 命中计入 StaleHits，
// 查不到计入 Misses。
func (s *cacheStore) GetStale(key string) (value []byte, fresh bool, found bool) {
	now := time.Now().UnixNano()

	s.mu.Lock()
	defer s.mu.Unlock()

	if el, ok := s.entries[key]; ok {
		e := el.Value.(*entry)
		if e.live(now) {
			s.lru.MoveToFront(el)
			fresh = e.fresh(now)
			if fresh {
				s.hits.Add(1)
			} else {
				s.staleHits.Add(1)
			}
			return cloneBytes(e.value), fresh, true
		}
		s.removeElementLocked(el)
	}

	if e, ok := s.loadFromDiskLocked(key); ok {
		if e.live(now) {
			s.insertLocked(e)
			fresh = e.fresh(now)
			if fresh {
				s.hits.Add(1)
			} else {
				s.staleHits.Add(1)
			}
			return cloneBytes(e.value), fresh, true
		}
		s.deleteDiskLocked(key)
	}

	s.misses.Add(1)
	return nil, false, false
}

// Set 写入内存与磁盘。磁盘写同步完成；失败只记日志，不影响内存缓存。
func (s *cacheStore) Set(key string, value []byte, ttl, staleTTL time.Duration) {
	if ttl < 0 {
		ttl = 0
	}
	if staleTTL < 0 {
		staleTTL = 0
	}
	now := time.Now()
	e := &entry{
		key:        key,
		value:      cloneBytes(value),
		expiresAt:  now.Add(ttl).UnixNano(),
		staleUntil: now.Add(ttl + staleTTL).UnixNano(),
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.insertLocked(e)
	s.mu.Unlock()

	// 磁盘写不持有 mu，避免写事务阻塞 Get 热路径。
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	if s.db == nil {
		return
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(cacheBucket)
		if b == nil {
			return nil
		}
		return b.Put([]byte(key), encodeRecord(e))
	}); err != nil {
		s.logger.Warn("缓存写入磁盘失败", "key", key, "err", err)
	}
}

// Delete 同时删除内存与磁盘条目。
func (s *cacheStore) Delete(key string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.removeMemoryLocked(key)
	s.mu.Unlock()

	s.deleteDiskLocked(key)
}

// Stats 返回统计快照。
func (s *cacheStore) Stats() Stats {
	s.mu.Lock()
	entries := len(s.entries)
	s.mu.Unlock()
	return Stats{
		MemoryEntries: entries,
		Hits:          s.hits.Load(),
		Misses:        s.misses.Load(),
		StaleHits:     s.staleHits.Load(),
	}
}

// Close 停止后台清理、清空内存并关闭 bbolt。可重复调用。
func (s *cacheStore) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	close(s.stop)
	<-s.done

	s.dbMu.Lock()
	db := s.db
	s.db = nil
	var errs []error
	if db != nil {
		// NoSync 模式下由这里补一次落盘，保证正常关闭后重启一定读得到。
		if err := db.Sync(); err != nil {
			errs = append(errs, fmt.Errorf("同步缓存文件: %w", err))
		}
		if err := db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("关闭缓存文件: %w", err))
		}
	}
	s.dbMu.Unlock()

	s.mu.Lock()
	s.entries = make(map[string]*list.Element)
	s.lru.Init()
	s.mu.Unlock()

	return errors.Join(errs...)
}

// cleanupLoop 定期清掉超过 staleUntil 的条目（内存与磁盘）。
func (s *cacheStore) cleanupLoop() {
	defer close(s.done)
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-ticker.C:
			s.cleanup(now.UnixNano())
		}
	}
}

// cleanup 清理过期条目。调用频率低，允许简单遍历。
func (s *cacheStore) cleanup(now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	for el := s.lru.Back(); el != nil; {
		prev := el.Prev()
		if e := el.Value.(*entry); !e.live(now) {
			s.removeElementLocked(el)
		}
		el = prev
	}

	var dead [][]byte
	s.dbMu.RLock()
	db := s.db
	if db != nil {
		if err := db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(cacheBucket)
			if b == nil {
				return nil
			}
			c := b.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				e, err := decodeRecord(string(k), v)
				if err != nil || !e.live(now) {
					dead = append(dead, append([]byte(nil), k...))
				}
			}
			return nil
		}); err != nil {
			s.dbMu.RUnlock()
			s.logger.Warn("扫描过期缓存失败", "err", err)
			return
		}
	}
	s.dbMu.RUnlock()

	if db == nil || len(dead) == 0 {
		return
	}
	if err := s.deleteDiskKeys(dead); err != nil {
		s.logger.Warn("清理过期缓存失败", "count", len(dead), "err", err)
	}
}

// insertLocked 插入或更新条目并刷新 LRU 顺序，必要时淘汰最久未使用的条目。
// 调用方必须持有 mu。
func (s *cacheStore) insertLocked(e *entry) {
	if el, ok := s.entries[e.key]; ok {
		el.Value = e
		s.lru.MoveToFront(el)
		return
	}
	s.entries[e.key] = s.lru.PushFront(e)
	for len(s.entries) > s.maxEntries {
		back := s.lru.Back()
		if back == nil {
			break
		}
		s.removeElementLocked(back)
	}
}

// removeMemoryLocked 从内存 LRU 中删除 key。调用方必须持有 mu。
func (s *cacheStore) removeMemoryLocked(key string) {
	if el, ok := s.entries[key]; ok {
		s.removeElementLocked(el)
	}
}

// removeElementLocked 从内存 LRU 中删除一个元素。调用方必须持有 mu。
func (s *cacheStore) removeElementLocked(el *list.Element) {
	e := el.Value.(*entry)
	delete(s.entries, e.key)
	s.lru.Remove(el)
}

// loadFromDiskLocked 从 bbolt 读取记录（不改动内存）。调用方必须持有 mu。
func (s *cacheStore) loadFromDiskLocked(key string) (*entry, bool) {
	var raw []byte
	s.dbMu.RLock()
	if s.db != nil {
		err := s.db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(cacheBucket)
			if b == nil {
				return nil
			}
			if v := b.Get([]byte(key)); v != nil {
				raw = append([]byte(nil), v...)
			}
			return nil
		})
		s.dbMu.RUnlock()
		if err != nil {
			s.logger.Warn("读取缓存文件失败", "key", key, "err", err)
			return nil, false
		}
	} else {
		s.dbMu.RUnlock()
		return nil, false
	}
	if raw == nil {
		return nil, false
	}
	e, err := decodeRecord(key, raw)
	if err != nil {
		s.logger.Warn("缓存记录损坏，已忽略", "key", key, "err", err)
		s.deleteDiskLocked(key)
		return nil, false
	}
	return e, true
}

// deleteDiskLocked 删除单条磁盘记录。调用方必须持有 mu。
func (s *cacheStore) deleteDiskLocked(key string) {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	if s.db == nil {
		return
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(cacheBucket)
		if b == nil {
			return nil
		}
		return b.Delete([]byte(key))
	}); err != nil {
		s.logger.Warn("删除缓存磁盘记录失败", "key", key, "err", err)
	}
}

// deleteDiskKeys 批量删除磁盘记录（一次写事务）。
func (s *cacheStore) deleteDiskKeys(keys [][]byte) error {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	if s.db == nil {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(cacheBucket)
		if b == nil {
			return nil
		}
		for _, k := range keys {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

// encodeRecord 把条目编码成磁盘格式：16 字节头 + 原始 value。
func encodeRecord(e *entry) []byte {
	buf := make([]byte, recordHeaderSize+len(e.value))
	binary.BigEndian.PutUint64(buf[0:8], uint64(e.expiresAt))
	binary.BigEndian.PutUint64(buf[8:16], uint64(e.staleUntil))
	copy(buf[recordHeaderSize:], e.value)
	return buf
}

// decodeRecord 解析磁盘格式。value 会复制一份，因为 bbolt 的切片只在事务内有效。
func decodeRecord(key string, raw []byte) (*entry, error) {
	if len(raw) < recordHeaderSize {
		return nil, fmt.Errorf("缓存记录 %q 长度非法: %d", key, len(raw))
	}
	value := make([]byte, len(raw)-recordHeaderSize)
	copy(value, raw[recordHeaderSize:])
	e := &entry{
		key:        key,
		value:      value,
		expiresAt:  int64(binary.BigEndian.Uint64(raw[0:8])),
		staleUntil: int64(binary.BigEndian.Uint64(raw[8:16])),
	}
	if e.staleUntil < e.expiresAt {
		e.staleUntil = e.expiresAt
	}
	return e, nil
}

// cloneBytes 复制字节切片，避免调用方与缓存互相篡改数据。
func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
