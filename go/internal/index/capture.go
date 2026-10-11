package index

// Retroactive importance: synaptic tagging and capture (LAB N6, flag "capture").
//
// A fact's importance is fixed when it is written, but what mattered is often
// only known later. When a strong event lands on a fact F, the low-importance
// facts written close to F and sharing its scope were probably part of the same
// story, so they earn a bounded, decaying boost.
//
// Events are human-authored supersessions: a person wrote F and it replaced at
// least one earlier belief. That covers a person resolving a dispute and a
// person correcting a fact. There is no surprise (prediction-error) signal in
// the store, so an agent's update is never an event here.
//
// Targets are live facts that rank at 3 or below, were not written by a person,
// and are neither hidden nor pulled from an untrusted origin. A target and its
// event share scope when they are in the same session (weight 1), in the same
// note (weight 1), or share an entity (weight 0.5). The score of a target is the
// maximum over events of
//
//	weight * 0.5^(|t_target - t_event| / captureHalfLife)
//
// for events within captureWindow. A maximum, not a sum, so a second event
// cannot stack without bound.
//
// The score is derived. It lives in the memory_capture table, which is rebuilt
// from memory_entries (themselves derived from the markdown) on every memory
// write, sync and reindex. Undoing the event removes the boost; the markdown
// and each fact's imp= trailer are never rewritten.
//
// Everything here runs only when exp.On("capture") is set. With the flag off no
// table is created and no query changes.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/trust"
)

// captureFlag names the experiment that turns this behaviour on.
const captureFlag = "capture"

// Constants are chosen from reasoning about how long a correction's context
// lasts, not from the benchmark. The window is one week, the half-life two days,
// and the entity-only weight half of a shared session or note.
const (
	captureWindow    = 7 * 24 * time.Hour
	captureHalfLife  = 2 * 24 * time.Hour
	captureEntityW   = 0.5
	captureMaxTarget = 3 // targets rank at or below the neutral rating
	// wCapture is the ranking weight of a capture score. It is the same order
	// as wReuse, so a boost reorders close ranks and cannot dominate them.
	wCapture = 0.05
)

const captureTableSQL = "CREATE TABLE IF NOT EXISTS memory_capture(" +
	"note TEXT NOT NULL, id TEXT NOT NULL, score REAL NOT NULL," +
	"reason TEXT NOT NULL, source TEXT NOT NULL, PRIMARY KEY(note,id))"

// captureRow is one derived boost: its score, the scope that earned it and the
// event it came from ("note#id").
type captureRow struct {
	score  float64
	reason string
	source string
}

func captureOn() bool { return exp.On(captureFlag) }

// captureUnlessBulk refreshes the boosts after a single-note write. A rebuild
// or sync holds bulk mode and refreshes once at its end instead.
func (ix *Index) captureUnlessBulk() error {
	if ix.bulk || !captureOn() {
		return nil
	}
	return ix.recomputeCapture()
}

// refreshCapture recomputes the boosts regardless of bulk mode. Reindex and
// sync call it once after their sweep.
func (ix *Index) refreshCapture() error {
	if !captureOn() {
		return nil
	}
	return ix.recomputeCapture()
}

