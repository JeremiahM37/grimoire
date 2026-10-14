// Package anchorlog keeps the history of anchor probe results: one row per
// verdict, with the hash of what the probe saw (anchor.Result.Observed) and
// never the content itself.
//
// The history answers two questions the verdict alone cannot. A pass that
// turned into a fail is a survival label: it says how long a claim of that kind
// stayed true. A pass whose observed hash changed is drift: the check still
// holds, but the thing it checked is not the same thing any more.
//
// The store lives beside the other per-feature databases (rules.db, adherence)
// in the vault's .grimoire directory. It is append-only and bounded: at most
// MaxPerAnchor rows are kept for each (fact, anchor) pair, oldest pruned first.
package anchorlog

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/anchor"

	_ "modernc.org/sqlite"
)

// MaxPerAnchor bounds the rows kept for one fact and one anchor.
const MaxPerAnchor = 200

// Origin names the caller that ran a probe.
const (
	OriginWrite    = "write"    // a memory write, or a procedure write
	OriginRecall   = "recall"   // a fresh probe while recalling a fact
	OriginDream    = "dream"    // reserved: no caller runs anchors from dream today
	OriginAnnotate = "annotate" // grimoire memory annotate
	OriginCLI      = "cli"      // the operator's admin report (grimoire memory anchors)
)

// State values, from State.
const (
	Live    = "live"
	Dead    = "dead"
	Drifted = "drifted"
	New     = "new"
)

// Row is one stored verdict. Anchor is the stable key of the probe (KeyOf), not
// its arguments.
type Row struct {
	FactID   string `json:"fact_id"`
	Anchor   string `json:"anchor"`
	OK       bool   `json:"ok"`
	Observed string `json:"observed,omitempty"`
	Why      string `json:"why,omitempty"`
	TS       int64  `json:"ts"`
	Origin   string `json:"origin"`
}

// Transition is a change between two consecutive verdicts of one anchor. Kind is
// "pass->fail", "fail->pass" or "drifted".
type Transition struct {
	FactID string `json:"fact_id"`
	Anchor string `json:"anchor"`
	Kind   string `json:"kind"`
	TS     int64  `json:"ts"`
	Origin string `json:"origin"`
}

// KeyOf is the stable hash of an anchor's type and arguments. Arguments are
// encoded in sorted key order by anchor.Encode, so the key does not depend on
// map order.
func KeyOf(a anchor.Anchor) string {
	h := sha256.Sum256([]byte(anchor.Encode([]anchor.Anchor{a})))
	return hex.EncodeToString(h[:16])
}

// State classifies the current verdict against the one before it. prev is nil
// for the first observation. A dead anchor is dead whatever came before it; a
// first pass is new; a pass whose observed value changed is drifted; anything
// else still holds.
func State(prev *Row, cur Row) string {
	switch {
	case !cur.OK:
		return Dead
	case prev != nil && prev.OK && prev.Observed != "" && cur.Observed != "" && prev.Observed != cur.Observed:
		return Drifted
	case prev == nil:
		return New
	}
	return Live
}

// Transitions lists every change between consecutive verdicts of the same
// (fact, anchor) pair, oldest first. The input may be in any order.
func Transitions(rows []Row) []Transition {
	sorted := append([]Row(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.FactID != b.FactID {
			return a.FactID < b.FactID
		}
		if a.Anchor != b.Anchor {
			return a.Anchor < b.Anchor
		}
		return a.TS < b.TS
	})
	var out []Transition
	for i := 1; i < len(sorted); i++ {
		prev, cur := sorted[i-1], sorted[i]
		if prev.FactID != cur.FactID || prev.Anchor != cur.Anchor {
			continue
		}
		kind := ""
		switch {
		case prev.OK && !cur.OK:
			kind = "pass->fail"
		case !prev.OK && cur.OK:
			kind = "fail->pass"
		case State(&prev, cur) == Drifted:
			kind = Drifted
		}
		if kind != "" {
			out = append(out, Transition{FactID: cur.FactID, Anchor: cur.Anchor, Kind: kind, TS: cur.TS, Origin: cur.Origin})
		}
	}
	return out
}

