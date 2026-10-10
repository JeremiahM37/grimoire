package rulecheck

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Row is one stored check with its evidence and status.
type Row struct {
	ID         string          `json:"id"`
	Target     string          `json:"target"` // the memory it came from (note:<path> or fact:<id>)
	RuleText   string          `json:"rule_text"`
	Spec       Spec            `json:"spec"`
	Source     string          `json:"source"`
	Why        string          `json:"why,omitempty"`
	Scanned    int             `json:"calls_scanned"`
	Actions    int             `json:"actions"`
	Matches    int             `json:"matches"`
	Sessions   int             `json:"sessions_affected"`
	MatchRate  float64         `json:"match_rate"`
	Labelled   int             `json:"labelled"`
	True       int             `json:"true_violations"`
	LiveTrue   int             `json:"live_true"`
	LiveFalse  int             `json:"live_false"`
	Retold     int             `json:"retold"`
	Precision  float64         `json:"precision"`
	CILow      float64         `json:"ci95_low"`
	CIHigh     float64         `json:"ci95_high"`
	Status     string          `json:"status"`
	Reason     string          `json:"reason"`
	User       string          `json:"user_state,omitempty"`
	FaithP     float64         `json:"faithful_p,omitempty"`
	Unfaithful bool            `json:"unfaithful,omitempty"`
	Sample     []LabelledMatch `json:"sample,omitempty"` // the labelled matches behind the precision
	Backtest   time.Time       `json:"backtest_at,omitempty"`
	Updated    time.Time       `json:"updated"`
}

// ID derives a stable id from the memory and the spec.
func ID(target string, sp Spec) string {
	b, _ := json.Marshal(sp)
	h := sha256.Sum256(append([]byte(target+"\x00"), b...))
	return hex.EncodeToString(h[:5])
}

// Evidence builds the policy input from a row (historical plus live labels).
func (r Row) Evidence(prev string) Evidence {
	return Evidence{RuleText: r.RuleText, Shape: r.Spec.Shape, Labelled: r.Labelled + r.LiveTrue + r.LiveFalse,
		True: r.True + r.LiveTrue, Matches: r.Matches, MatchRate: r.MatchRate, User: r.User, Previous: prev, Unfaithful: r.Unfaithful}
}

// Rescore recomputes precision, its interval and the status.
func (r *Row) Rescore(p Policy) {
	e := r.Evidence(r.Status)
	r.Precision = e.Precision()
	r.CILow, r.CIHigh = Wilson(e.True, e.Labelled)
	r.Status, r.Reason = p.Decide(e)
}