// captureScores returns every fact with a recorded boost. A missing table reads
// as an empty map, which is the flag-off ranking.
func (ix *Index) captureScores() (map[entKey]captureRow, error) {
	if err := ix.DB.Exec(captureTableSQL); err != nil {
		return nil, err
	}
	rows, err := ix.DB.Query("SELECT note,id,score,reason,source FROM memory_capture WHERE score>0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[entKey]captureRow{}
	for rows.Next() {
		var note, id string
		var c captureRow
		if err := rows.Scan(&note, &id, &c.score, &c.reason, &c.source); err != nil {
			return nil, err
		}
		out[entKey{note, id}] = c
	}
	return out, rows.Err()
}

// CaptureOf reports the retroactive boost of one fact: its score, the scope
// that earned it and the event it came from. It is zero and empty when the
// flag is off or nothing promoted the fact.
func (ix *Index) CaptureOf(note, id string) (score float64, reason, source string, err error) {
	caps, err := ix.captureScores()
	if err != nil {
		return 0, "", "", err
	}
	c := caps[entKey{note, id}]
	return c.score, c.reason, c.source, nil
}

// captureRank adds the boost to each hit's score. Called only under the flag.
// A read error leaves the ranking as it was without the flag, so a broken
// capture table cannot take recall down with it.
func (ix *Index) captureRank(out []MemoryHit) {
	caps, err := ix.captureScores()
	if err != nil || len(caps) == 0 {
		return
	}
	for i := range out {
		if c, ok := caps[entKey{out[i].Note, out[i].ID}]; ok {
			out[i].Score += wCapture * c.score
		}
	}
}

// capturePruneClause excludes boosted facts from prune proposals. It is empty
// with the flag off, so the prune query is byte-identical then.
func capturePruneClause() (string, error) {
	if !captureOn() {
		return "", nil
	}
	return " AND NOT EXISTS (SELECT 1 FROM memory_capture c" +
		" WHERE c.note=memory_entries.note AND c.id=memory_entries.id AND c.score>0)", nil
}

// captureFact is the slice of one memory row the capture pass needs.
type captureFact struct {
	note, id, session string
	t                 time.Time
	stamp             string
	human, superseded bool
	hidden, trusted   bool
	low               bool // rank at or below the neutral rating
	isEvent           bool // a person's fact that replaced an earlier one
	ents              map[string]bool
}

// recomputeCapture rebuilds memory_capture from the memory rows. It runs under
// the caller's write lock.
func (ix *Index) recomputeCapture() error {
	if err := ix.DB.Exec(captureTableSQL); err != nil {
		return err
	}
	facts, err := ix.loadCaptureFacts()
	if err != nil {
		return err
	}
	best := captureBoosts(facts)

	if err := ix.DB.Exec("DELETE FROM memory_capture"); err != nil {
		return err
	}
	keys := make([]entKey, 0, len(best))
	for k := range best {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].note != keys[j].note {
			return keys[i].note < keys[j].note
		}
		return keys[i].id < keys[j].id
	})
	const chunk = 200
	for start := 0; start < len(keys); start += chunk {
		end := min(start+chunk, len(keys))
		var b strings.Builder
		b.WriteString("INSERT INTO memory_capture(note,id,score,reason,source) VALUES ")
		args := make([]any, 0, (end-start)*5)
		for i, k := range keys[start:end] {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("(?,?,?,?,?)")
			c := best[k]
			args = append(args, k.note, k.id, c.score, c.reason, c.source)
		}
		if err := ix.DB.Exec(b.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// loadCaptureFacts reads every memory row, its entities, and the set of fact ids
// some later fact superseded.
func (ix *Index) loadCaptureFacts() ([]captureFact, error) {
	ents := map[entKey]map[string]bool{}
	erows, err := ix.DB.Query("SELECT note,id,entity FROM memory_entities")
	if err != nil {
		return nil, err
	}
	for erows.Next() {
		var note, id, ent string
		if err := erows.Scan(&note, &id, &ent); err != nil {
			erows.Close()
			return nil, err
		}
		k := entKey{note, id}
		if ents[k] == nil {
			ents[k] = map[string]bool{}
		}
		ents[k][ent] = true
	}
	erows.Close()
	if err := erows.Err(); err != nil {
		return nil, err
	}

	rows, err := ix.DB.Query("SELECT note,id,session,stamp,origin,visibility,importance," +
		"human,hand,superseded_by FROM memory_entries")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type raw struct {
		f          captureFact
		origin     string
		visibility string
		imp        int
		human      bool
		supBy      string
	}
	var all []raw
	predecessors := map[string]bool{}
	for rows.Next() {
		var r raw
		var h, hand int
		if err := rows.Scan(&r.f.note, &r.f.id, &r.f.session, &r.f.stamp, &r.origin,
			&r.visibility, &r.imp, &h, &hand, &r.supBy); err != nil {
			return nil, err
		}
		r.human = h == 1 || hand == 1
		if r.supBy != "" && !strings.HasPrefix(r.supBy, "retracted:") {
			predecessors[r.supBy] = true
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]captureFact, 0, len(all))
	for _, r := range all {
		f := r.f
		t, ok := memory.StampTime(f.stamp)
		if !ok {
			continue
		}
		f.t = t
		f.human = r.human
		f.superseded = r.supBy != ""
		f.hidden = memory.Entry{Visibility: r.visibility}.Hidden()
		f.trusted = trust.FromOrigin(r.origin) == trust.Trusted
		eff := r.imp
		if eff < memory.MinImportance || eff > memory.MaxImportance {
			eff = 3
			if r.human {
				eff = 4
			}
		}
		f.low = eff <= captureMaxTarget
		f.ents = ents[entKey{f.note, f.id}]
		// A person's correction is an event only when it actually replaced
		// something; a person's new fact with no predecessor is not one.
		f.isEvent = r.human && predecessors[f.id]
		out = append(out, f)
	}
	return out, nil
}

// captureBoosts computes the best boost of every target from every event.
func captureBoosts(facts []captureFact) map[entKey]captureRow {
	var events, targets []captureFact
	for _, f := range facts {
		if f.hidden || !f.trusted {
			continue // a hidden or pulled fact is never an event and never a target
		}
		if f.isEvent {
			events = append(events, f)
		}
		if !f.human && !f.superseded && f.low {
			targets = append(targets, f)
		}
	}
	if len(events) == 0 || len(targets) == 0 {
		return nil
	}
	sort.Slice(targets, func(i, j int) bool {
		if !targets[i].t.Equal(targets[j].t) {
			return targets[i].t.Before(targets[j].t)
		}
		if targets[i].note != targets[j].note {
			return targets[i].note < targets[j].note
		}
		return targets[i].id < targets[j].id
	})
	sort.Slice(events, func(i, j int) bool {
		if !events[i].t.Equal(events[j].t) {
			return events[i].t.Before(events[j].t)
		}
		if events[i].note != events[j].note {
			return events[i].note < events[j].note
		}
		return events[i].id < events[j].id
	})

	best := map[entKey]captureRow{}
	for _, e := range events {
		lo := sort.Search(len(targets), func(i int) bool {
			return targets[i].t.After(e.t.Add(-captureWindow))
		})
		// Strictly inside the window, as the survey rule |t1 - t0| < tau states it.
		for i := lo; i < len(targets) && targets[i].t.Before(e.t.Add(captureWindow)); i++ {
			g := targets[i]
			if g.note == e.note && g.id == e.id {
				continue
			}
			w, reason := captureScope(e, g)
			if w == 0 {
				continue
			}
			dt := math.Abs(g.t.Sub(e.t).Hours())
			score := w * math.Pow(0.5, dt/captureHalfLife.Hours())
			k := entKey{g.note, g.id}
			if cur, ok := best[k]; !ok || score > cur.score {
				best[k] = captureRow{score: score, reason: reason,
					source: fmt.Sprintf("%s#%s", e.note, e.id)}
			}
		}
	}
	return best
}

// captureScope is the strongest shared scope between an event and a target, and
// its weight. Zero means they share nothing.
func captureScope(e, g captureFact) (float64, string) {
	if e.session != "" && e.session == g.session {
		return 1, "session"
	}
	if e.note == g.note {
		return 1, "note"
	}
	small, big := e.ents, g.ents
	if len(big) < len(small) {
		small, big = big, small
	}
	for ent := range small {
		if big[ent] {
			return captureEntityW, "entity"
		}
	}
	return 0, ""
}
