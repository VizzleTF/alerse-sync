// Package ratelimit limits requests per sync chain with token buckets.
package ratelimit

import (
	"sync"
	"time"
)

// sweepInterval is how often idle buckets are dropped.
const sweepInterval = 10 * time.Minute

// Limiter allows each key a number of requests per minute, all of which it
// may spend at once. A nil Limiter allows everything.
type Limiter struct {
	perSecond float64
	burst     float64
	now       func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter of perMinute requests per key, or nil if perMinute
// is not positive.
func New(perMinute int) *Limiter {
	if perMinute <= 0 {
		return nil
	}
	return &Limiter{
		perSecond: float64(perMinute) / 60,
		burst:     float64(perMinute),
		now:       time.Now,
		buckets:   map[string]*bucket{},
	}
}

// Allow takes a request of key. If the key has none left, it returns false
// and how long until it has one.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.perSecond)
	b.last = now
	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) / l.perSecond * float64(time.Second))
		return false, wait
	}
	b.tokens--
	return true, 0
}

// sweep drops buckets that have refilled, which behave like new ones.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepInterval {
		return
	}
	l.lastSweep = now
	for key, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.perSecond >= l.burst {
			delete(l.buckets, key)
		}
	}
}
