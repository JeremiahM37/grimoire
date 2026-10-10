package adherence

import (
	"database/sql"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/utilization"
)

// SQL for an action's outcome. "Bad" is any sign the action went wrong; an
// action is "known" when at least one outcome signal exists for it (a hook that
// could not see a tool response reports none, and such an action says nothing).
const (
	badSQL   = `(a.failed=1 OR a.tests_fail>0 OR a.reedit=1 OR a.revert=1 OR a.thrash>=3 OR a.denied=1 OR a.correction=1)`
	knownSQL = `(a.failed>=0 OR a.tests_fail>=0 OR ` + badSQL + `)`
)

// Interval is a proportion with its 95% Wilson interval.
type Interval struct {
	K    int     `json:"k"`
	N    int     `json:"n"`
	Rate float64 `json:"rate"`
	Lo   float64 `json:"lo"`
	Hi   float64 `json:"hi"`
}

// Wilson returns k/n with a 95% Wilson score interval. n==0 gives zeros.
func Wilson(k, n int) Interval {
	iv := Interval{K: k, N: n}
	if n <= 0 {
		return iv
	}
	const z = 1.96
	p := float64(k) / float64(n)
	d := 1 + z*z/float64(n)
	c := p + z*z/(2*float64(n))
	h := z * math.Sqrt(p*(1-p)/float64(n)+z*z/(4*float64(n)*float64(n)))
	iv.Rate, iv.Lo, iv.Hi = p, math.Max(0, (c-h)/d), math.Min(1, (c+h)/d)
	return iv
}

// Benefit compares the bad-outcome rate of actions a memory influenced with
// actions it did not, matched on tool and session stage. It is OBSERVATIONAL:
// the agent may be more likely to pick up a memory on easy work, so a
// difference is "associated with", never "caused by" (the holdout is that).
type Benefit struct {
	Label        string   `json:"label"` // always "associated"
	Influenced   int      `json:"influenced_actions"`
	Matched      int      `json:"matched_influenced_actions"` // influenced actions in strata that also have comparison actions
	Comparison   int      `json:"comparison_actions"`
	Strata       int      `json:"strata"`
	BadInfluence Interval `json:"bad_rate_influenced"`
	BadMatched   float64  `json:"bad_rate_comparison_matched"`
	Diff         float64  `json:"diff"` // influenced minus matched comparison; negative is better
	Lo           float64  `json:"lo"`
	Hi           float64  `json:"hi"`
	Note         string   `json:"note,omitempty"`
}

type stratum struct{ n, bad int }

type strataKey struct{ tool, stage string }

func readStrata(rs *sql.Rows, err error) (map[strataKey]stratum, error) {
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[strataKey]stratum{}
	for rs.Next() {
		var k strataKey
		var s stratum
		if err := rs.Scan(&k.tool, &k.stage, &s.n, &s.bad); err != nil {
			return nil, err
		}
		out[k] = s
	}
	return out, rs.Err()
}

// MatchedDiff is the stratified difference in bad-outcome rate. Each stratum
// is weighted by its influenced actions; the variance adds the two Wald terms
// with an add-one-half correction so a stratum of all-clean actions still
// carries uncertainty.
func MatchedDiff(inf, ctl map[strataKey]stratum) Benefit {
	b := Benefit{Label: "associated"}
	var w, wi, wc, vr float64
	k, n := 0, 0
	for key, i := range inf {
		b.Influenced += i.n
		k += i.bad
		n += i.n
		c, ok := ctl[key]
		if !ok || c.n == 0 || i.n == 0 {
			continue
		}
		b.Strata++
		b.Matched += i.n
		b.Comparison += c.n
		pi, pc := float64(i.bad)/float64(i.n), float64(c.bad)/float64(c.n)
		qi := (float64(i.bad) + 0.5) / (float64(i.n) + 1)
		qc := (float64(c.bad) + 0.5) / (float64(c.n) + 1)
		weight := float64(i.n)
		w += weight
		wi += weight * pi
		wc += weight * pc
		vr += weight * weight * (qi*(1-qi)/float64(i.n) + qc*(1-qc)/float64(c.n))
	}
	b.BadInfluence = Wilson(k, n)
	if w == 0 {
		b.Note = "no comparison actions in the same tool and stage"
		return b
	}
	b.BadMatched = wc / w
	b.Diff = (wi - wc) / w
	se := math.Sqrt(vr) / w
	b.Lo, b.Hi = b.Diff-1.96*se, b.Diff+1.96*se
	return b
}

