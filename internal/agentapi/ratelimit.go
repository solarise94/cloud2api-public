package agentapi

import (
	"sync"
	"time"
)

// rateLimiter is a per-key token bucket keyed on requests_per_minute
// (UPGRADE §5.2). Zero/negative limits are unlimited. In-memory only: the
// deployment is a single instance.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{buckets: map[string]*bucket{}}
}

// allow consumes one token for keyID. rpm <= 0 always allows.
func (l *rateLimiter) allow(keyID string, rpm int) (bool, time.Duration) {
	if rpm <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[keyID]
	if !ok {
		if len(l.buckets) > 4096 {
			// Drop stale buckets so the map cannot grow without bound.
			for k, bb := range l.buckets {
				if now.Sub(bb.last) > 10*time.Minute {
					delete(l.buckets, k)
				}
			}
		}
		b = &bucket{tokens: float64(rpm), last: now}
		l.buckets[keyID] = b
	} else {
		// Refill at rpm tokens per minute since the last visit.
		elapsed := now.Sub(b.last).Seconds()
		b.tokens += elapsed * float64(rpm) / 60
		if b.tokens > float64(rpm) {
			b.tokens = float64(rpm)
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := (1 - b.tokens) * 60 / float64(rpm)
	d := time.Duration(wait * float64(time.Second))
	if d < time.Second {
		d = time.Second
	}
	return false, d
}