// Store is rules.db, beside the adherence log.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// Open opens (creating) rules.db in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "rules.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS rule_checks (id TEXT PRIMARY KEY, target TEXT NOT NULL, body TEXT NOT NULL,
			status TEXT NOT NULL, user_state TEXT NOT NULL DEFAULT '', updated INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS rule_checks_target ON rule_checks(target)`,
		`CREATE TABLE IF NOT EXISTS rule_firings (id INTEGER PRIMARY KEY AUTOINCREMENT, check_id TEXT NOT NULL,
			session TEXT NOT NULL, ts INTEGER NOT NULL, label INTEGER)`,
		`CREATE INDEX IF NOT EXISTS rule_firings_check ON rule_firings(check_id, ts)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("rules schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Put stores (replaces) a row.
func (s *Store) Put(r Row) error {
	r.Updated = time.Now()
	b, _ := json.Marshal(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO rule_checks(id,target,body,status,user_state,updated) VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET body=excluded.body, status=excluded.status, user_state=excluded.user_state, updated=excluded.updated`,
		r.ID, r.Target, string(b), r.Status, r.User, r.Updated.Unix())
	return err
}

// All lists every row, best status first.
func (s *Store) All() ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.db.Query(`SELECT body FROM rule_checks`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []Row
	for rs.Next() {
		var b string
		if err := rs.Scan(&b); err != nil {
			return nil, err
		}
		var r Row
		if json.Unmarshal([]byte(b), &r) == nil {
			out = append(out, r)
		}
	}
	rank := map[string]int{StatusEnforce: 0, StatusActive: 1, StatusSuggestion: 2, StatusDisabled: 3}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Status] != rank[out[j].Status] {
			return rank[out[i].Status] < rank[out[j].Status]
		}
		if out[i].Precision != out[j].Precision {
			return out[i].Precision > out[j].Precision
		}
		return out[i].ID < out[j].ID
	})
	return out, rs.Err()
}

// Get returns one row by id or unique id prefix.
func (s *Store) Get(id string) (Row, bool) {
	all, _ := s.All()
	var hit []Row
	for _, r := range all {
		if r.ID == id {
			return r, true
		}
		if len(id) >= 4 && len(r.ID) >= len(id) && r.ID[:len(id)] == id {
			hit = append(hit, r)
		}
	}
	if len(hit) == 1 {
		return hit[0], true
	}
	return Row{}, false
}

// ReplaceTarget swaps the checks stored for a memory with fresh ones, keeping
// a person's enable/disable and the live labels of any check that survives.
func (s *Store) ReplaceTarget(target string, rows []Row, p Policy) error {
	old, _ := s.All()
	keep := map[string]Row{}
	for _, r := range old {
		if r.Target == target {
			keep[r.ID] = r
		}
	}
	s.mu.Lock()
	_, err := s.db.Exec(`DELETE FROM rule_checks WHERE target=?`, target)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for _, r := range rows {
		if o, ok := keep[r.ID]; ok {
			r.User, r.LiveTrue, r.LiveFalse, r.Status = o.User, o.LiveTrue, o.LiveFalse, o.Status
			if r.Labelled == 0 && r.Matches == 0 && o.Labelled > 0 {
				r.Labelled, r.True, r.Matches, r.Sample = o.Labelled, o.True, o.Matches, o.Sample
			}
		}
		r.Rescore(p)
		if err := s.Put(r); err != nil {
			return err
		}
	}
	return nil
}

// SetUser records a person's decision and rescores.
func (s *Store) SetUser(id, state string, p Policy) (Row, error) {
	r, ok := s.Get(id)
	if !ok {
		return Row{}, fmt.Errorf("no check %q", id)
	}
	r.User = state
	r.Rescore(p)
	return r, s.Put(r)
}

// Fire records that an active check flagged a live call and returns the
// firing id, which a later label refers to.
func (s *Store) Fire(checkID, session string, now time.Time) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`INSERT INTO rule_firings(check_id,session,ts) VALUES(?,?,?)`, checkID, session, now.Unix())
	if err != nil {
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

// LabelFiring records whether a live firing was a true violation, folds it into
// the check's precision and re-decides its status (so a bad run demotes it).
func (s *Store) LabelFiring(firing int64, truth bool, p Policy) (Row, error) {
	s.mu.Lock()
	var checkID string
	var cur sql.NullInt64
	err := s.db.QueryRow(`SELECT check_id, label FROM rule_firings WHERE id=?`, firing).Scan(&checkID, &cur)
	s.mu.Unlock()
	if err != nil {
		return Row{}, fmt.Errorf("no such firing")
	}
	if cur.Valid {
		return Row{}, fmt.Errorf("firing already labelled")
	}
	v := 0
	if truth {
		v = 1
	}
	s.mu.Lock()
	s.db.Exec(`UPDATE rule_firings SET label=? WHERE id=?`, v, firing)
	s.mu.Unlock()
	r, ok := s.Get(checkID)
	if !ok {
		return Row{}, fmt.Errorf("check gone")
	}
	if truth {
		r.LiveTrue++
	} else {
		r.LiveFalse++
	}
	r.Rescore(p)
	return r, s.Put(r)
}

// NewestOpenFiring finds the newest unlabelled firing of any check belonging to
// target within window, for automatic labelling by a re-tell.
func (s *Store) NewestOpenFiring(target string, window time.Duration, now time.Time) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var id int64
	err := s.db.QueryRow(`SELECT f.id FROM rule_firings f JOIN rule_checks c ON c.id=f.check_id
		WHERE c.target=? AND f.label IS NULL AND f.ts>=? ORDER BY f.id DESC LIMIT 1`, target, now.Add(-window).Unix()).Scan(&id)
	return id, err == nil
}