// scopeSQL says which injections define "influenced by": one memory, one kind,
// or any.
type scopeSQL struct {
	where string
	args  []any
}

// ScopeTarget, ScopeKind and ScopeAll build a benefit scope.
func ScopeTarget(t string) scopeSQL { return scopeSQL{"i.target=?", []any{t}} }
func ScopeKind(k string) scopeSQL   { return scopeSQL{"i.kind=?", []any{k}} }
func ScopeAll() scopeSQL            { return scopeSQL{"1=1", nil} }

// Benefit computes the observational benefit of a scope since the cutoff.
func (s *Store) Benefit(sc scopeSQL, since time.Time) (Benefit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.benefitLocked(sc, since)
}

func (s *Store) benefitLocked(sc scopeSQL, since time.Time) (Benefit, error) {
	linked := `a.id IN (SELECT l.action_id FROM trace_links l JOIN injections i ON i.id=l.injection_id WHERE ` + sc.where + `)`
	args := append([]any{since.Unix()}, sc.args...)
	inf, err := readStrata(s.db.Query(`SELECT a.tool, a.stage, COUNT(*), SUM(`+badSQL+`) FROM trace_actions a
		WHERE a.ts>=? AND `+knownSQL+` AND `+linked+` GROUP BY a.tool, a.stage`, args...))
	if err != nil {
		return Benefit{}, err
	}
	ctl, err := readStrata(s.db.Query(`SELECT a.tool, a.stage, COUNT(*), SUM(`+badSQL+`) FROM trace_actions a
		WHERE a.ts>=? AND `+knownSQL+` AND NOT `+linked+`
		AND EXISTS (SELECT 1 FROM injections j WHERE j.session=a.session AND j.ts<=a.ts)
		GROUP BY a.tool, a.stage`, args...))
	if err != nil {
		return Benefit{}, err
	}
	return MatchedDiff(inf, ctl), nil
}

// ActionOutcomes summarises what happened to a set of actions.
type ActionOutcomes struct {
	Actions    int `json:"actions"`
	Observed   int `json:"observed"` // actions with an outcome signal
	Failed     int `json:"failed"`
	TestsFail  int `json:"tests_failed"`
	TestsPass  int `json:"tests_passed"`
	Reedited   int `json:"re_edited"`
	Reverted   int `json:"reverted"`
	Thrash     int `json:"thrash"`
	Denied     int `json:"denied"`
	Corrected  int `json:"corrected"`
	BadActions int `json:"bad"`
}

// ReminderStats is the action-stage natural experiment: a memory was shown
// for a pending action, and the next executed action was or was not that one.
type ReminderStats struct {
	Reminders int      `json:"reminders"`
	Unchanged int      `json:"unchanged"`
	Changed   int      `json:"changed"`
	Abandoned int      `json:"abandoned"`
	Open      int      `json:"open"`
	Ask       int      `json:"ask"`         // reminders that came with an enforce: ask decision
	AskRan    int      `json:"ask_ran"`     // ... and the action then ran as asked
	AskNot    int      `json:"ask_not_run"` // ... and it never ran (declined or dropped)
	Change    Interval `json:"changed_rate"`
}