// Store is the result history. A nil *Store is valid and records nothing, so a
// caller with no vault never has to check.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// Open opens (creating) anchors.db in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "anchors.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS anchor_results (fact_id TEXT NOT NULL, anchor TEXT NOT NULL, ok INTEGER NOT NULL,
			observed TEXT NOT NULL DEFAULT '', why TEXT NOT NULL DEFAULT '', ts INTEGER NOT NULL, origin TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS anchor_results_key ON anchor_results(fact_id, anchor, ts)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("anchor history schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	return s.db.Close()
}

// maxWhy bounds the stored message. Messages are short by contract; this is a
// backstop so a long error can never bloat the table.
const maxWhy = 200

// Record appends the verdict of one probe run for a fact. Only pass and fail are
// kept: an error is the probe failing to answer, and a skip is a probe that was
// not run here, so neither is a claim about the world. The table is then pruned
// to MaxPerAnchor rows for this (fact, anchor).
func (s *Store) Record(factID, origin string, a anchor.Anchor, res anchor.Result) error {
	if s == nil || factID == "" {
		return nil
	}
	if res.Status != anchor.Pass && res.Status != anchor.Fail {
		return nil
	}
	key := KeyOf(a)
	ok := 0
	if res.Status == anchor.Pass {
		ok = 1
	}
	why := res.Message
	if len(why) > maxWhy {
		why = why[:maxWhy]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO anchor_results(fact_id, anchor, ok, observed, why, ts, origin) VALUES(?,?,?,?,?,?,?)`,
		factID, key, ok, res.Observed, why, time.Now().Unix(), origin); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM anchor_results WHERE fact_id=? AND anchor=? AND rowid NOT IN (
		SELECT rowid FROM anchor_results WHERE fact_id=? AND anchor=? ORDER BY ts DESC, rowid DESC LIMIT ?)`,
		factID, key, factID, key, MaxPerAnchor); err != nil {
		return err
	}
	return tx.Commit()
}

// LatestState is the state of an anchor on a fact, from its two newest verdicts.
// It is "" when nothing has been recorded for the pair.
func (s *Store) LatestState(factID string, a anchor.Anchor) (string, error) {
	if s == nil {
		return "", nil
	}
	rows, err := s.query(`SELECT fact_id, anchor, ok, observed, why, ts, origin FROM anchor_results
		WHERE fact_id=? AND anchor=? ORDER BY ts DESC, rowid DESC LIMIT 2`, factID, KeyOf(a))
	if err != nil || len(rows) == 0 {
		return "", err
	}
	if len(rows) == 1 {
		return State(nil, rows[0]), nil
	}
	return State(&rows[1], rows[0]), nil
}

// Recent returns the newest rows, newest first, for one fact (or every fact when
// factID is ""), at most limit of them.
func (s *Store) Recent(factID string, limit int) ([]Row, error) {
	if s == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 500
	}
	if factID == "" {
		return s.query(`SELECT fact_id, anchor, ok, observed, why, ts, origin FROM anchor_results
			ORDER BY ts DESC, rowid DESC LIMIT ?`, limit)
	}
	return s.query(`SELECT fact_id, anchor, ok, observed, why, ts, origin FROM anchor_results
		WHERE fact_id=? ORDER BY ts DESC, rowid DESC LIMIT ?`, factID, limit)
}

func (s *Store) query(q string, args ...any) ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []Row
	for rs.Next() {
		var r Row
		var ok int
		if err := rs.Scan(&r.FactID, &r.Anchor, &ok, &r.Observed, &r.Why, &r.TS, &r.Origin); err != nil {
			return nil, err
		}
		r.OK = ok == 1
		out = append(out, r)
	}
	return out, rs.Err()
}
