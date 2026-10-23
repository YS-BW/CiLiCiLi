package source

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	// healthAlpha 是延迟 EWMA 的平滑系数。
	healthAlpha = 0.3
	// defaultHealthThreshold 是默认的连续失败次数阈值。
	defaultHealthThreshold = 2
	// healthFlushInterval 是异步落盘间隔；热路径只改内存与脏标记。
	healthFlushInterval = 500 * time.Millisecond
	// healthBoltTimeout 是等待 bbolt 文件锁的时长；同一文件只允许单进程打开。
	healthBoltTimeout = 2 * time.Second
)

var (
	// healthBucket 是 bbolt 中保存健康状态的 bucket。
	healthBucket = []byte("health")
	// defaultHealthLadder 是默认的熔断阶梯：30min -> 24h -> 长期停用。
	defaultHealthLadder = []time.Duration{30 * time.Minute, 24 * time.Hour}
)

// healthStore 是 HealthStore 的线程安全实现。
//
// 热路径（Record*/IsOpen/Get）只操作内存；持久化由后台协程合并写入，
// Close 时做最后一次同步落盘，因此“Close 后再打开”一定能读到最新状态。
//
// 锁约定：mu 保护 states/dirty/db/closed，所有导出方法都通过它做互斥；
// 落盘时不持有 mu（先快照脏集合），避免 bbolt fsync 阻塞搜索热路径。
type healthStore struct {
	mu        sync.Mutex
	threshold int
	ladder    []time.Duration
	states    map[string]HealthState
	dirty     map[string]*HealthState // value 为 nil 表示删除
	logger    *slog.Logger

	db     *bolt.DB
	stop   chan struct{}
	done   chan struct{}
	closed bool
}

// NewHealthStore 创建健康/熔断存储。Path 为空时纯内存；否则用 bbolt 持久化。
func NewHealthStore(cfg HealthConfig) (HealthStore, error) {
	threshold := cfg.Threshold
	if threshold <= 0 {
		threshold = defaultHealthThreshold
	}
	ladder := cfg.Ladder
	if len(ladder) == 0 {
		ladder = defaultHealthLadder
	}
	ladder = append([]time.Duration(nil), ladder...) // 复制，避免调用方后续修改

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	h := &healthStore{
		threshold: threshold,
		ladder:    ladder,
		states:    make(map[string]HealthState),
		dirty:     make(map[string]*HealthState),
		logger:    logger,
	}

	if cfg.Path != "" {
		if dir := filepath.Dir(cfg.Path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return nil, fmt.Errorf("创建健康数据目录 %q: %w", dir, err)
			}
		}
		db, err := bolt.Open(cfg.Path, 0o600, &bolt.Options{Timeout: healthBoltTimeout})
		if err != nil {
			return nil, fmt.Errorf("打开健康数据文件 %q: %w", cfg.Path, err)
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists(healthBucket)
			return err
		}); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("初始化健康 bucket: %w", err)
		}
		if err := h.load(db); err != nil {
			_ = db.Close()
			return nil, err
		}
		h.db = db
		h.stop = make(chan struct{})
		h.done = make(chan struct{})
		go h.flushLoop()
	}

	return h, nil
}

// load 从 bbolt 恢复健康状态；损坏的单条记录跳过，不影响启动。
func (h *healthStore) load(db *bolt.DB) error {
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(healthBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var st HealthState
			if err := json.Unmarshal(v, &st); err != nil {
				h.logger.Warn("健康记录损坏，已忽略", "key", string(k), "err", err)
				return nil
			}
			if st.Key == "" {
				st.Key = string(k)
			}
			h.states[st.Key] = st
			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("读取健康数据: %w", err)
	}
	h.logger.Debug("健康状态恢复完成", "sources", len(h.states))
	return nil
}

// Get 返回健康状态的副本。
func (h *healthStore) Get(key string) (HealthState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.states[key]
	return st, ok
}

// IsOpen 表示该源当前应被跳过（熔断中或长期停用）。
func (h *healthStore) IsOpen(key string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.states[key]
	if !ok {
		return false
	}
	if st.Disabled {
		return true
	}
	return st.OpenUntil > now.UnixMilli()
}

// RecordSuccess 记录一次成功：更新 EWMA、清零连续失败、降一级熔断并解除停用。
func (h *healthStore) RecordSuccess(key string, ms int64, now time.Time) {
	if ms < 0 {
		ms = 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}

	st := h.states[key]
	st.Key = key
	if st.Successes == 0 {
		st.EWMAms = float64(ms)
	} else {
		st.EWMAms = healthAlpha*float64(ms) + (1-healthAlpha)*st.EWMAms
	}
	st.Successes++
	st.FailStreak = 0
	st.LastOK = now.UnixMilli()
	st.OpenUntil = 0
	st.Disabled = false
	if st.Level > 0 {
		st.Level--
	}

	h.states[key] = st
	h.markDirtyLocked(key, st)
}