// Card is the trace of one memory.
type Card struct {
	Target       string         `json:"target"`
	Kind         string         `json:"kind,omitempty"`
	Path         string         `json:"-"`
	FactID       string         `json:"-"`
	Exposures    int            `json:"exposures"`
	Withheld     int            `json:"withheld"`
	Uptake       Interval       `json:"uptake"`
	Influence    Interval       `json:"influence"`
	Evidence     map[string]int `json:"link_evidence"`
	Outcomes     map[string]int `json:"adherence_outcomes"`
	Linked       ActionOutcomes `json:"linked_actions"`
	Reminders    ReminderStats  `json:"reminders"`
	Benefit      Benefit        `json:"benefit"`
	MeaningMatch Interval       `json:"meaning_match,omitempty"`
	// Recent lists the latest actions this memory was linked to: tool, stage,
	// time and outcome codes, never a command, path or text.
	Recent []ActionRow `json:"recent_actions"`
}

// RecentActions is how many linked actions a card lists.
const RecentActions = 10

// Card builds the per-memory card.
func (s *Store) Card(target string, since time.Time) (Card, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := Card{Target: target, Evidence: map[string]int{}, Outcomes: map[string]int{}}
	rs, err := s.db.Query(`SELECT `+rowCols+`, kind, rem, ask, pend, meaning FROM injections WHERE target=? AND ts>=? ORDER BY id`, target, since.Unix())
	if err != nil {
		return c, err
	}
	type trow struct {
		Row
		kind, rem, pend string
		ask             int
		meaning         float64
	}
	var rows []trow
	for rs.Next() {
		var r trow
		var ci, ct, f, n int
		if err := rs.Scan(&r.ID, &r.Session, &r.Tag, &r.Key, &r.Target, &r.Path, &r.FactID, &r.Stage,
			&r.Relevance, &r.TS, &r.Tools, &ci, &r.CheckResult, &ct, &f, &n, &r.FPN, &r.FPHit,
			&r.kind, &r.rem, &r.ask, &r.pend, &r.meaning); err != nil {
			rs.Close()
			return c, err
		}
		r.Cited, r.Contradicted, r.Finalized, r.Counted = ci != 0, ct != 0, f != 0, n != 0
		rows = append(rows, r)
	}
	rs.Close()
	linked := map[int64]string{} // injection -> strongest evidence seen
	lr, err := s.db.Query(`SELECT l.injection_id, l.evidence FROM trace_links l JOIN injections i ON i.id=l.injection_id WHERE i.target=? AND i.ts>=?`, target, since.Unix())
	if err != nil {
		return c, err
	}
	for lr.Next() {
		var id int64
		var ev string
		if err := lr.Scan(&id, &ev); err != nil {
			lr.Close()
			return c, err
		}
		linked[id] = ev
		c.Evidence[ev]++
	}
	lr.Close()

	var up, upN, inf, infN, mm, mmN int
	for _, r := range rows {
		c.Exposures++
		if r.kind != "" {
			c.Kind = r.kind
		}
		if r.Path != "" {
			c.Path, c.FactID = r.Path, r.FactID
		}
		c.Outcomes[r.Outcome()]++
		// Uptake can only be seen for a memory that was fingerprintable (or
		// cited): the rest are not in the denominator.
		if r.FPN > 0 || r.Cited {
			upN++
			if r.Cited || r.FPHit >= UsedMin {
				up++
			}
		}
		if r.meaning >= 0 {
			mmN++
			if r.meaning >= 0.5 {
				mm++
			}
		}
		if r.Tools > 0 || r.rem != "" {
			infN++
			if _, ok := linked[r.ID]; ok {
				inf++
			}
		}
		if r.pend != "" {
			c.Reminders.Reminders++
			switch r.rem {
			case RemUnchanged:
				c.Reminders.Unchanged++
			case RemChanged:
				c.Reminders.Changed++
			case RemAbandoned:
				c.Reminders.Abandoned++
			default:
				c.Reminders.Open++
			}
			if r.ask == 1 {
				c.Reminders.Ask++
				switch r.rem {
				case RemUnchanged:
					c.Reminders.AskRan++
				case RemAbandoned, RemChanged:
					c.Reminders.AskNot++
				}
			}
		}
	}
	c.Uptake, c.Influence = Wilson(up, upN), Wilson(inf, infN)
	if mmN > 0 {
		c.MeaningMatch = Wilson(mm, mmN)
	}
	c.Reminders.Change = Wilson(c.Reminders.Changed, c.Reminders.Changed+c.Reminders.Unchanged)
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM trace_withheld WHERE target=? AND ts>=?`, target, since.Unix()).Scan(&c.Withheld); err != nil {
		return c, err
	}

	ar, err := s.db.Query(`SELECT a.failed, a.tests_pass, a.tests_fail, a.reedit, a.revert, a.thrash, a.denied, a.correction
		FROM trace_actions a WHERE a.ts>=? AND a.id IN
		(SELECT l.action_id FROM trace_links l JOIN injections i ON i.id=l.injection_id WHERE i.target=?)`, since.Unix(), target)
	if err != nil {
		return c, err
	}
	for ar.Next() {
		var failed, tp, tf, re, rv, th, dn, co int
		if err := ar.Scan(&failed, &tp, &tf, &re, &rv, &th, &dn, &co); err != nil {
			ar.Close()
			return c, err
		}
		o := &c.Linked
		o.Actions++
		bad := failed == 1 || tf > 0 || re == 1 || rv == 1 || th >= 3 || dn == 1 || co == 1
		if failed >= 0 || tf >= 0 || bad {
			o.Observed++
		}
		o.Failed += b2i(failed == 1)
		if tf > 0 {
			o.TestsFail++
		}
		if tp > 0 {
			o.TestsPass++
		}
		o.Reedited += re
		o.Reverted += rv
		o.Thrash += b2i(th >= 3)
		o.Denied += dn
		o.Corrected += co
		o.BadActions += b2i(bad)
	}
	ar.Close()
	c.Recent = []ActionRow{}
	rr, err := s.db.Query(`SELECT a.id, a.seq, a.tool, a.stage, a.ts, a.failed, a.tests_pass, a.tests_fail, a.reedit, a.revert, a.thrash, a.denied, a.correction
		FROM trace_actions a WHERE a.ts>=? AND a.id IN
		(SELECT l.action_id FROM trace_links l JOIN injections i ON i.id=l.injection_id WHERE i.target=?)
		ORDER BY a.ts DESC, a.id DESC LIMIT ?`, since.Unix(), target, RecentActions)
	if err != nil {
		return c, err
	}
	for rr.Next() {
		var a ActionRow
		var re, rv, dn, co int
		if err := rr.Scan(&a.ID, &a.Seq, &a.Tool, &a.Stage, &a.TS, &a.Failed, &a.TestsPass, &a.TestsFail, &re, &rv, &a.Thrash, &dn, &co); err != nil {
			rr.Close()
			return c, err
		}
		a.Reedit, a.Revert, a.Denied, a.Correction = re == 1, rv == 1, dn == 1, co == 1
		c.Recent = append(c.Recent, a)
	}
	rr.Close()
	c.Benefit, err = s.benefitLocked(ScopeTarget(target), since)
	return c, err
}

// KindSummary is the roll-up of one memory kind (or all).
type KindSummary struct {
	Kind      string        `json:"kind"`
	Memories  int           `json:"memories"`
	Exposures int           `json:"exposures"`
	Withheld  int           `json:"withheld"`
	Uptake    Interval      `json:"uptake"`
	Influence Interval      `json:"influence"`
	Reminders ReminderStats `json:"reminders"`
	Benefit   Benefit       `json:"benefit"`
}

// Summary rolls the trace up by kind, and overall (Kind "").
func (s *Store) Summary(since time.Time) ([]KindSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.db.Query(`SELECT i.kind, COUNT(*), COUNT(DISTINCT i.target),
		SUM(CASE WHEN i.fp_n>0 OR i.cited=1 THEN 1 ELSE 0 END),
		SUM(CASE WHEN (i.fp_n>0 OR i.cited=1) AND (i.cited=1 OR i.fp_hit>=?) THEN 1 ELSE 0 END),
		SUM(CASE WHEN i.tools>0 OR i.rem<>'' THEN 1 ELSE 0 END),
		SUM(CASE WHEN (i.tools>0 OR i.rem<>'') AND EXISTS (SELECT 1 FROM trace_links l WHERE l.injection_id=i.id) THEN 1 ELSE 0 END),
		SUM(CASE WHEN i.pend<>'' THEN 1 ELSE 0 END),
		SUM(CASE WHEN i.rem='unchanged' THEN 1 ELSE 0 END), SUM(CASE WHEN i.rem='changed' THEN 1 ELSE 0 END),
		SUM(CASE WHEN i.rem='abandoned' THEN 1 ELSE 0 END),
		SUM(CASE WHEN i.ask=1 THEN 1 ELSE 0 END)
		FROM injections i WHERE i.ts>=? GROUP BY i.kind`, UsedMin, since.Unix())
	if err != nil {
		return nil, err
	}
	type acc struct {
		KindSummary
		up, upN, inf, infN int
	}
	by := map[string]*acc{}
	var all acc
	for rs.Next() {
		var kind string
		var n, mem, upN, up, infN, inf, rem, un, ch, ab, ask int
		if err := rs.Scan(&kind, &n, &mem, &upN, &up, &infN, &inf, &rem, &un, &ch, &ab, &ask); err != nil {
			rs.Close()
			return nil, err
		}
		a := &acc{KindSummary: KindSummary{Kind: kind, Memories: mem, Exposures: n}, up: up, upN: upN, inf: inf, infN: infN}
		a.Reminders = ReminderStats{Reminders: rem, Unchanged: un, Changed: ch, Abandoned: ab, Open: rem - un - ch - ab, Ask: ask}
		by[kind] = a
		all.Memories += mem
		all.Exposures += n
		all.up += up
		all.upN += upN
		all.inf += inf
		all.infN += infN
		all.Reminders.Reminders += rem
		all.Reminders.Unchanged += un
		all.Reminders.Changed += ch
		all.Reminders.Abandoned += ab
		all.Reminders.Open += rem - un - ch - ab
		all.Reminders.Ask += ask
	}
	rs.Close()
	wr, err := s.db.Query(`SELECT kind, COUNT(*) FROM trace_withheld WHERE ts>=? GROUP BY kind`, since.Unix())
	if err != nil {
		return nil, err
	}
	for wr.Next() {
		var kind string
		var n int
		if err := wr.Scan(&kind, &n); err != nil {
			wr.Close()
			return nil, err
		}
		if a := by[kind]; a != nil {
			a.Withheld = n
		} else {
			by[kind] = &acc{KindSummary: KindSummary{Kind: kind, Withheld: n}}
		}
		all.Withheld += n
	}
	wr.Close()
	var out []KindSummary
	finish := func(a *acc, sc scopeSQL) error {
		a.Uptake, a.Influence = Wilson(a.up, a.upN), Wilson(a.inf, a.infN)
		a.Reminders.Change = Wilson(a.Reminders.Changed, a.Reminders.Changed+a.Reminders.Unchanged)
		b, err := s.benefitLocked(sc, since)
		if err != nil {
			return err
		}
		a.Benefit = b
		out = append(out, a.KindSummary)
		return nil
	}
	kinds := make([]string, 0, len(by))
	for k := range by {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	all.Kind = ""
	if err := finish(&all, ScopeAll()); err != nil {
		return nil, err
	}
	for _, k := range kinds {
		if err := finish(by[k], ScopeKind(k)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// MaxRecordWindow is how many following actions make up a record's outcome.
const MaxRecordWindow = 5

// Records returns the holdout decision points since the cutoff, for an
// estimator: every shown item that faced a withholding chance and every item
// withheld, each with the outcome of the actions that followed. A decision
// followed by no observable action is left out. target "" returns all.
func (s *Store) Records(target string, since time.Time, limit int) ([]utilization.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 50000 {
		limit = 50000
	}
	var pts []utilization.Record
	q := `SELECT target, kind, session, stage, tool, ts, p_withhold, CASE WHEN cited=1 OR fp_hit>=` + strconv.Itoa(UsedMin) + ` THEN 1 ELSE 0 END,
		EXISTS (SELECT 1 FROM trace_links l WHERE l.injection_id=injections.id)
		FROM injections WHERE p_withhold>0 AND ts>=?`
	args := []any{since.Unix()}
	if target != "" {
		q += ` AND target=?`
		args = append(args, target)
	}
	rs, err := s.db.Query(q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		r := utilization.Record{Treated: true}
		var up, inf int
		if err := rs.Scan(&r.Target, &r.Kind, &r.Session, &r.Stage, &r.Tool, &r.TS, &r.PWithhold, &up, &inf); err != nil {
			rs.Close()
			return nil, err
		}
		r.Uptake, r.Influenced = up == 1, inf == 1
		pts = append(pts, r)
	}
	rs.Close()
	q = `SELECT target, kind, session, stage, tool, ts, p_withhold FROM trace_withheld WHERE ts>=?`
	args = []any{since.Unix()}
	if target != "" {
		q += ` AND target=?`
		args = append(args, target)
	}
	wr, err := s.db.Query(q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	for wr.Next() {
		var r utilization.Record
		if err := wr.Scan(&r.Target, &r.Kind, &r.Session, &r.Stage, &r.Tool, &r.TS, &r.PWithhold); err != nil {
			wr.Close()
			return nil, err
		}
		pts = append(pts, r)
	}
	wr.Close()
	var out []utilization.Record
	for _, r := range pts {
		ar, err := s.db.Query(`SELECT `+badSQL+` FROM trace_actions a WHERE a.session=? AND a.ts>=? AND `+knownSQL+`
			ORDER BY a.ts, a.id LIMIT ?`, r.Session, r.TS, MaxRecordWindow)
		if err != nil {
			return nil, err
		}
		bad := 0
		for ar.Next() {
			var b int
			if err := ar.Scan(&b); err != nil {
				ar.Close()
				return nil, err
			}
			r.Actions++
			bad += b
		}
		ar.Close()
		if r.Actions == 0 {
			continue
		}
		r.Y = float64(bad) / float64(r.Actions)
		out = append(out, r)
	}
	return out, nil
}

// ActionRow is an executed call as the trace holds it: codes only.
type ActionRow struct {
	ID         int64  `json:"id"`
	Seq        int    `json:"seq"`
	Tool       string `json:"tool"`
	Stage      string `json:"stage"`
	TS         int64  `json:"ts"`
	Failed     int    `json:"failed"` // -1 unknown
	TestsPass  int    `json:"tests_passed"`
	TestsFail  int    `json:"tests_failed"`
	Reedit     bool   `json:"re_edited"`
	Revert     bool   `json:"reverted"`
	Thrash     int    `json:"thrash"`
	Denied     bool   `json:"denied"`
	Correction bool   `json:"corrected"`
}

// Actions lists a session's executed calls in order.
func (s *Store) Actions(session string) ([]ActionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.db.Query(`SELECT id, seq, tool, stage, ts, failed, tests_pass, tests_fail, reedit, revert, thrash, denied, correction
		FROM trace_actions WHERE session=? ORDER BY seq`, session)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []ActionRow
	for rs.Next() {
		var a ActionRow
		var re, rv, dn, co int
		if err := rs.Scan(&a.ID, &a.Seq, &a.Tool, &a.Stage, &a.TS, &a.Failed, &a.TestsPass, &a.TestsFail, &re, &rv, &a.Thrash, &dn, &co); err != nil {
			return nil, err
		}
		a.Reedit, a.Revert, a.Denied, a.Correction = re == 1, rv == 1, dn == 1, co == 1
		out = append(out, a)
	}
	return out, rs.Err()
}
