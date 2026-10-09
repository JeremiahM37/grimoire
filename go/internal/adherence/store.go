// Package adherence records which memories were injected into an agent's
// context and what the agent then did, so "did the memory work" is measured
// from behaviour (a cited tag, a passed or failed check) and not from a
// grader model.
//
// It owns one small SQLite file beside the index. Nothing in it is part of
// anyone's notes; it is derived telemetry and is bounded: injections older
// than Retention are dropped.
package adherence

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Retention is how long an injection row is kept.
const Retention = 30 * 24 * time.Hour

// maxRows is a hard bound on the injection log whatever the retention says.
const maxRows = 200000

// Outcomes, in the order they win when several apply to one injection.
const (
	Violated     = "violated"
	Contradicted = "contradicted"
	Cited        = "cited"
	Followed     = "followed"
	Ignored      = "ignored"
	Pending      = "pending"
)

// Store is the adherence database.
type Store struct {
	db        *sql.DB
	mu        sync.Mutex
	lastPrune time.Time
}

// Open opens (creating) the database in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "adherence.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("adherence schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

var schema = []string{
	`CREATE TABLE IF NOT EXISTS injections (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session TEXT NOT NULL, tag TEXT NOT NULL, key TEXT NOT NULL,
		target TEXT NOT NULL, path TEXT NOT NULL DEFAULT '', fact_id TEXT NOT NULL DEFAULT '',
		stage TEXT NOT NULL DEFAULT '', relevance REAL NOT NULL DEFAULT 0, ts INTEGER NOT NULL,
		tools INTEGER NOT NULL DEFAULT 0,
		cited INTEGER NOT NULL DEFAULT 0, check_result TEXT NOT NULL DEFAULT '',
		contradicted INTEGER NOT NULL DEFAULT 0, finalized INTEGER NOT NULL DEFAULT 0,
		counted INTEGER NOT NULL DEFAULT 0)`,
	`CREATE INDEX IF NOT EXISTS injections_session ON injections(session, finalized)`,
	`CREATE INDEX IF NOT EXISTS injections_target ON injections(target, ts)`,
	`CREATE INDEX IF NOT EXISTS injections_ts ON injections(ts)`,
	`CREATE TABLE IF NOT EXISTS checks (
		target TEXT PRIMARY KEY, forbid TEXT NOT NULL DEFAULT '', require TEXT NOT NULL DEFAULT '',
		enforce TEXT NOT NULL DEFAULT '', tools TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT 'hand',
		ts INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS suggestions (
		target TEXT PRIMARY KEY, forbid TEXT NOT NULL DEFAULT '', require TEXT NOT NULL DEFAULT '',
		enforce TEXT NOT NULL DEFAULT '', tools TEXT NOT NULL DEFAULT '', confidence REAL NOT NULL DEFAULT 0,
		reason TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', text TEXT NOT NULL DEFAULT '',
		ts INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS gate_log (
		ts INTEGER NOT NULL, ms INTEGER NOT NULL, candidates INTEGER NOT NULL,
		kept INTEGER NOT NULL, dropped INTEGER NOT NULL, timed_out INTEGER NOT NULL)`,
}

// Injection is one memory shown to a session.
type Injection struct {
	Session, Tag, Key, Target, Path, FactID, Stage string
	Relevance                                      float64
}

// Tags returns the shortest hex prefix of each key (at least min characters)
// that is unique within the set. The tag is a prefix of the key, so it is
// deterministic and a grader needs no model to resolve it.
func Tags(keys []string, min int) []string {
	if min < 4 {
		min = 4
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		n := min
		for ; n < len(k); n++ {
			unique := true
			for j, o := range keys {
				if j != i && strings.HasPrefix(o, k[:n]) {
					unique = false
					break
				}
			}
			if unique {
				break
			}
		}
		if n > len(k) {
			n = len(k)
		}
		out[i] = k[:n]
	}
	return out
}

