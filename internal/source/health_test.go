package source

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func newHealthStoreForTest(t *testing.T, cfg HealthConfig) HealthStore {
	t.Helper()
	h, err := NewHealthStore(cfg)
	if err != nil {
		t.Fatalf("NewHealthStore(%+v): %v", cfg, err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// recordFailuresForTest 连续记录 n 次失败并返回最后一次的状态。
func recordFailuresForTest(h HealthStore, key string, n int, now time.Time) HealthState {
	var st HealthState
	for i := 0; i < n; i++ {
		st = h.RecordFailure(key, false, now)
	}
	return st
}

func TestDefaultsAndFirstCircuit(t *testing.T) {
	h := newHealthStoreForTest(t, HealthConfig{})
	now := time.Now()

	st := recordFailuresForTest(h, "s1", 1, now)
	if st.Level != 0 || st.FailStreak != 1 || st.Failures != 1 {
		t.Fatalf("首次失败后状态异常: %+v", st)
	}
	if h.IsOpen("s1", now) {
		t.Fatal("未达阈值不应熔断")
	}

	st = recordFailuresForTest(h, "s1", 1, now)
	if st.Level != 1 {
		t.Fatalf("连续 2 次失败应升到 level 1, got %d", st.Level)
	}
	if st.FailStreak != 2 {
		t.Fatalf("熔断后 failStreak 应保留为阈值, got %d", st.FailStreak)
	}
	if st.Failures != 2 {
		t.Fatalf("Failures = %d, want 2", st.Failures)
	}
	if st.Disabled {
		t.Fatal("第一档不应长期停用")
	}
	if !h.IsOpen("s1", now) {
		t.Fatal("熔断期内 IsOpen 应为 true")
	}
	if h.IsOpen("s1", now.Add(29*time.Minute)) != true {
		t.Fatal("30 分钟阶梯内应保持熔断")
	}
	if h.IsOpen("s1", now.Add(31*time.Minute)) {
		t.Fatal("30 分钟阶梯结束后应恢复")
	}
	if h.IsOpen("unknown", now) {
		t.Fatal("未知源不应被视为熔断")
	}

	stored, ok := h.Get("s1")
	if !ok {
		t.Fatal("Get 应返回已记录的状态")
	}
	wantUntil := now.Add(30 * time.Minute)
	if diff := time.UnixMilli(stored.OpenUntil).Sub(wantUntil); diff < -time.Second || diff > time.Second {
		t.Fatalf("OpenUntil 偏差过大: %v", diff)
	}
	if stored.LastFail != now.UnixMilli() {
		t.Fatalf("LastFail = %d, want %d", stored.LastFail, now.UnixMilli())
	}
}

func TestLadderAndLongTermDisable(t *testing.T) {
	h := newHealthStoreForTest(t, HealthConfig{})

	t0 := time.Now()
	st := recordFailuresForTest(h, "s", 2, t0)
	if st.Level != 1 {
		t.Fatalf("level = %d, want 1", st.Level)
	}
	want1 := t0.Add(defaultHealthLadder[0])
	if diff := time.UnixMilli(st.OpenUntil).Sub(want1); diff < -time.Second || diff > time.Second {
		t.Fatalf("第一档结束时间偏差过大: %v", diff)
	}

	// 30min 阶梯结束后再来一轮连续失败 -> level 2（24h）。
	t1 := t0.Add(31 * time.Minute)
	st = recordFailuresForTest(h, "s", 2, t1)
	if st.Level != 2 || st.Disabled {
		t.Fatalf("第二档状态异常: %+v", st)
	}
	want2 := t1.Add(defaultHealthLadder[1])
	if diff := time.UnixMilli(st.OpenUntil).Sub(want2); diff < -time.Second || diff > time.Second {
		t.Fatalf("第二档结束时间偏差过大: %v", diff)
	}
	if h.IsOpen("s", t1.Add(defaultHealthLadder[1]+time.Minute)) {
		t.Fatal("24h 阶梯结束后应恢复")
	}

	// 24h 结束后再来一轮 -> 阶梯用尽，长期停用。
	t2 := t1.Add(25 * time.Hour)
	st = recordFailuresForTest(h, "s", 2, t2)
	if st.Level != len(defaultHealthLadder)+1 {
		t.Fatalf("长期停用 level = %d, want %d", st.Level, len(defaultHealthLadder)+1)
	}
	if !st.Disabled {
		t.Fatal("阶梯用尽后应 Disabled=true")
	}
	if st.OpenUntil != 0 {
		t.Fatalf("长期停用不应设置 OpenUntil, got %d", st.OpenUntil)
	}
	if !h.IsOpen("s", t2.Add(30*24*time.Hour)) {
		t.Fatal("长期停用后 IsOpen 应始终为 true")
	}

	// 停用状态下继续失败不应让 level 越界。
	st = recordFailuresForTest(h, "s", 2, t2.Add(time.Hour))
	if st.Level != len(defaultHealthLadder)+1 || !st.Disabled {
		t.Fatalf("停用后继续失败改写异常: %+v", st)
	}
}

func TestRecordSuccessDowngradeAndEWMA(t *testing.T) {
	h := newHealthStoreForTest(t, HealthConfig{})
	t0 := time.Now()

	recordFailuresForTest(h, "s", 2, t0) // level 1
	t1 := t0.Add(time.Hour)
	h.RecordSuccess("s", 100, t1)

	st, ok := h.Get("s")
	if !ok {
		t.Fatal("Get 应命中")
	}
	if st.Level != 0 {
		t.Fatalf("成功应降一级到 0, got %d", st.Level)
	}
	if st.OpenUntil != 0 || st.Disabled {
		t.Fatalf("成功应清理熔断/停用: %+v", st)
	}
	if st.FailStreak != 0 {
		t.Fatalf("成功应清零 failStreak, got %d", st.FailStreak)
	}
	if st.Successes != 1 || st.EWMAms != 100 {
		t.Fatalf("首次成功 EWMA 应为样本值 100: %+v", st)
	}
	if st.LastOK != t1.UnixMilli() {
		t.Fatalf("LastOK = %d, want %d", st.LastOK, t1.UnixMilli())
	}
	if h.IsOpen("s", t1) {
		t.Fatal("成功后不应熔断")
	}

	// EWMA: 0.3*300 + 0.7*100 = 160。
	h.RecordSuccess("s", 300, t1.Add(time.Minute))
	st, _ = h.Get("s")
	want := healthAlpha*300 + (1-healthAlpha)*100
	if math.Abs(st.EWMAms-want) > 1e-9 {
		t.Fatalf("EWMA = %v, want %v", st.EWMAms, want)
	}
	if st.Successes != 2 {
		t.Fatalf("Successes = %d, want 2", st.Successes)
	}
}

func TestRecordSuccessRearmsDisabled(t *testing.T) {
	h := newHealthStoreForTest(t, HealthConfig{})
	t0 := time.Now()

	// 打到长期停用。
	t1 := t0.Add(31 * time.Minute)
	t2 := t1.Add(25 * time.Hour)
	recordFailuresForTest(h, "s", 2, t0)
	recordFailuresForTest(h, "s", 2, t1)
	st := recordFailuresForTest(h, "s", 2, t2)
	if !st.Disabled {
		t.Fatalf("前置条件失败，期望停用: %+v", st)
	}

	h.RecordSuccess("s", 50, t2.Add(time.Hour))
	st, _ = h.Get("s")
	if st.Disabled {
		t.Fatal("成功一次应清除 Disabled")
	}
	if st.Level != len(defaultHealthLadder) {
		t.Fatalf("成功应只降一级到 %d, got %d", len(defaultHealthLadder), st.Level)
	}
	if st.OpenUntil != 0 {
		t.Fatalf("成功应清理 OpenUntil, got %d", st.OpenUntil)
	}
	if h.IsOpen("s", t2.Add(2*time.Hour)) {
		t.Fatal("成功降级后不应再熔断")
	}
}

func TestCustomThresholdAndLadder(t *testing.T) {
	h := newHealthStoreForTest(t, HealthConfig{
		Threshold: 1,
		Ladder:    []time.Duration{time.Minute},
	})
	now := time.Now()

	st := h.RecordFailure("s", true, now) // timedOut 也计为失败
	if st.Level != 1 || st.Failures != 1 {
		t.Fatalf("Threshold=1 时一次失败应熔断: %+v", st)
	}
	if !h.IsOpen("s", now) {
		t.Fatal("应处于熔断中")
	}
	if diff := time.UnixMilli(st.OpenUntil).Sub(now.Add(time.Minute)); diff < -time.Second || diff > time.Second {
		t.Fatalf("自定义阶梯偏差过大: %v", diff)
	}

	st = recordFailuresForTest(h, "s", 1, now.Add(2*time.Minute))
	if st.Level != 2 {
		t.Fatalf("阶梯用尽后 level = %d, want 2", st.Level)
	}
	if !st.Disabled {
		t.Fatal("自定义单档阶梯用尽后应长期停用")
	}
}

func TestReset(t *testing.T) {
	h := newHealthStoreForTest(t, HealthConfig{})
	now := time.Now()
	recordFailuresForTest(h, "s", 2, now)
	if !h.IsOpen("s", now) {
		t.Fatal("前置失败：应处于熔断中")
	}

	h.Reset("s")
	if _, ok := h.Get("s"); ok {
		t.Fatal("Reset 后 Get 不应命中")
	}
	if h.IsOpen("s", now) {
		t.Fatal("Reset 后不应熔断")
	}
	if got := h.Snapshot(now); len(got) != 0 {
		t.Fatalf("Reset 后 Snapshot = %+v, want 空", got)
	}
	h.Reset("s") // 重复 Reset 幂等
}

func TestSnapshotSortedAndCopied(t *testing.T) {
	h := newHealthStoreForTest(t, HealthConfig{})
	now := time.Now()

	h.RecordSuccess("c", 10, now)
	h.RecordSuccess("a", 20, now)
	recordFailuresForTest(h, "b", 1, now)

	snap := h.Snapshot(now)
	if len(snap) != 3 {
		t.Fatalf("Snapshot 长度 = %d, want 3", len(snap))
	}
	for i, want := range []string{"a", "b", "c"} {
		if snap[i].Key != want {
			t.Fatalf("Snapshot[%d].Key = %q, want %q", i, snap[i].Key, want)
		}
	}

	// 修改快照不应影响内部状态。
	snap[0].Key = "hacked"
	snap[0].Level = 99
	st, ok := h.Get("a")
	if !ok || st.Key != "a" || st.Level != 0 {
		t.Fatalf("Snapshot 未返回副本: %+v", st)
	}
	if st.LastOK != now.UnixMilli() {
		t.Fatalf("LastOK 应为 Unix 毫秒 %d, got %d", now.UnixMilli(), st.LastOK)
	}

	// Get 同样返回副本。
	st, _ = h.Get("a")
	st.Level = 42
	if again, _ := h.Get("a"); again.Level != 0 {
		t.Fatal("Get 返回的副本被修改后影响了内部状态")
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "health.db")
	t0 := time.Now()

	h1, err := NewHealthStore(HealthConfig{Path: path, Logger: nil})
	if err != nil {
		t.Fatalf("NewHealthStore: %v", err)
	}
	recordFailuresForTest(h1, "b", 2, t0) // level 1
	h1.RecordSuccess("a", 123, t0)
	recordFailuresForTest(h1, "c", 1, t0) // 未达阈值
	if err := h1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("持久化文件未创建: %v", err)
	}

	h2, err := NewHealthStore(HealthConfig{Path: path})
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	snap := h2.Snapshot(time.Now())
	if len(snap) != 3 {
		t.Fatalf("重启后 Snapshot 长度 = %d, want 3: %+v", len(snap), snap)
	}
	b, ok := h2.Get("b")
	if !ok || b.Level != 1 || b.Failures != 2 || b.Disabled || b.OpenUntil == 0 {
		t.Fatalf("重启后 b 状态异常: %+v", b)
	}
	a, ok := h2.Get("a")
	if !ok || a.EWMAms != 123 || a.Successes != 1 || a.LastOK != t0.UnixMilli() {
		t.Fatalf("重启后 a 状态异常: %+v", a)
	}
	c, ok := h2.Get("c")
	if !ok || c.FailStreak != 1 || c.Failures != 1 || c.Level != 0 {
		t.Fatalf("重启后 c 状态异常: %+v", c)
	}

	// Reset 也必须落盘。
	h2.Reset("b")
	if err := h2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	h3, err := NewHealthStore(HealthConfig{Path: path})
	if err != nil {
		t.Fatalf("第三次打开: %v", err)
	}
	defer func() { _ = h3.Close() }()
	if _, ok := h3.Get("b"); ok {
		t.Fatal("Reset 后的源不应在重启后出现")
	}
	if _, ok := h3.Get("a"); !ok {
		t.Fatal("未 Reset 的源应保留")
	}
}

func TestCloseIdempotentAndAfterClose(t *testing.T) {
	h, err := NewHealthStore(HealthConfig{})
	if err != nil {
		t.Fatalf("NewHealthStore: %v", err)
	}
	now := time.Now()
	h.RecordFailure("s", false, now)
	if err := h.Close(); err != nil {
		t.Fatalf("首次 Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("重复 Close 应返回 nil: %v", err)
	}
	if _, ok := h.Get("s"); ok {
		t.Fatal("Close 后 Get 不应命中")
	}
	if h.IsOpen("s", now) {
		t.Fatal("Close 后 IsOpen 应为 false")
	}
	h.RecordSuccess("s", 1, now) // no-op，不应 panic
	h.RecordFailure("s", false, now)
	h.Reset("s")
	if got := h.Snapshot(now); len(got) != 0 {
		t.Fatalf("Close 后 Snapshot = %+v, want 空", got)
	}
}

func TestConcurrentAccess(t *testing.T) {
	h := newHealthStoreForTest(t, HealthConfig{Path: filepath.Join(t.TempDir(), "health.db")})
	const (
		goroutines = 8
		ops        = 150
	)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			now := time.Now()
			for i := 0; i < ops; i++ {
				key := fmt.Sprintf("s%d", (g*5+i)%6)
				switch i % 6 {
				case 0:
					h.RecordSuccess(key, int64(i), now)
				case 1:
					h.RecordFailure(key, i%2 == 0, now)
				case 2:
					_ = h.IsOpen(key, now)
				case 3:
					_, _ = h.Get(key)
				case 4:
					_ = h.Snapshot(now)
				case 5:
					if i%30 == 0 {
						h.Reset(key)
					}
				}
			}
		}(g)
	}
	wg.Wait()
	if got := len(h.Snapshot(time.Now())); got > 6 {
		t.Fatalf("Snapshot 条目数 = %d, want <= 6", got)
	}
}

func TestAsyncFlush(t *testing.T) {
	h := newHealthStoreForTest(t, HealthConfig{Path: filepath.Join(t.TempDir(), "health.db")})
	now := time.Now()
	recordFailuresForTest(h, "s", 2, now)

	hs := h.(*healthStore)
	hs.flush() // 不等 500ms 后台周期，直接触发一次

	var raw []byte
	hs.mu.Lock()
	err := hs.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(healthBucket); b != nil {
			if v := b.Get([]byte("s")); v != nil {
				raw = append([]byte(nil), v...)
			}
		}
		return nil
	})
	hs.mu.Unlock()
	if err != nil {
		t.Fatalf("读取落盘数据: %v", err)
	}
	var st HealthState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("解析落盘数据: %v", err)
	}
	if st.Level != 1 || st.Failures != 2 || st.OpenUntil == 0 {
		t.Fatalf("异步落盘状态异常: %+v", st)
	}
}