// RecordFailure 记录一次失败，并在连续失败达到阈值时按 Ladder 升一级熔断：
// 阶梯用尽后进入长期停用（Disabled=true）。返回更新后的状态副本。
//
// 计数规则（与 LibreTV 原实现一致）：
//   - 熔断期内的失败只累加 failStreak，不重复升级、不改变 OpenUntil；
//   - 上一次熔断到期后的第一次失败把 failStreak 重置为 1（停用打断了连续失败链），
//     因此每次升档都需要再来一轮连续失败；
//   - 成功会把 failStreak 清零并降一级。
func (h *healthStore) RecordFailure(key string, timedOut bool, now time.Time) HealthState {
	_ = timedOut // 与真正失败同属失败，超时标记仅供调用方展示

	h.mu.Lock()
	defer h.mu.Unlock()

	st := h.states[key]
	if h.closed {
		st.Key = key
		return st
	}

	nowMs := now.UnixMilli()
	st.Key = key
	if st.OpenUntil != 0 && st.OpenUntil <= nowMs {
		// 上一次熔断刚到期：连续失败链被停用打断，从这次失败重新计数。
		st.FailStreak = 1
		st.OpenUntil = 0
	} else {
		st.FailStreak++
	}
	st.Failures++
	st.LastFail = nowMs

	// 已在熔断期或长期停用时不重复升级（正常搜索会跳过这些源，这里只是防御）。
	alreadyOpen := st.Disabled || st.OpenUntil > nowMs
	if !alreadyOpen && st.FailStreak >= h.threshold {
		h.escalateLocked(&st, now)
	}

	h.states[key] = st
	h.markDirtyLocked(key, st)
	return st
}

// escalateLocked 升一级熔断。调用方必须持有 mu。
func (h *healthStore) escalateLocked(st *HealthState, now time.Time) {
	st.Level++
	if st.Level > len(h.ladder) {
		st.Level = len(h.ladder) + 1
		st.Disabled = true
		st.OpenUntil = 0
		return
	}
	st.Disabled = false
	st.OpenUntil = now.Add(h.ladder[st.Level-1]).UnixMilli()
}

// Reset 清除单个源的健康状态（管理接口的“恢复”语义）。
func (h *healthStore) Reset(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	delete(h.states, key)
	if h.db != nil {
		h.dirty[key] = nil
	}
}

// Snapshot 返回全部健康状态（按 key 升序，均为副本）。
func (h *healthStore) Snapshot(now time.Time) []HealthState {
	h.mu.Lock()
	out := make([]HealthState, 0, len(h.states))
	for _, st := range h.states {
		out = append(out, st)
	}
	h.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Close 停止后台落盘、刷出脏数据并关闭 bbolt。可重复调用。
func (h *healthStore) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	stop, done, db := h.stop, h.done, h.db
	h.mu.Unlock()

	if stop != nil {
		close(stop)
		<-done
	}

	// 循环已退出，此后不会再有并发落盘；取走剩余脏数据做最后一次写入。
	h.mu.Lock()
	pending := h.dirty
	h.dirty = nil
	h.db = nil
	h.states = make(map[string]HealthState)
	h.mu.Unlock()

	if db == nil {
		return nil
	}
	var errs []error
	if len(pending) > 0 {
		if err := applyHealthPending(db, pending); err != nil {
			errs = append(errs, fmt.Errorf("写入健康数据: %w", err))
		}
	}
	if err := db.Close(); err != nil {
		errs = append(errs, fmt.Errorf("关闭健康数据文件: %w", err))
	}
	return errors.Join(errs...)
}

// markDirtyLocked 记录待落盘状态。调用方必须持有 mu。
func (h *healthStore) markDirtyLocked(key string, st HealthState) {
	if h.db == nil || h.closed {
		return
	}
	h.dirty[key] = &st
}

// flushLoop 定期把脏状态合并写入 bbolt。
func (h *healthStore) flushLoop() {
	defer close(h.done)
	ticker := time.NewTicker(healthFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			h.flush()
		}
	}
}

// flush 把当前脏集合快照写入磁盘；失败时把未被新状态覆盖的项放回脏集合等待重试。
func (h *healthStore) flush() {
	h.mu.Lock()
	if h.closed || h.db == nil || len(h.dirty) == 0 {
		h.mu.Unlock()
		return
	}
	pending := h.dirty
	h.dirty = make(map[string]*HealthState)
	db := h.db
	h.mu.Unlock()

	if err := applyHealthPending(db, pending); err != nil {
		h.logger.Warn("健康状态落盘失败，稍后重试", "count", len(pending), "err", err)
		h.mu.Lock()
		if !h.closed && h.db != nil {
			for key, st := range pending {
				if _, newer := h.dirty[key]; !newer {
					h.dirty[key] = st
				}
			}
		}
		h.mu.Unlock()
	}
}

// applyHealthPending 在单个写事务中应用一批状态（nil 表示删除）。
func applyHealthPending(db *bolt.DB, pending map[string]*HealthState) error {
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(healthBucket)
		if b == nil {
			return nil
		}
		for key, st := range pending {
			if st == nil {
				if err := b.Delete([]byte(key)); err != nil {
					return err
				}
				continue
			}
			data, err := json.Marshal(st)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(key), data); err != nil {
				return err
			}
		}
		return nil
	})
}
