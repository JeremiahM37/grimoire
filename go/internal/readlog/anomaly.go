package readlog

import (
	"sort"
	"strings"
	"time"
)

// Reading the audit trail back.
//
// v2.4.5 added a record of who opened which restricted document. Nothing has
// ever read it. That is the ordinary fate of an audit log: it is written for
// the incident, and by the time there is an incident nobody remembers it
// exists, so it gets its first query six weeks late and answers a question
// that has already been decided.
//
// The signal worth watching is BREADTH, not depth. A person doing their job
// opens a handful of documents they were looking for. The tells of a
// compromised agent, a departing employee, or a connector mirroring the wrong
// channel all look the same and look nothing like that: many distinct
// documents, quickly, or a run of attempts at documents the caller cannot
// open. Neither needs new instrumentation — both are already in the table.
//
// This is deliberately NOT an alerting system. It computes a signal on demand
// and reports it; there is no daemon, no threshold state, no notification
// channel. A self-hosted product that starts emailing people has acquired an
// SMTP configuration, a retry policy and a way to leak the very document names
// this table exists to protect. Surfacing it where an operator already looks
// is the whole intervention.

// Thresholds. Deliberately blunt: a tuned detector nobody understands produces
// alerts nobody trusts, and these numbers are meant to be argued with.
const (
	// DefaultWindow is how far back a burst is measured over. Five minutes is
	// short enough that ordinary work rarely fills it and long enough to catch
	// a script that is pacing itself a little.
	DefaultWindow = 5 * time.Minute
	// DefaultBreadth is how many DISTINCT restricted documents one caller may
	// open in a window before it is worth a look. Restricted documents only —
	// on most instances a person could work all day and log nothing.
	DefaultBreadth = 15
	// DefaultDenials is how many refusals in a window count as somebody
	// walking paths they cannot open. Lower, because the innocent explanation
	// (a stale link) does not repeat.
	DefaultDenials = 5
)

// Anomaly is one caller's burst.
type Anomaly struct {
	Kind string `json:"kind"` // "breadth" or "denials"
	User string `json:"user"`
	Name string `json:"name"`
	Addr string `json:"addr,omitempty"`

	// Count is distinct documents for a breadth burst, attempts for a denial
	// run — the number the threshold was compared against.
	Count int `json:"count"`
	// Documents are up to a handful of the paths involved, so the operator can
	// tell "swept the whole vault" from "read one folder". Capped: an operator
	// deciding whether to care does not need four hundred paths, and a
	// response that quoted every restricted document a caller touched would
	// be its own disclosure.
	Documents []string `json:"documents"`
	First     string   `json:"first"`
	Last      string   `json:"last"`
}

const maxSampleDocs = 8

// Options tune a scan. Zero values mean the defaults.
type Options struct {
	Window   time.Duration
	Breadth  int
	Denials  int
	Since    time.Time // ignore events before this; zero means the window
	MaxScan  int       // rows to consider; 0 means a sensible cap
	Now      time.Time // injectable clock, for tests
	OnlyUser string
}

// WithDefaultsPublic exposes the resolved options, so a response can echo the
// thresholds it was computed with instead of a UI keeping its own copy of the
// defaults and drifting from them.
func (o Options) WithDefaultsPublic() Options { return o.withDefaults() }