// TagRE finds citations of the form m:3e99 in agent text.
var TagRE = regexp.MustCompile(`\bm:([0-9a-f]{4,8})\b`)

// ExtractTags returns the distinct tags (without the "m:") found in text.
func ExtractTags(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range TagRE.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// Log records injections. It also prunes old rows, at most once a minute.
func (s *Store) Log(items []Injection, now time.Time) error {
	if len(items) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, it := range items {
		if _, err := tx.Exec(`INSERT INTO injections(session,tag,key,target,path,fact_id,stage,relevance,ts) VALUES(?,?,?,?,?,?,?,?,?)`,
			it.Session, it.Tag, it.Key, it.Target, it.Path, it.FactID, it.Stage, it.Relevance, now.Unix()); err != nil {
			tx.Rollback()
			return err
		}
	}
	if now.Sub(s.lastPrune) > time.Minute {
		s.lastPrune = now
		tx.Exec(`DELETE FROM injections WHERE ts < ?`, now.Add(-Retention).Unix())
		tx.Exec(`DELETE FROM injections WHERE id <= (SELECT COALESCE(MAX(id),0) FROM injections) - ?`, maxRows)
		tx.Exec(`DELETE FROM gate_log WHERE ts < ?`, now.Add(-Retention).Unix())
	}
	return tx.Commit()
}

// Row is an injection as stored.
type Row struct {
	ID                               int64
	Session, Tag, Key, Target, Path  string
	FactID, Stage                    string
	Relevance                        float64
	TS                               int64
	Tools                            int
	Cited                            bool
	CheckResult                      string
	Contradicted, Finalized, Counted bool
}

// Outcome is the final classification of a row.
func (r Row) Outcome() string {
	switch {
	case r.CheckResult == Violated:
		return Violated
	case r.Contradicted:
		return Contradicted
	case r.Cited:
		return Cited
	case r.CheckResult == Followed:
		return Followed
	case r.Finalized:
		return Ignored
	}
	return Pending
}

const rowCols = `id,session,tag,key,target,path,fact_id,stage,relevance,ts,tools,cited,check_result,contradicted,finalized,counted`

func scanRows(rs *sql.Rows) ([]Row, error) {
	defer rs.Close()
	var out []Row
	for rs.Next() {
		var r Row
		var c, ct, f, n int
		if err := rs.Scan(&r.ID, &r.Session, &r.Tag, &r.Key, &r.Target, &r.Path, &r.FactID, &r.Stage,
			&r.Relevance, &r.TS, &r.Tools, &c, &r.CheckResult, &ct, &f, &n); err != nil {
			return nil, err
		}
		r.Cited, r.Contradicted, r.Finalized, r.Counted = c != 0, ct != 0, f != 0, n != 0
		out = append(out, r)
	}
	return out, rs.Err()
}

// OpenRows returns a session's injections that have not been finalised and
// are newer than since.
func (s *Store) OpenRows(session string, since time.Time) ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.db.Query(`SELECT `+rowCols+` FROM injections WHERE session=? AND finalized=0 AND ts>=? ORDER BY id`,
		session, since.Unix())
	if err != nil {
		return nil, err
	}
	return scanRows(rs)
}

// SetCheck records a check result on one row ("followed" never overwrites
// "violated").
func (s *Store) SetCheck(id int64, result string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE injections SET check_result=? WHERE id=? AND check_result!='violated'`, result, id)
	return err
}

// BumpTools counts a tool call against every open row of a session.
func (s *Store) BumpTools(session string, since time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE injections SET tools=tools+1 WHERE session=? AND finalized=0 AND ts>=?`, session, since.Unix())
	return err
}

// MarkCited flags the session's open rows whose tag was cited and returns how
// many changed.
func (s *Store) MarkCited(session string, tags []string, since time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range tags {
		res, err := s.db.Exec(`UPDATE injections SET cited=1 WHERE session=? AND finalized=0 AND ts>=? AND tag=? AND cited=0`,
			session, since.Unix(), t)
		if err != nil {
			return n, err
		}
		c, _ := res.RowsAffected()
		n += int(c)
	}
	return n, nil
}

