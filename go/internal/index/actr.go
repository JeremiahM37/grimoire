package index

import (
	"math"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
)

// ACT-R base-level activation as the frequency-recency term (lab flag "actr").
//
// A fact's base-level activation is B = ln(sum_j (t - t_j)^-d) over the times
// t_j at which it was learned or used. Each past event contributes a power-law
// term, so a fact used often recently scores high, a fact used once long ago
// scores low, and the power law (rather than the exponential of the fixed
// 90-day half-life) keeps old-but-repeated facts competitive.
//
// The creation stamp is always the first event: a fact that has never been
// used is still ranked by its age, so the flag changes nothing about facts
// with no use history except the shape of the decay.
//
// Events are the use trace only (RecordUse, i.e. a helpful vote). Recalls
// are NOT events: recall counts are written on every query, so counting them
// would feed the retriever's own choices back into the score.
//
// Storage is bounded: the last actrKeep event times per fact are kept
// exactly, and older ones are folded into a summary (count, first, last)
// that is treated as uniformly spread over its span (the optimised-learning
// idea of Petrov 2006, here as an explicit integral).

// ActrFlag is the experiment name for this file.
const ActrFlag = "actr"

const (
	// actrDecay is d in B = ln sum (t - t_j)^-d. 0.5 is the standard value
	// (Anderson and Lebiere 1998); it is not fitted here.
	actrDecay = 0.5
	// actrKeep is how many event times are stored exactly per fact.
	actrKeep = 32
	// actrMinAge floors an event's age so a use "now" cannot produce an
	// infinite term. One hour, in days.
	actrMinAge = 1.0 / 24
)

// actrTrace is one fact's event history as ranking sees it.
type actrTrace struct {
	// Creation is the write stamp: the first event, never folded.
	Creation time.Time
	// Events are stored use times, sorted ascending.
	Events []time.Time
	// Folded counts older events that were summarised away, spread over
	// [FoldFirst, FoldLast]. Zero means nothing was folded.
	Folded    int
	FoldFirst time.Time
	FoldLast  time.Time
}

// powTerm is (age in days)^-d with the age floored and future events skipped
// by the caller.
func powTerm(ageDays float64) float64 {
	if ageDays < actrMinAge {
		ageDays = actrMinAge
	}
	return math.Pow(ageDays, -actrDecay)
}

// actrBase returns B for a trace at time now, and false when nothing is
// usable (no creation stamp and no events at or before now).
func actrBase(tr actrTrace, now time.Time) (float64, bool) {
	sum := 0.0
	add := func(t time.Time) {
		age := now.Sub(t).Hours() / 24
		if age < 0 {
			return // a future event (clock skew, a typo) is ignored, not clamped
		}
		sum += powTerm(age)
	}
	if !tr.Creation.IsZero() {
		add(tr.Creation)
	}
	for _, t := range tr.Events {
		add(t)
	}
	if tr.Folded > 0 {
		hi := now.Sub(tr.FoldFirst).Hours() / 24 // oldest folded event: largest age
		lo := now.Sub(tr.FoldLast).Hours() / 24  // newest folded event: smallest age
		if lo >= 0 {
			if lo < actrMinAge {
				lo = actrMinAge
			}
			if hi < lo {
				hi = lo
			}
			span := hi - lo
			if span < actrMinAge {
				sum += float64(tr.Folded) * powTerm((hi+lo)/2)
			} else {
				// integral of x^-d over [lo, hi], times the density of events
				// over the span, is the expected contribution of the folded set.
				integral := (math.Pow(hi, 1-actrDecay) - math.Pow(lo, 1-actrDecay)) / (1 - actrDecay)
				sum += float64(tr.Folded) / span * integral
			}
		}
	}
	if sum <= 0 {
		return 0, false
	}
	return math.Log(sum), true
}

// actrRecency maps B to (0,1) for the ranking term. The logistic keeps the
// same scale as the fixed recency term it replaces, so the existing weight
// (wRecency) still means what it did. B = 0 maps to 0.5.
func actrRecency(b float64) float64 {
	return 1 / (1 + math.Exp(-b))
}

// parseStampAt reads a memory stamp the way recencyScoreWith does.
func parseStampAt(stamp string, loc *time.Location) (time.Time, bool) {
	if stamp == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04", stamp, loc)
	if err != nil {
		if t, err = time.ParseInLocation("2006-01-02", stamp, loc); err != nil {
			return time.Time{}, false
		}
	}
	return t, true
}

