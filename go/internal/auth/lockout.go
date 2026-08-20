package auth

import (
	"sync"
	"time"
)

// Slowing down password guessing.
//
// The secret vault has had exponential lockout since before this package
// existed, for exactly the reason it applies here: an endpoint that will check
// an unlimited number of passwords is an offline attack conducted online. Login
// shipped without it, which made the weakest credential on the instance — a
// person's password — the easiest one to attack.
//
// Per account AND per source address, because either alone leaves a hole: per
// account only, and one attacker can spray one password across every account;
// per address only, and a botnet walks around it. Failures are held in memory
// rather than in the database — a restart clearing them is acceptable, and a
// write per failed login is a way to turn a guessing attempt into disk load.

// lockoutAfter failures, then a delay that doubles to lockoutMax.
const (
	lockoutAfter = 5
	lockoutBase  = 2 * time.Second
	lockoutMax   = 5 * time.Minute
	lockoutReset = 15 * time.Minute
)

type attempts struct {
	failures int
	last     time.Time
}

type limiter struct {
	mu sync.Mutex
	by map[string]*attempts
}

func newLimiter() *limiter { return &limiter{by: map[string]*attempts{}} }

// retryAfter reports how long this key must wait, zero when it may proceed.
func (l *limiter) retryAfter(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.by[key]
	if a == nil {
		return 0
	}
	if now.Sub(a.last) > lockoutReset {
		delete(l.by, key)
		return 0
	}
	if a.failures < lockoutAfter {
		return 0
	}
	wait := lockoutBase << min(a.failures-lockoutAfter, 10)
	if wait > lockoutMax {
		wait = lockoutMax
	}
	if elapsed := now.Sub(a.last); elapsed < wait {
		return wait - elapsed
	}
	return 0
}

func (l *limiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.by[key]
	if a == nil || now.Sub(a.last) > lockoutReset {
		a = &attempts{}
		l.by[key] = a
	}
	a.failures++
	a.last = now
	// Bound the map: a spray across thousands of invented names must not become
	// a memory leak. Oldest entries go first.
	if len(l.by) > 4096 {
		oldestKey, oldest := "", now
		for k, v := range l.by {
			if v.last.Before(oldest) {
				oldestKey, oldest = k, v.last
			}
		}
		delete(l.by, oldestKey)
	}
}

func (l *limiter) succeed(key string) {
	l.mu.Lock()
	delete(l.by, key)
	l.mu.Unlock()
}