// Finalize closes the session's open rows and returns them with their final
// outcome. A row that was never cited, never checked and never contradicted is
// "ignored".
func (s *Store) Finalize(session string, since time.Time) ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.db.Query(`SELECT `+rowCols+` FROM injections WHERE session=? AND finalized=0 AND ts>=? ORDER BY id`,
		session, since.Unix())
	if err != nil {
		return nil, err
	}
	rows, err := scanRows(rs)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].Finalized = true
		if _, err := s.db.Exec(`UPDATE injections SET finalized=1 WHERE id=?`, rows[i].ID); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// MarkCounted notes that a row's outcome was fed to the feedback counters.
func (s *Store) MarkCounted(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Exec(`UPDATE injections SET counted=1 WHERE id=?`, id)
}

// Contradict marks the newest injection of target within window as
// contradicted by a later re-tell. It returns that row when it changed.
func (s *Store) Contradict(target string, window time.Duration, now time.Time) (*Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.db.Query(`SELECT `+rowCols+` FROM injections WHERE target=? AND ts>=? AND contradicted=0 ORDER BY id DESC LIMIT 1`,
		target, now.Add(-window).Unix())
	if err != nil {
		return nil, err
	}
	rows, err := scanRows(rs)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	if _, err := s.db.Exec(`UPDATE injections SET contradicted=1 WHERE id=?`, rows[0].ID); err != nil {
		return nil, err
	}
	rows[0].Contradicted = true
	return &rows[0], nil
}

// Stats are per-target outcome counts.
type Stats struct {
	Target                                                              string
	Path, FactID                                                        string
	Injected, Cited, Followed, Violated, Ignored, Contradicted, Pending int
}

// Rate is the share of finished injections that were acted on (cited or
// followed).
func (s Stats) Rate() float64 {
	done := s.Injected - s.Pending
	if done <= 0 {
		return 0
	}
	return float64(s.Cited+s.Followed) / float64(done)
}

// Report summarises outcomes since the cutoff, per target and overall.
func (s *Store) Report(since time.Time) (map[string]*Stats, Stats, error) {
	s.mu.Lock()
	rs, err := s.db.Query(`SELECT `+rowCols+` FROM injections WHERE ts>=?`, since.Unix())
	s.mu.Unlock()
	if err != nil {
		return nil, Stats{}, err
	}
	rows, err := scanRows(rs)
	if err != nil {
		return nil, Stats{}, err
	}
	by := map[string]*Stats{}
	var all Stats
	for _, r := range rows {
		st := by[r.Target]
		if st == nil {
			st = &Stats{Target: r.Target, Path: r.Path, FactID: r.FactID}
			by[r.Target] = st
		}
		for _, x := range []*Stats{st, &all} {
			x.Injected++
			switch r.Outcome() {
			case Violated:
				x.Violated++
			case Contradicted:
				x.Contradicted++
			case Cited:
				x.Cited++
			case Followed:
				x.Followed++
			case Ignored:
				x.Ignored++
			default:
				x.Pending++
			}
		}
	}
	return by, all, nil
}

// IgnoredPenalty returns a multiplier in (0,1] for injection ranking. A
// target injected at least minTimes in the window and acted on never (all
// finished injections ignored) is down-ranked, more the longer it has been
// ignored. Ranking for recall is untouched.
func (s *Store) IgnoredPenalty(targets []string, since time.Time) map[string]float64 {
	out := map[string]float64{}
	if len(targets) == 0 {
		return out
	}
	by, _, err := s.Report(since)
	if err != nil {
		return out
	}
	for _, t := range targets {
		st := by[t]
		if st == nil {
			continue
		}
		if p := Penalty(*st); p < 1 {
			out[t] = p
		}
	}
	return out
}

