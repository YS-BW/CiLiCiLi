package watchparty

import (
	"math"
	"sync"
	"time"
)

const (
	// DefaultRateLimit 是每个 IP 在默认时间窗口内允许的请求数。
	DefaultRateLimit = 20
	// DefaultRateWindow 是默认的限流时间窗口。
	DefaultRateWindow = 10 * time.Second
)

type rateBucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter 是简单的内存令牌桶，按 key（端点类型 + 客户端 IP）分别计数。
type rateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*rateBucket
	limit     float64
	window    time.Duration
	now       func() time.Time
	stop      chan struct{}
	closeOnce sync.Once
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	if limit <= 0 {
		limit = DefaultRateLimit
	}
	if window <= 0 {
		window = DefaultRateWindow
	}
	l := &rateLimiter{
		buckets: make(map[string]*rateBucket),
		limit:   float64(limit),
		window:  window,
		now:     time.Now,
		stop:    make(chan struct{}),
	}
	go l.cleanupLoop()
	return l
}

func (l *rateLimiter) allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket := l.buckets[key]
	if bucket == nil {
		bucket = &rateBucket{tokens: l.limit, last: now}
		l.buckets[key] = bucket
	} else {
		elapsed := now.Sub(bucket.last).Seconds()
		if elapsed > 0 {
			bucket.tokens = math.Min(l.limit, bucket.tokens+elapsed*l.limit/l.window.Seconds())
			bucket.last = now
		}
	}
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

func (l *rateLimiter) cleanupLoop() {
	ticker := time.NewTicker(l.window)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.cleanup()
		case <-l.stop:
			return
		}
	}
}

func (l *rateLimiter) cleanup() {
	now := l.now()
	cutoff := 2 * l.window
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, bucket := range l.buckets {
		if now.Sub(bucket.last) >= cutoff {
			delete(l.buckets, key)
		}
	}
}

func (l *rateLimiter) close() {
	l.closeOnce.Do(func() { close(l.stop) })
}