func (o Options) withDefaults() Options {
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	if o.Breadth <= 0 {
		o.Breadth = DefaultBreadth
	}
	if o.Denials <= 0 {
		o.Denials = DefaultDenials
	}
	if o.MaxScan <= 0 {
		o.MaxScan = 5000
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	return o
}

// Anomalies scans recent events for bursts.
//
// A sliding window per caller rather than fixed buckets: with hourly buckets a
// sweep that straddles the boundary is two half-sweeps and neither trips the
// threshold, which is the failure mode of most naive rate detection — and it is
// trivially exploitable by anyone who has noticed the boundary.
func (l *Log) Anomalies(opt Options) ([]Anomaly, error) {
	if l == nil || l.db == nil {
		return nil, nil
	}
	opt = opt.withDefaults()

	// Read the whole recent tail once and window it in memory. The alternative
	// — a SQL query per caller per window — is many queries on the single
	// connection this package already goes out of its way not to monopolize.
	rows, err := l.Recent(Query{Limit: opt.MaxScan, User: opt.OnlyUser})
	if err != nil {
		return nil, err
	}
	// Recent returns newest first; a sliding window wants chronological order.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	cutoff := opt.Since
	if cutoff.IsZero() {
		// A day, not a window: the caller asked "has anything happened", and a
		// five-minute horizon answers "not in the last five minutes", which is
		// not the question anybody has.
		cutoff = opt.Now.Add(-24 * time.Hour)
	}

	byCaller := map[string][]Row{}
	for _, r := range rows {
		at, err := time.Parse(time.RFC3339, r.At)
		if err != nil || at.Before(cutoff) {
			continue
		}
		byCaller[callerKey(r)] = append(byCaller[callerKey(r)], r)
	}

	var out []Anomaly
	for _, evs := range byCaller {
		if a, ok := breadthBurst(evs, opt); ok {
			out = append(out, a)
		}
		if a, ok := denialBurst(evs, opt); ok {
			out = append(out, a)
		}
	}
	// Worst first, then stable by time, so an operator reads the top and stops.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Last > out[j].Last
	})
	return out, nil
}

// callerKey identifies who is reading. The account when there is one, the
// address otherwise — an unauthenticated caller has no account, and grouping
// every one of them together would hide exactly the scan worth seeing.
func callerKey(r Row) string {
	if strings.TrimSpace(r.User) != "" {
		return "u:" + r.User
	}
	return "a:" + r.Addr
}

// breadthBurst finds the widest sliding window of DISTINCT allowed documents.
func breadthBurst(evs []Row, opt Options) (Anomaly, bool) {
	best := Anomaly{Kind: "breadth"}
	found := false
	for i := range evs {
		start, ok := parseAt(evs[i].At)
		if !ok {
			continue
		}
		seen := map[string]bool{}
		var docs []string
		last := evs[i].At
		for j := i; j < len(evs); j++ {
			at, ok := parseAt(evs[j].At)
			if !ok || at.Sub(start) > opt.Window {
				break
			}
			if !evs[j].Allowed {
				// Denials are the other detector's business. Counting them
				// here would let a caller trip the breadth threshold with
				// documents it never actually received.
				continue
			}
			if !seen[evs[j].Path] {
				seen[evs[j].Path] = true
				if len(docs) < maxSampleDocs {
					docs = append(docs, evs[j].Path)
				}
			}
			last = evs[j].At
		}
		if len(seen) >= opt.Breadth && len(seen) > best.Count {
			best = Anomaly{Kind: "breadth", User: evs[i].User, Name: evs[i].Name,
				Addr: evs[i].Addr, Count: len(seen), Documents: docs,
				First: evs[i].At, Last: last}
			found = true
		}
	}
	return best, found
}

// denialBurst finds a run of refusals — somebody walking paths they cannot
// open, which looks like nothing at all in a model that only logs successes.
func denialBurst(evs []Row, opt Options) (Anomaly, bool) {
	best := Anomaly{Kind: "denials"}
	found := false
	for i := range evs {
		start, ok := parseAt(evs[i].At)
		if !ok || evs[i].Allowed {
			continue
		}
		n := 0
		var docs []string
		last := evs[i].At
		for j := i; j < len(evs); j++ {
			at, ok := parseAt(evs[j].At)
			if !ok || at.Sub(start) > opt.Window {
				break
			}
			if evs[j].Allowed {
				continue
			}
			n++
			if len(docs) < maxSampleDocs {
				docs = append(docs, evs[j].Path)
			}
			last = evs[j].At
		}
		if n >= opt.Denials && n > best.Count {
			best = Anomaly{Kind: "denials", User: evs[i].User, Name: evs[i].Name,
				Addr: evs[i].Addr, Count: n, Documents: docs,
				First: evs[i].At, Last: last}
			found = true
		}
	}
	return best, found
}

func parseAt(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}