// Penalty is the injection multiplier for a target's history: 1 until it has
// been ignored five times with fewer than a fifth of finished injections acted
// on, then falling to 0.6 at twenty.
func Penalty(st Stats) float64 {
	if st.Ignored < 5 || st.Rate() >= 0.2 {
		return 1
	}
	over := float64(st.Ignored-5) / 15
	if over > 1 {
		over = 1
	}
	return 0.9 - 0.3*over
}

// ---- checks ----

// Check is a regex test over a tool call's target.
type Check struct {
	Target  string   `json:"target"`
	Forbid  string   `json:"forbid,omitempty"`
	Require string   `json:"require,omitempty"`
	Enforce string   `json:"enforce,omitempty"` // "" or "ask"
	Tools   []string `json:"tools,omitempty"`   // limit to these tool names
	Source  string   `json:"source,omitempty"`  // hand | accepted
	TS      int64    `json:"ts,omitempty"`
}

// MaxPattern bounds a stored regex.
const MaxPattern = 300

// Validate checks a Check is usable. Go's regexp is linear-time, so a hostile
// pattern cannot stall the server; the length bound keeps rows small.
func (c Check) Validate() error {
	if c.Forbid == "" && c.Require == "" {
		return fmt.Errorf("a check needs forbid or require")
	}
	for _, p := range []string{c.Forbid, c.Require} {
		if len(p) > MaxPattern {
			return fmt.Errorf("pattern longer than %d bytes", MaxPattern)
		}
		if p != "" {
			if _, err := regexp.Compile(p); err != nil {
				return fmt.Errorf("bad pattern: %v", err)
			}
		}
	}
	if c.Enforce != "" && c.Enforce != "ask" {
		return fmt.Errorf("enforce must be empty or ask")
	}
	if c.Enforce == "ask" && c.Forbid == "" {
		return fmt.Errorf("enforce: ask needs a forbid pattern")
	}
	return nil
}

// AppliesTo reports whether the check covers a tool.
func (c Check) AppliesTo(tool string) bool {
	if len(c.Tools) == 0 {
		return true
	}
	for _, t := range c.Tools {
		if strings.EqualFold(t, tool) {
			return true
		}
	}
	return false
}

// Violates reports whether target matches the forbid pattern.
func (c Check) Violates(target string) bool {
	if c.Forbid == "" {
		return false
	}
	re, err := regexp.Compile(c.Forbid)
	return err == nil && re.MatchString(target)
}

// Satisfies reports whether target matches the require pattern.
func (c Check) Satisfies(target string) bool {
	if c.Require == "" {
		return false
	}
	re, err := regexp.Compile(c.Require)
	return err == nil && re.MatchString(target)
}

// UpsertCheck stores (replaces) a check.
func (s *Store) UpsertCheck(c Check, now time.Time) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Source == "" {
		c.Source = "hand"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO checks(target,forbid,require,enforce,tools,source,ts) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(target) DO UPDATE SET forbid=excluded.forbid, require=excluded.require, enforce=excluded.enforce,
		tools=excluded.tools, source=excluded.source, ts=excluded.ts`,
		c.Target, c.Forbid, c.Require, c.Enforce, strings.Join(c.Tools, ","), c.Source, now.Unix())
	return err
}

// DeleteCheck removes a check.
func (s *Store) DeleteCheck(target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM checks WHERE target=?`, target)
	return err
}

func scanChecks(rs *sql.Rows) ([]Check, error) {
	defer rs.Close()
	var out []Check
	for rs.Next() {
		var c Check
		var tools string
		if err := rs.Scan(&c.Target, &c.Forbid, &c.Require, &c.Enforce, &tools, &c.Source, &c.TS); err != nil {
			return nil, err
		}
		if tools != "" {
			c.Tools = strings.Split(tools, ",")
		}
		out = append(out, c)
	}
	return out, rs.Err()
}