func TestFailureWhileOpenDoesNotEscalate(t *testing.T) {
	h := newHealthStoreForTest(t, HealthConfig{})
	now := time.Now()
	recordFailuresForTest(h, "s", 2, now) // level 1，30min

	before, _ := h.Get("s")
	recordFailuresForTest(h, "s", 2, now.Add(5*time.Minute))
	after, _ := h.Get("s")
	if after.Level != 1 {
		t.Fatalf("熔断期内失败不应升级, level = %d", after.Level)
	}
	if after.OpenUntil != before.OpenUntil {
		t.Fatalf("熔断期内失败不应改变 OpenUntil: %d != %d", after.OpenUntil, before.OpenUntil)
	}
	if after.FailStreak != 4 {
		t.Fatalf("熔断期内 failStreak 应继续累加, got %d", after.FailStreak)
	}

	// 熔断到期后的第一次失败：连续失败链被停用打断，从 1 重新计数且不越级。
	st := h.RecordFailure("s", false, now.Add(31*time.Minute))
	if st.FailStreak != 1 || st.Level != 1 || st.OpenUntil != 0 {
		t.Fatalf("到期后第一次失败状态异常: %+v", st)
	}
	// 再来一次才升到第二档。
	st = h.RecordFailure("s", false, now.Add(31*time.Minute))
	if st.Level != 2 || st.Disabled {
		t.Fatalf("到期后第二轮失败应升到 level 2: %+v", st)
	}
}
