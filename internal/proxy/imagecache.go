package proxy

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// imageDiskCache 把成功取回的图片按 URL 哈希落盘，二次访问直接命中本地文件，
// 避免每次回源（采集站图床常见 1~2s 首字节）。
//
// 设计要点：
//   - key = sha1(完整 URL)，文件名 <hash>，扩展名记录在旁路元信息里（这里直接用 .img）。
//   - 请求命中前校验 TTL（默认 7 天）：过期则删除并回源。
//   - 并发安全；写盘用临时文件 + rename，避免读到半截文件。
//   - 容量兜底：目录文件数超过上限时按 mtime 淘汰最旧的一批（简单 LRU 近似）。
type imageDiskCache struct {
	dir     string
	ttl     time.Duration
	maxFile int64 // 单文件最大字节（超过不缓存）
	maxNum  int   // 目录内文件数上限

	mu     sync.Mutex
	missMu sync.Mutex
	lastGC time.Time
}

func newImageDiskCache(dir string, ttl time.Duration) *imageDiskCache {
	if dir == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	_ = os.MkdirAll(dir, 0o755)
	const singleMax = 8 << 20 // 单图 8MB 以上不缓存
	const defaultNum = 20000  // 最多 2 万个图片文件
	return &imageDiskCache{dir: dir, ttl: ttl, maxFile: singleMax, maxNum: defaultNum}
}

func (c *imageDiskCache) path(url string) string {
	sum := sha1.Sum([]byte(url))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".img")
}

// get 返回命中的本地文件路径；未命中/过期返回空串。
func (c *imageDiskCache) get(url string) string {
	if c == nil {
		return ""
	}
	p := c.path(url)
	fi, err := os.Stat(p)
	if err != nil {
		return ""
	}
	if c.ttl > 0 && time.Since(fi.ModTime()) > c.ttl {
		_ = os.Remove(p)
		return ""
	}
	return p
}

// put 把已完整读入内存的图片写入缓存（body 由调用方保证是完整图片）。
func (c *imageDiskCache) put(url string, data []byte) {
	if c == nil || int64(len(data)) == 0 || int64(len(data)) > c.maxFile {
		return
	}
	p := c.path(url)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return
	}
	c.maybeGC()
}

// maybeGC 每 10 分钟最多触发一次容量清理（文件数超上限时按 mtime 淘汰 10%）。
func (c *imageDiskCache) maybeGC() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if time.Since(c.lastGC) < 10*time.Minute {
		c.mu.Unlock()
		return
	}
	c.lastGC = time.Now()
	c.mu.Unlock()

	entries, err := os.ReadDir(c.dir)
	if err != nil || len(entries) <= c.maxNum {
		return
	}
	// 按修改时间排序，删最旧的 10%。
	type fi struct {
		name string
		mod  time.Time
	}
	files := make([]fi, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fi{e.Name(), info.ModTime()})
	}
	target := c.maxNum / 10
	if target < 100 {
		target = 100
	}
	// 简单选择：按时间升序排序（少量文件可接受）。
	for i := 0; i < len(files); i++ {
		for j := i + 1; j < len(files); j++ {
			if files[j].mod.Before(files[i].mod) {
				files[i], files[j] = files[j], files[i]
			}
		}
	}
	for i := 0; i < target && i < len(files); i++ {
		_ = os.Remove(filepath.Join(c.dir, files[i].name))
	}
}
