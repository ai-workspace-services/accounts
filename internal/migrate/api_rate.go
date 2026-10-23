package migrate

import (
	"crypto/sha256"
	"sync"
	"time"
)

// A listener serves one configured job. The subject digest avoids retaining
// caller identifiers; the fixed cap prevents authenticated cardinality growth.
type migrationRateBucket struct {
	tokens float64
	at     time.Time
}
type migrationRateLimiter struct {
	mu      sync.Mutex
	buckets map[[32]byte]migrationRateBucket
}

func (l *migrationRateLimiter) allow(job, subject string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = make(map[[32]byte]migrationRateBucket)
	}
	key := sha256.Sum256([]byte(job + "\x00" + subject))
	bucket, exists := l.buckets[key]
	if !exists {
		if len(l.buckets) >= 1024 {
			for k, v := range l.buckets {
				if now.Sub(v.at) >= time.Minute {
					delete(l.buckets, k)
				}
			}
			if len(l.buckets) >= 1024 {
				return false
			}
		}
		bucket = migrationRateBucket{tokens: 120, at: now}
	}
	elapsed := now.Sub(bucket.at).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	bucket.tokens += elapsed * 30
	if bucket.tokens > 120 {
		bucket.tokens = 120
	}
	bucket.at = now
	allowed := bucket.tokens >= 1
	if allowed {
		bucket.tokens--
	}
	l.buckets[key] = bucket
	return allowed
}
