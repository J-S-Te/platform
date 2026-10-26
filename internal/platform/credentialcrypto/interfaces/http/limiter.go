package credentialhttp

import (
	"strings"
	"sync"
	"time"
)

// AccountWindowLimiter applies the existing login account bucket after the
// encrypted envelope is decrypted, so encrypted requests cannot bypass the
// per-account limit by presenting an empty outer JSON body.
type AccountWindowLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	now     func() time.Time
	buckets map[string]windowBucket
}

type windowBucket struct {
	started time.Time
	count   int
}

func NewAccountWindowLimiter(limit int, window time.Duration) *AccountWindowLimiter {
	if limit <= 0 {
		limit = 30
	}
	if window <= 0 {
		window = time.Minute
	}
	return &AccountWindowLimiter{limit: limit, window: window, now: time.Now, buckets: make(map[string]windowBucket)}
}

func (limiter *AccountWindowLimiter) Allow(account string) bool {
	account = strings.ToLower(strings.TrimSpace(account))
	if account == "" || len(account) > 128 {
		return false
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := limiter.now()
	for key, bucket := range limiter.buckets {
		if now.Sub(bucket.started) >= limiter.window {
			delete(limiter.buckets, key)
		}
	}
	bucket := limiter.buckets[account]
	if bucket.started.IsZero() || now.Sub(bucket.started) >= limiter.window {
		bucket = windowBucket{started: now}
	}
	bucket.count++
	limiter.buckets[account] = bucket
	return bucket.count <= limiter.limit
}
