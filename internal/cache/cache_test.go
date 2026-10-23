package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newTestStore 创建缓存并在测试结束时关闭（Close 可重复调用）。
func newTestStore(t *testing.T, cfg Config) Store {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSetGetFresh(t *testing.T) {
	s := newTestStore(t, Config{})
	s.Set("k", []byte("value"), time.Hour, time.Hour)

	got, ok := s.Get("k")
	if !ok {
		t.Fatal("Get 应立即命中新鲜条目")
	}
	if string(got) != "value" {
		t.Fatalf("Get = %q, want %q", got, "value")
	}

	got, fresh, ok := s.GetStale("k")
	if !ok || !fresh {
		t.Fatalf("GetStale = (%q, fresh=%v, found=%v), want fresh hit", got, fresh, ok)
	}

	if _, ok := s.Get("missing"); ok {
		t.Fatal("Get 不应命中不存在的 key")
	}
}

func TestTTLFreshThenStaleThenExpired(t *testing.T) {
	s := newTestStore(t, Config{})
	// 新鲜期 25ms，之后还能 stale 兜底 55ms，总计 80ms。
	s.Set("k", []byte("v"), 25*time.Millisecond, 55*time.Millisecond)

	// 新鲜期内 Get 命中。
	if _, ok := s.Get("k"); !ok {
		t.Fatal("新鲜期内 Get 应命中")
	}

	// 等过新鲜期，进入 stale 窗口。
	time.Sleep(40 * time.Millisecond)
	if _, ok := s.Get("k"); ok {
		t.Fatal("超过 TTL 后 Get 不应返回条目")
	}
	got, fresh, ok := s.GetStale("k")
	if !ok {
		t.Fatal("stale 窗口内 GetStale 应命中")
	}
	if fresh {
		t.Fatal("超过 TTL 的条目 GetStale 应标记 fresh=false")
	}
	if string(got) != "v" {
		t.Fatalf("GetStale = %q, want %q", got, "v")
	}

	// 等过 staleUntil，条目彻底过期。
	time.Sleep(50 * time.Millisecond)
	if _, _, ok := s.GetStale("k"); ok {
		t.Fatal("超过 staleUntil 后 GetStale 不应命中")
	}
}

func TestLRUEviction(t *testing.T) {
	s := newTestStore(t, Config{MaxMemoryEntries: 2})
	s.Set("a", []byte("a"), time.Hour, time.Hour)
	s.Set("b", []byte("b"), time.Hour, time.Hour)

	// 访问 a 使其成为最近使用，b 变成最旧。
	if _, ok := s.Get("a"); !ok {
		t.Fatal("Get(a) 应命中")
	}
	s.Set("c", []byte("c"), time.Hour, time.Hour)

	if _, ok := s.Get("b"); ok {
		t.Fatal("b 应已被 LRU 淘汰")
	}
	if _, ok := s.Get("a"); !ok {
		t.Fatal("a 因被访问过不应被淘汰")
	}
	if _, ok := s.Get("c"); !ok {
		t.Fatal("c 刚写入不应被淘汰")
	}
	if got := s.Stats().MemoryEntries; got != 2 {
		t.Fatalf("MemoryEntries = %d, want 2", got)
	}
}

func TestDefaultMaxMemoryEntries(t *testing.T) {
	s := newTestStore(t, Config{})
	total := defaultMaxMemoryEntries + 8
	for i := 0; i < total; i++ {
		s.Set(fmt.Sprintf("k%d", i), []byte("v"), time.Hour, time.Hour)
	}
	if got := s.Stats().MemoryEntries; got != defaultMaxMemoryEntries {
		t.Fatalf("MemoryEntries = %d, want 默认值 %d", got, defaultMaxMemoryEntries)
	}
	if _, ok := s.Get("k0"); ok {
		t.Fatal("最旧的 key 应被淘汰")
	}
	if _, ok := s.Get(fmt.Sprintf("k%d", total-1)); !ok {
		t.Fatal("最新写入的 key 不应被淘汰")
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t, Config{})
	s.Set("k", []byte("v"), time.Hour, time.Hour)
	s.Delete("k")

	if _, ok := s.Get("k"); ok {
		t.Fatal("Delete 后 Get 不应命中")
	}
	if _, _, ok := s.GetStale("k"); ok {
		t.Fatal("Delete 后 GetStale 不应命中")
	}
	s.Delete("not-exist") // 幂等，不应 panic
}

func TestStats(t *testing.T) {
	s := newTestStore(t, Config{})
	if _, ok := s.Get("absent"); ok {
		t.Fatal("不应命中")
	}
	s.Set("k", []byte("v"), 25*time.Millisecond, 55*time.Millisecond)
	if _, ok := s.Get("k"); !ok {
		t.Fatal("应命中新鲜条目")
	}
	if _, _, ok := s.GetStale("k"); !ok {
		t.Fatal("GetStale 应命中新鲜条目")
	}

	time.Sleep(40 * time.Millisecond)
	if _, ok := s.Get("k"); ok {
		t.Fatal("超过 TTL 后 Get 不应命中")
	}
	if _, fresh, ok := s.GetStale("k"); !ok || fresh {
		t.Fatal("应命中 stale 条目且 fresh=false")
	}

	got := s.Stats()
	want := Stats{MemoryEntries: 1, Hits: 2, Misses: 2, StaleHits: 1}
	if got != want {
		t.Fatalf("Stats = %+v, want %+v", got, want)
	}
}

func TestValueIsolation(t *testing.T) {
	s := newTestStore(t, Config{})
	in := []byte("hello")
	s.Set("k", in, time.Hour, time.Hour)

	in[0] = 'X' // 修改写入时的切片，不应影响缓存
	if got, _ := s.Get("k"); string(got) != "hello" {
		t.Fatalf("缓存被写入方篡改: %q", got)
	}

	got, _ := s.Get("k")
	got[0] = 'Y' // 修改返回值，不应影响缓存
	if again, _ := s.Get("k"); string(again) != "hello" {
		t.Fatalf("缓存被读取方篡改: %q", again)
	}

	stale, _, _ := s.GetStale("k")
	stale[0] = 'Z'
	if again, _ := s.Get("k"); string(again) != "hello" {
		t.Fatalf("GetStale 返回的切片篡改了缓存: %q", again)
	}
}

func TestCloseIdempotentAndAfterClose(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.Set("k", []byte("v"), time.Hour, time.Hour)

	if err := s.Close(); err != nil {
		t.Fatalf("首次 Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("重复 Close 应返回 nil: %v", err)
	}

	if _, ok := s.Get("k"); ok {
		t.Fatal("Close 后 Get 不应命中")
	}
	if _, _, ok := s.GetStale("k"); ok {
		t.Fatal("Close 后 GetStale 不应命中")
	}
	s.Set("x", []byte("v"), time.Hour, time.Hour) // no-op，不应 panic
	s.Delete("x")
	if got := s.Stats().MemoryEntries; got != 0 {
		t.Fatalf("Close 后 MemoryEntries = %d, want 0", got)
	}
}

func TestPersistenceRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "cache.db")

	s1, err := New(Config{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s1.Set("k1", []byte("v1"), time.Minute, time.Minute)
	s1.Set("k2", []byte("v2"), time.Minute, time.Minute)
	s1.Delete("k2")
	// k3 在重启时已过新鲜期但仍在 stale 期内，应能通过 GetStale 恢复。
	s1.Set("k3", []byte("v3"), 20*time.Millisecond, 2*time.Second)
	time.Sleep(40 * time.Millisecond)
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("持久化文件未创建: %v", err)
	}

	s2, err := New(Config{Path: path})
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer func() { _ = s2.Close() }()

	got, ok := s2.Get("k1")
	if !ok || string(got) != "v1" {
		t.Fatalf("重启后 Get(k1) = (%q, %v), want (v1, true)", got, ok)
	}
	if _, ok := s2.Get("k3"); ok {
		t.Fatal("重启后已过新鲜期的 k3 不应被 Get 命中")
	}
	got, fresh, ok := s2.GetStale("k3")
	if !ok || fresh || string(got) != "v3" {
		t.Fatalf("重启后 GetStale(k3) = (%q, fresh=%v, found=%v), want stale hit", got, fresh, ok)
	}
	if _, _, ok := s2.GetStale("k2"); ok {
		t.Fatal("已删除的 key 不应在重启后出现")
	}
	if got := s2.Stats().MemoryEntries; got != 2 {
		t.Fatalf("重启后 MemoryEntries = %d, want 2", got)
	}
}

func TestPersistenceReadThroughAfterEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	s := newTestStore(t, Config{Path: path, MaxMemoryEntries: 1})
	s.Set("a", []byte("A"), time.Minute, time.Minute)
	s.Set("b", []byte("B"), time.Minute, time.Minute) // a 被挤出内存，但仍在磁盘

	if got := s.Stats().MemoryEntries; got != 1 {
		t.Fatalf("MemoryEntries = %d, want 1", got)
	}
	got, fresh, ok := s.GetStale("a")
	if !ok || !fresh || string(got) != "A" {
		t.Fatalf("GetStale(a) 应读穿磁盘 = (%q, %v, %v)", got, fresh, ok)
	}
	if got, ok := s.Get("b"); !ok || string(got) != "B" {
		t.Fatalf("Get(b) 应读穿磁盘 = (%q, %v)", got, ok)
	}
}

func TestExpiredDiskEntriesDroppedOnRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	s1, err := New(Config{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s1.Set("k", []byte("v"), 20*time.Millisecond, 20*time.Millisecond)
	time.Sleep(60 * time.Millisecond) // 超过 staleUntil
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := New(Config{Path: path})
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if got := s2.Stats().MemoryEntries; got != 0 {
		t.Fatalf("过期条目不应恢复，MemoryEntries = %d", got)
	}
	if _, _, ok := s2.GetStale("k"); ok {
		t.Fatal("过期条目不应命中")
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := newTestStore(t, Config{MaxMemoryEntries: 64})
	const (
		goroutines = 8
		ops        = 200
	)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				key := fmt.Sprintf("k%d", (g*13+i)%32)
				switch i % 5 {
				case 0:
					s.Set(key, []byte("v"), 50*time.Millisecond, 50*time.Millisecond)
				case 1:
					_, _ = s.Get(key)
				case 2:
					_, _, _ = s.GetStale(key)
				case 3:
					s.Delete(key)
				case 4:
					_ = s.Stats()
				}
			}
		}(g)
	}
	wg.Wait()
	if got := s.Stats().MemoryEntries; got > 64 {
		t.Fatalf("MemoryEntries = %d, 超过上限 %d", got, 64)
	}
}

func TestConcurrentPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	s := newTestStore(t, Config{Path: path, MaxMemoryEntries: 32})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				key := fmt.Sprintf("p%d", (g*7+i)%16)
				switch i % 3 {
				case 0:
					s.Set(key, []byte("v"), time.Minute, time.Minute)
				case 1:
					_, _ = s.Get(key)
				default:
					if _, _, ok := s.GetStale(key); ok {
						s.Delete(key)
					}
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestBackgroundCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	s := newTestStore(t, Config{Path: path})
	s.Set("keep", []byte("keep"), time.Hour, time.Hour)
	s.Set("drop", []byte("drop"), 20*time.Millisecond, 20*time.Millisecond)
	time.Sleep(60 * time.Millisecond)

	// 直接触发一次后台清理，避免等默认的 1 分钟间隔。
	s.(*cacheStore).cleanup(time.Now().UnixNano())

	if got := s.Stats().MemoryEntries; got != 1 {
		t.Fatalf("清理后 MemoryEntries = %d, want 1", got)
	}
	if _, _, ok := s.GetStale("drop"); ok {
		t.Fatal("清理后过期条目不应命中")
	}
	if _, ok := s.Get("keep"); !ok {
		t.Fatal("未过期条目不应被清理")
	}
}

func TestCorruptFileRebuilt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	if err := os.WriteFile(path, []byte("this is not a bbolt database"), 0o600); err != nil {
		t.Fatalf("准备损坏文件: %v", err)
	}

	s1, err := New(Config{Path: path})
	if err != nil {
		t.Fatalf("损坏文件应自动重建而不是报错: %v", err)
	}
	s1.Set("k", []byte("v"), time.Minute, time.Minute)
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := New(Config{Path: path})
	if err != nil {
		t.Fatalf("重建后重新打开: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if got, ok := s2.Get("k"); !ok || string(got) != "v" {
		t.Fatalf("重建后 Get(k) = (%q, %v), want (v, true)", got, ok)
	}
}
