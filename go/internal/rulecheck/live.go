package rulecheck

import (
	"sync"
	"time"
)

// Hit is an active check that flags a live call.
type Hit struct {
	Row     Row
	Enforce bool
}

type liveCheck struct {
	row Row
	c   *Compiled
}

// Live evaluates active checks against calls as they happen. It keeps a
// bounded per-session history of earlier calls for require_before. Nothing is
// persisted except firings, which feed live precision.
type Live struct {
	Store  *Store
	Policy func() Policy

	mu       sync.Mutex
	checks   []liveCheck
	loadedAt time.Time
	hist     map[string]*sessionHist
}

type sessionHist struct {
	calls []string
	last  time.Time
}

const (
	maxSessions = 256
	reload      = 20 * time.Second
)

func (l *Live) refresh(now time.Time) {
	if l.Store == nil || (l.checks != nil && now.Sub(l.loadedAt) < reload) {
		return
	}
	rows, err := l.Store.All()
	if err != nil {
		return
	}
	l.checks = l.checks[:0:0]
	for _, r := range rows {
		if r.Status != StatusActive && r.Status != StatusEnforce {
			continue
		}
		if c, err := r.Spec.Compile(); err == nil {
			l.checks = append(l.checks, liveCheck{r, c})
		}
	}
	l.loadedAt = now
	if l.checks == nil {
		l.checks = []liveCheck{}
	}
}

// Invalidate makes the next call reload the active set (after a status change).
func (l *Live) Invalidate() { l.mu.Lock(); l.checks = nil; l.mu.Unlock() }

// Check returns the active checks that flag the call. It records nothing.
func (l *Live) Check(session, tool, target, cwd, agent string) []Hit {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.refresh(now)
	var earlier []string
	if h := l.hist[session]; h != nil {
		earlier = h.calls
	}
	var out []Hit
	for _, lc := range l.checks {
		if lc.c.AppliesTo(tool, cwd, agent) && lc.c.Violates(target, earlier) {
			out = append(out, Hit{Row: lc.row, Enforce: lc.row.Status == StatusEnforce})
		}
	}
	return out
}

// Observe records a finished call: it counts firings of the checks that flagged
// it and then adds it to the session's history. It returns the firing ids.
func (l *Live) Observe(session, tool, target, cwd, agent string) []int64 {
	hits := l.Check(session, tool, target, cwd, agent)
	var ids []int64
	now := time.Now()
	if l.Store != nil {
		for _, h := range hits {
			if id := l.Store.Fire(h.Row.ID, session, now); id != 0 {
				ids = append(ids, id)
			}
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hist == nil {
		l.hist = map[string]*sessionHist{}
	}
	h := l.hist[session]
	if h == nil {
		if len(l.hist) >= maxSessions {
			var oldest string
			var ot time.Time
			for k, v := range l.hist {
				if oldest == "" || v.last.Before(ot) {
					oldest, ot = k, v.last
				}
			}
			delete(l.hist, oldest)
		}
		h = &sessionHist{}
		l.hist[session] = h
	}
	if len(h.calls) >= MaxHistory {
		h.calls = h.calls[1:]
	}
	h.calls = append(h.calls, target)
	h.last = now
	return ids
}