// actrRecord writes one use event for a fact and folds the oldest stored
// events into the summary when the per-fact log is full. It does nothing
// unless the flag is on, so the flag-off database is never touched.
func (ix *Index) actrRecord(rel, id string, at time.Time) error {
	if !exp.On(ActrFlag) {
		return nil
	}
	if err := ix.actrSchema(); err != nil {
		return err
	}
	unix := float64(at.UnixNano()) / 1e9
	if err := ix.DB.Exec(
		"INSERT OR IGNORE INTO actr_events(note,id,at) VALUES(?,?,?)", rel, id, unix); err != nil {
		return err
	}
	rows, err := ix.DB.Query(
		"SELECT at FROM actr_events WHERE note=? AND id=? ORDER BY at ASC", rel, id)
	if err != nil {
		return err
	}
	var times []float64
	for rows.Next() {
		var t float64
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return err
		}
		times = append(times, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	excess := len(times) - actrKeep
	if excess <= 0 {
		return nil
	}
	old := times[:excess]
	if err := ix.DB.Exec(`INSERT INTO actr_folded(note,id,n,first,last) VALUES(?,?,?,?,?)
		ON CONFLICT(note,id) DO UPDATE SET n=n+excluded.n,
		first=MIN(first,excluded.first), last=MAX(last,excluded.last)`,
		rel, id, len(old), old[0], old[len(old)-1]); err != nil {
		return err
	}
	return ix.DB.Exec(
		"DELETE FROM actr_events WHERE note=? AND id=? AND at <= ?", rel, id, old[len(old)-1])
}

func (ix *Index) actrSchema() error {
	if err := ix.DB.Exec(`CREATE TABLE IF NOT EXISTS actr_events(
		note TEXT NOT NULL, id TEXT NOT NULL, at REAL NOT NULL,
		PRIMARY KEY(note,id,at))`); err != nil {
		return err
	}
	return ix.DB.Exec(`CREATE TABLE IF NOT EXISTS actr_folded(
		note TEXT NOT NULL, id TEXT NOT NULL, n INTEGER NOT NULL,
		first REAL NOT NULL, last REAL NOT NULL, PRIMARY KEY(note,id))`)
}

// actrTraces loads the event history of every candidate. It returns nil when
// the flag is off, which is the signal rankMemory uses to keep the old term.
func (ix *Index) actrTraces(cands []memoryRow) map[entKey]actrTrace {
	if !exp.On(ActrFlag) {
		return nil
	}
	out := map[entKey]actrTrace{}
	if err := ix.actrSchema(); err != nil {
		return out
	}
	toTime := func(f float64) time.Time {
		return time.Unix(0, int64(f*1e9))
	}
	rows, err := ix.DB.Query("SELECT note,id,at FROM actr_events ORDER BY at ASC")
	if err == nil {
		for rows.Next() {
			var k entKey
			var at float64
			if rows.Scan(&k.note, &k.id, &at) != nil {
				break
			}
			tr := out[k]
			tr.Events = append(tr.Events, toTime(at))
			out[k] = tr
		}
		rows.Close()
	}
	rows, err = ix.DB.Query("SELECT note,id,n,first,last FROM actr_folded")
	if err == nil {
		for rows.Next() {
			var k entKey
			var n int
			var first, last float64
			if rows.Scan(&k.note, &k.id, &n, &first, &last) != nil {
				break
			}
			tr := out[k]
			tr.Folded, tr.FoldFirst, tr.FoldLast = n, toTime(first), toTime(last)
			out[k] = tr
		}
		rows.Close()
	}
	for _, c := range cands {
		k := entKey{c.hit.Note, c.hit.ID}
		tr := out[k]
		if t, ok := parseStampAt(c.hit.Stamp, time.Local); ok {
			tr.Creation = t
		}
		out[k] = tr
	}
	return out
}

// actrRecencyFor is the replacement recency term for one candidate. A fact
// with no usable creation stamp keeps the fixed term, as it does today.
func actrRecencyFor(tr actrTrace, now time.Time, fallback float64) float64 {
	b, ok := actrBase(tr, now)
	if !ok {
		return fallback
	}
	return actrRecency(b)
}