// Checks returns the checks for the given targets, or all when none are given.
func (s *Store) Checks(targets ...string) (map[string]Check, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.db.Query(`SELECT target,forbid,require,enforce,tools,source,ts FROM checks`)
	if err != nil {
		return nil, err
	}
	all, err := scanChecks(rs)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, t := range targets {
		want[t] = true
	}
	out := map[string]Check{}
	for _, c := range all {
		if len(want) == 0 || want[c.Target] {
			out[c.Target] = c
		}
	}
	return out, nil
}

// ---- suggestions ----

// Suggestion is a proposed check awaiting a person.
type Suggestion struct {
	Check
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason,omitempty"`
	Model      string  `json:"model,omitempty"`
	Text       string  `json:"text,omitempty"`
}

// PutSuggestion stores a proposal. A target that already has a hand-written
// check is never proposed for.
func (s *Store) PutSuggestion(g Suggestion, now time.Time) error {
	if err := g.Check.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO suggestions(target,forbid,require,enforce,tools,confidence,reason,model,text,ts) VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(target) DO UPDATE SET forbid=excluded.forbid, require=excluded.require, enforce=excluded.enforce,
		tools=excluded.tools, confidence=excluded.confidence, reason=excluded.reason, model=excluded.model, text=excluded.text, ts=excluded.ts`,
		g.Target, g.Forbid, g.Require, g.Enforce, strings.Join(g.Tools, ","), g.Confidence, g.Reason, g.Model, g.Text, now.Unix())
	return err
}

// Suggestions lists proposals, newest first.
func (s *Store) Suggestions() ([]Suggestion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.db.Query(`SELECT target,forbid,require,enforce,tools,confidence,reason,model,text,ts FROM suggestions ORDER BY ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []Suggestion
	for rs.Next() {
		var g Suggestion
		var tools string
		if err := rs.Scan(&g.Target, &g.Forbid, &g.Require, &g.Enforce, &tools, &g.Confidence, &g.Reason, &g.Model, &g.Text, &g.TS); err != nil {
			return nil, err
		}
		if tools != "" {
			g.Tools = strings.Split(tools, ",")
		}
		out = append(out, g)
	}
	return out, rs.Err()
}

// DeleteSuggestion drops a proposal.
func (s *Store) DeleteSuggestion(target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM suggestions WHERE target=?`, target)
	return err
}

// ---- gate telemetry ----

// GateCall is one gate pass.
type GateCall struct {
	MS                        int64
	Candidates, Kept, Dropped int
	TimedOut                  bool
}

// LogGate records one gate pass.
func (s *Store) LogGate(g GateCall, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	to := 0
	if g.TimedOut {
		to = 1
	}
	s.db.Exec(`INSERT INTO gate_log(ts,ms,candidates,kept,dropped,timed_out) VALUES(?,?,?,?,?,?)`,
		now.Unix(), g.MS, g.Candidates, g.Kept, g.Dropped, to)
}

// GateSummary reports gate latency percentiles and counts since the cutoff.
type GateSummary struct {
	Calls    int   `json:"calls"`
	TimedOut int   `json:"timed_out"`
	Kept     int   `json:"kept"`
	Dropped  int   `json:"dropped"`
	P50MS    int64 `json:"p50_ms"`
	P95MS    int64 `json:"p95_ms"`
}

// Gate summarises the gate log.
func (s *Store) Gate(since time.Time) (GateSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.db.Query(`SELECT ms,kept,dropped,timed_out FROM gate_log WHERE ts>=?`, since.Unix())
	if err != nil {
		return GateSummary{}, err
	}
	defer rs.Close()
	var g GateSummary
	var ms []int64
	for rs.Next() {
		var m int64
		var k, d, t int
		if err := rs.Scan(&m, &k, &d, &t); err != nil {
			return g, err
		}
		ms = append(ms, m)
		g.Calls++
		g.Kept += k
		g.Dropped += d
		g.TimedOut += t
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i] < ms[j] })
	if n := len(ms); n > 0 {
		g.P50MS = ms[n*50/100]
		i := (n*95 + 99) / 100
		if i > n {
			i = n
		}
		g.P95MS = ms[i-1]
	}
	return g, rs.Err()
}
