// Package guard shuts out client addresses that keep sending requests the
// server refuses: bad tokens, malformed bodies, limits they ignore. A shut
// out address is refused before anything else is done for its requests.
package guard

import (
	"sync"
	"time"
)

// maxAddrs bounds the addresses tracked; past it the oldest state is dropped.
const maxAddrs = 100_000

// Guard counts refused requests per address. A nil Guard shuts out nobody.
type Guard struct {
	// Failures within Window shut an address out for Ban.
	failures int
	window   time.Duration
	ban      time.Duration
	now      func() time.Time

	mu    sync.Mutex
	addrs map[string]*addr
}

type addr struct {
	start  time.Time // of the window being counted
	count  int
	banned time.Time // until
}

// New returns a guard that shuts an address out for ban after failures
// refused requests within window, or nil if failures is not positive.
func New(failures int, window, ban time.Duration) *Guard {
	if failures <= 0 || window <= 0 || ban <= 0 {
		return nil
	}
	return &Guard{failures: failures, window: window, ban: ban, now: time.Now, addrs: map[string]*addr{}}
}

// Banned reports whether key is shut out, and for how long still.
func (g *Guard) Banned(key string) (bool, time.Duration) {
	if g == nil || key == "" {
		return false, 0
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.addrs[key]
	if !ok || !now.Before(a.banned) {
		return false, 0
	}
	return true, a.banned.Sub(now)
}

// Fail records a refused request of key, and reports whether that shut the
// address out.
func (g *Guard) Fail(key string) bool {
	if g == nil || key == "" {
		return false
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.addrs[key]
	if !ok {
		if len(g.addrs) >= maxAddrs {
			g.sweep(now)
		}
		a = &addr{start: now}
		g.addrs[key] = a
	}
	if now.Sub(a.start) >= g.window {
		a.start, a.count = now, 0
	}
	a.count++
	if a.count >= g.failures && !now.Before(a.banned) {
		a.banned, a.start, a.count = now.Add(g.ban), now, 0
		return true
	}
	return false
}

// Addrs returns the number of addresses tracked.
func (g *Guard) Addrs() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.addrs)
}

// sweep drops addresses neither shut out nor counting; if that frees
// nothing, it drops all that are not shut out.
func (g *Guard) sweep(now time.Time) {
	for key, a := range g.addrs {
		if !now.Before(a.banned) && now.Sub(a.start) >= g.window {
			delete(g.addrs, key)
		}
	}
	if len(g.addrs) < maxAddrs {
		return
	}
	for key, a := range g.addrs {
		if !now.Before(a.banned) {
			delete(g.addrs, key)
		}
	}
}
