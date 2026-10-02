package cloudsync

import (
	"log"
	"time"
)

// Kick asks the background loop to run a round soon. It never blocks.
func (e *Engine) Kick() {
	e.mu.Lock()
	if e.kick == nil {
		e.kick = make(chan struct{}, 1)
	}
	ch := e.kick
	e.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Debounce is how long the loop waits after the last local edit before a
// round, so a burst of typing becomes one round rather than one per save.
var Debounce = 5 * time.Second

// Run syncs in the background until stop closes: every Interval, soon after
// a local edit, and whenever Kick is called. rev reports the index revision,
// which moves on every note write, so an edit is noticed without hooking
// every write path. Rounds while sync is off cost one settings read.
func (e *Engine) Run(stop <-chan struct{}, rev func() int64) {
	e.mu.Lock()
	if e.kick == nil {
		e.kick = make(chan struct{}, 1)
	}
	kick := e.kick
	e.mu.Unlock()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var lastRound, lastEdit time.Time
	var lastRev int64
	if rev != nil {
		lastRev = rev()
	}
	kicked := true // a first round shortly after start
	for {
		select {
		case <-stop:
			return
		case <-kick:
			kicked = true
		case <-tick.C:
		}
		if e.Folder() == "" {
			kicked = false
			continue
		}
		now := time.Now()
		if rev != nil {
			if v := rev(); v != lastRev {
				lastRev, lastEdit = v, now
			}
		}
		due := kicked || now.Sub(lastRound) >= e.Interval() ||
			(!lastEdit.IsZero() && lastEdit.After(lastRound) && now.Sub(lastEdit) >= Debounce)
		if !due {
			continue
		}
		kicked = false
		if _, err := e.SyncOnce(); err != nil && !IsCode(err, CodeBusy) {
			log.Printf("cloud sync: %v", err)
		}
		// The round's own writes moved the revision; they are not edits.
		if rev != nil {
			lastRev = rev()
		}
		lastRound = time.Now()
	}
}
