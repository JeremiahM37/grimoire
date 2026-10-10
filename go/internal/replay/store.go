package replay

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// Bounds. The corpus is derived telemetry beside the index (never part of the
// notes, never synced); every dimension of it is capped.
const (
	// MaxText is the most request text kept per situation, in characters.
	MaxText = 400
	// DefaultMaxLive and DefaultMaxSeed bound the situation counts.
	DefaultMaxLive = 5000
	DefaultMaxSeed = 5000
	// DefaultRetention is how long a live situation survives without being
	// seen again. Seeds do not age.
	DefaultRetention = 60 * 24 * time.Hour
	// MaxExpect bounds the memories remembered per situation.
	MaxExpect = 12
	// firedKeep is how long an unresolved firing waits for its outcome.
	firedKeep = 14 * 24 * time.Hour
	// settle is how long after a firing its outcome is still unknown.
	settle = 10 * time.Minute
)

// Weight of each outcome as evidence the memory was right to fire. A violated
// memory fired correctly (the agent erred), so it still counts; a pending one
// is weak; ignored and contradicted are not useful.
var OutcomeWeight = map[string]float64{
	"cited": 1, "followed": 0.7, "violated": 0.5, "pending": 0.2,
	"ignored": 0, "contradicted": 0,
}

// Limits sets the bounds; zero values mean the defaults.
type Limits struct {
	MaxLive, MaxSeed int
	Retention        time.Duration
}

func (l Limits) live() int {
	if l.MaxLive > 0 {
		return l.MaxLive
	}
	return DefaultMaxLive
}
func (l Limits) seed() int {
	if l.MaxSeed > 0 {
		return l.MaxSeed
	}
	return DefaultMaxSeed
}
func (l Limits) ret() time.Duration {
	if l.Retention > 0 {
		return l.Retention
	}
	return DefaultRetention
}

// Store is the situation corpus.
type Store struct {
	db     *sql.DB
	mu     sync.Mutex
	Limits Limits
	// Settle is how long after a firing its outcome is still unknown.
	Settle time.Duration
	last   time.Time
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS situations (
		id TEXT PRIMARY KEY, text TEXT NOT NULL, stage TEXT NOT NULL DEFAULT '',
		min_rel REAL NOT NULL DEFAULT 0, lim INTEGER NOT NULL DEFAULT 0, bud INTEGER NOT NULL DEFAULT 0,
		source TEXT NOT NULL DEFAULT 'live', n INTEGER NOT NULL DEFAULT 1,
		first_ts INTEGER NOT NULL, last_ts INTEGER NOT NULL,
		avoid TEXT NOT NULL DEFAULT '', vec BLOB, vec_sig TEXT NOT NULL DEFAULT '')`,
	`CREATE INDEX IF NOT EXISTS situations_last ON situations(source, last_ts)`,
	`CREATE TABLE IF NOT EXISTS expect (
		sit TEXT NOT NULL, target TEXT NOT NULL, weight REAL NOT NULL DEFAULT 0,
		outcome TEXT NOT NULL DEFAULT '', ts INTEGER NOT NULL,
		PRIMARY KEY(sit, target))`,
	`CREATE TABLE IF NOT EXISTS fired (
		sit TEXT NOT NULL, session TEXT NOT NULL, target TEXT NOT NULL, stage TEXT NOT NULL DEFAULT '',
		relevance REAL NOT NULL DEFAULT 0, ts INTEGER NOT NULL, resolved INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY(sit, session, target, ts))`,
	`CREATE INDEX IF NOT EXISTS fired_open ON fired(resolved, ts)`,
}

// Open opens (creating) replay.db in dir.
func Open(dir string, lim Limits) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "replay.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("replay schema: %w", err)
		}
	}
	return &Store{db: db, Limits: lim, Settle: settle}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

var secretish = regexp.MustCompile(`[A-Za-z0-9_\-]{24,}`)

// Clean bounds and scrubs request text before it is kept: whitespace is
// collapsed, long opaque tokens (keys, hashes, tokens pasted into a prompt)
// become "…", and the result is cut to MaxText characters.
func Clean(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	text = secretish.ReplaceAllStringFunc(text, func(tok string) string {
		for _, r := range tok {
			if r >= '0' && r <= '9' {
				return "…"
			}
		}
		return tok // a long plain word is not a secret
	})
	if utf8.RuneCountInString(text) > MaxText {
		text = string([]rune(text)[:MaxText])
	}
	return text
}

// ID is the identity of a situation: its text and stage.
func ID(text, stage string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(text) + "\x00" + stage))
	return hex.EncodeToString(sum[:8])
}

// Fire is one memory that fired.
type Fire struct {
	Target    string
	Relevance float64
}

// Note records a context request and what fired for it. It is called for every
// hybrid request that carries a session, including those that fired nothing
// (those are the situations that prove a change added noise).
func (s *Store) Note(session, text, stage string, minRel float64, limit, budget int, fired []Fire, now time.Time) error {
	text = Clean(text)
	if text == "" {
		return nil
	}
	id := ID(text, stage)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE situations SET n=n+1, last_ts=?, min_rel=?, lim=?, bud=? WHERE id=?`, now.Unix(), minRel, limit, budget, id)
	if err != nil {
		return err
	}
	if c, _ := res.RowsAffected(); c == 0 {
		if _, err := tx.Exec(`INSERT INTO situations(id,text,stage,min_rel,lim,bud,source,n,first_ts,last_ts) VALUES(?,?,?,?,?,?,'live',1,?,?)`,
			id, text, stage, minRel, limit, budget, now.Unix(), now.Unix()); err != nil {
			return err
		}
	}
	for _, f := range fired {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO fired(sit,session,target,stage,relevance,ts) VALUES(?,?,?,?,?,?)`,
			id, session, f.Target, stage, f.Relevance, now.Unix()); err != nil {
			return err
		}
		// A memory that fired is part of the baseline even before its outcome
		// is known: a later change that makes it fire elsewhere is noise.
		if _, err := tx.Exec(`INSERT OR IGNORE INTO expect(sit,target,weight,outcome,ts) VALUES(?,?,0,'',?)`, id, f.Target, now.Unix()); err != nil {
			return err
		}
	}
	if now.Sub(s.last) > time.Minute {
		s.last = now
		s.pruneTx(tx, now)
	}
	return tx.Commit()
}

func (s *Store) pruneTx(tx *sql.Tx, now time.Time) {
	tx.Exec(`DELETE FROM situations WHERE source='live' AND last_ts < ?`, now.Add(-s.Limits.ret()).Unix())
	tx.Exec(`DELETE FROM situations WHERE source='live' AND id NOT IN
		(SELECT id FROM situations WHERE source='live' ORDER BY last_ts DESC LIMIT ?)`, s.Limits.live())
	tx.Exec(`DELETE FROM situations WHERE source='seed' AND id NOT IN
		(SELECT id FROM situations WHERE source='seed' ORDER BY first_ts DESC, id LIMIT ?)`, s.Limits.seed())
	tx.Exec(`DELETE FROM expect WHERE sit NOT IN (SELECT id FROM situations)`)
	tx.Exec(`DELETE FROM fired WHERE sit NOT IN (SELECT id FROM situations) OR ts < ?`, now.Add(-firedKeep).Unix())
	// Per situation, keep the MaxExpect strongest expectations.
	tx.Exec(`DELETE FROM expect WHERE rowid IN (
		SELECT rowid FROM (SELECT rowid, ROW_NUMBER() OVER (PARTITION BY sit ORDER BY weight DESC, ts DESC) AS r FROM expect) WHERE r > ?)`, MaxExpect)
}

// Outcome is what the adherence log says about one injection.
type Outcome struct {
	Session, Target, Stage, Outcome string
	TS                              int64
}

// Resolve folds outcomes into expectations. Each unresolved firing older than
// the settle time is matched to the adherence row of the same session, target
// and stage within a few seconds; the situation keeps the best weight seen.
// It returns how many firings were resolved.
func (s *Store) Resolve(outcomes []Outcome, now time.Time) (int, error) {
	idx := map[string][]Outcome{}
	for _, o := range outcomes {
		k := o.Session + "\x00" + o.Target
		idx[k] = append(idx[k], o)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.db.Query(`SELECT sit,session,target,stage,ts FROM fired WHERE resolved=0 AND ts<=?`, now.Add(-s.Settle).Unix())
	if err != nil {
		return 0, err
	}
	type open struct {
		sit, session, target, stage string
		ts                          int64
	}
	var opens []open
	for rs.Next() {
		var o open
		if rs.Scan(&o.sit, &o.session, &o.target, &o.stage, &o.ts) == nil {
			opens = append(opens, o)
		}
	}
	rs.Close()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, o := range opens {
		best, found := "", false
		for _, c := range idx[o.session+"\x00"+o.target] {
			if c.Stage == o.stage && abs64(c.TS-o.ts) <= 5 {
				best, found = c.Outcome, true
				break
			}
		}
		if !found {
			if now.Unix()-o.ts < int64(firedKeep/time.Second)/2 {
				continue // the outcome may still arrive
			}
			best = "ignored"
		}
		w, ok := OutcomeWeight[best]
		if !ok {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO expect(sit,target,weight,outcome,ts) VALUES(?,?,?,?,?)
			ON CONFLICT(sit,target) DO UPDATE SET
				weight=MAX(weight, excluded.weight),
				outcome=CASE WHEN excluded.weight >= weight THEN excluded.outcome ELSE outcome END,
				ts=excluded.ts`, o.sit, o.target, w, best, o.ts); err != nil {
			return n, err
		}
		if _, err := tx.Exec(`UPDATE fired SET resolved=1 WHERE sit=? AND session=? AND target=? AND ts=?`, o.sit, o.session, o.target, o.ts); err != nil {
			return n, err
		}
		n++
	}
	return n, tx.Commit()
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// Seed is one case of a seed suite.
type Seed struct {
	Text   string
	Stage  string
	MinRel float64
	Limit  int
	Budget int
	// Expect are memories that must fire; Avoid ones that must not.
	Expect []string
	Avoid  []string
}

// AddSeeds stores seed situations (idempotent: the same text and stage is
// updated, not duplicated). It returns how many were new.
func (s *Store) AddSeeds(seeds []Seed, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	added := 0
	for _, sd := range seeds {
		text := Clean(sd.Text)
		if text == "" || (len(sd.Expect) == 0 && len(sd.Avoid) == 0) {
			continue
		}
		id := ID(text, sd.Stage)
		avoid, _ := json.Marshal(sd.Avoid)
		res, err := tx.Exec(`INSERT INTO situations(id,text,stage,min_rel,lim,bud,source,n,first_ts,last_ts,avoid)
			VALUES(?,?,?,?,?,?,'seed',1,?,?,?) ON CONFLICT(id) DO NOTHING`,
			id, text, sd.Stage, sd.MinRel, sd.Limit, sd.Budget, now.Unix(), now.Unix(), string(avoid))
		if err != nil {
			return added, err
		}
		if c, _ := res.RowsAffected(); c > 0 {
			added++
		}
		for _, t := range sd.Expect {
			if _, err := tx.Exec(`INSERT INTO expect(sit,target,weight,outcome,ts) VALUES(?,?,1,'seed',?)
				ON CONFLICT(sit,target) DO UPDATE SET weight=MAX(weight,1), outcome='seed'`, id, t, now.Unix()); err != nil {
				return added, err
			}
		}
	}
	s.pruneTx(tx, now)
	return added, tx.Commit()
}

// Remap carries expectations from one memory to its replacement once a merge
// or supersession has really happened, so later replays keep counting it.
func (s *Store) Remap(old, nw string) error {
	if old == "" || nw == "" || old == nw {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO expect(sit,target,weight,outcome,ts)
		SELECT sit,?,weight,outcome,ts FROM expect WHERE target=? ON CONFLICT(sit,target) DO UPDATE SET
			weight=MAX(expect.weight, excluded.weight)`, nw, old); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM expect WHERE target=?`, old); err != nil {
		return err
	}
	tx.Exec(`UPDATE fired SET target=? WHERE target=?`, nw, old)
	// Seed avoid lists name targets as JSON; rewrite the quoted occurrence.
	tx.Exec(`UPDATE situations SET avoid=REPLACE(avoid, ?, ?) WHERE avoid LIKE ?`,
		`"`+old+`"`, `"`+nw+`"`, `%"`+old+`"%`)
	return tx.Commit()
}

// Embedder embeds text; Signature identifies the model so cached situation
// vectors are dropped when it changes.
type Embedder interface {
	Embed(texts []string) [][]float32
	Signature() string
}

// Situations loads the corpus. Missing vectors are embedded (once; they are
// cached with the embedder's signature). kind filters by source ("" = both).
func (s *Store) Situations(emb Embedder, source string) ([]Situation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := `SELECT id,text,stage,min_rel,lim,bud,source,n,first_ts,last_ts,avoid,vec,vec_sig FROM situations`
	var args []any
	if source != "" {
		q += ` WHERE source=?`
		args = append(args, source)
	}
	q += ` ORDER BY last_ts DESC, id`
	rs, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	var out []Situation
	sig := ""
	if emb != nil {
		sig = emb.Signature()
	}
	var need []int
	for rs.Next() {
		var sit Situation
		var avoid, vsig string
		var blob []byte
		if err := rs.Scan(&sit.ID, &sit.Text, &sit.Stage, &sit.MinRel, &sit.Limit, &sit.Budget, &sit.Source, &sit.N,
			&sit.First, &sit.Last, &avoid, &blob, &vsig); err != nil {
			rs.Close()
			return nil, err
		}
		if avoid != "" {
			json.Unmarshal([]byte(avoid), &sit.Avoid)
		}
		if len(blob) > 0 && vsig == sig {
			sit.Vec = unpack(blob)
		} else if emb != nil {
			need = append(need, len(out))
		}
		sit.Expect = map[string]float64{}
		out = append(out, sit)
	}
	rs.Close()
	byID := map[string]int{}
	for i := range out {
		byID[out[i].ID] = i
	}
	er, err := s.db.Query(`SELECT sit,target,weight FROM expect`)
	if err != nil {
		return nil, err
	}
	for er.Next() {
		var id, t string
		var w float64
		if er.Scan(&id, &t, &w) == nil {
			if i, ok := byID[id]; ok {
				out[i].Expect[t] = w
			}
		}
	}
	er.Close()
	if len(need) > 0 && emb != nil {
		for lo := 0; lo < len(need); lo += 64 {
			hi := lo + 64
			if hi > len(need) {
				hi = len(need)
			}
			texts := make([]string, 0, hi-lo)
			for _, i := range need[lo:hi] {
				texts = append(texts, out[i].Text)
			}
			vecs := emb.Embed(texts)
			for k, i := range need[lo:hi] {
				if k < len(vecs) && len(vecs[k]) > 0 {
					out[i].Vec = vecs[k]
					s.db.Exec(`UPDATE situations SET vec=?, vec_sig=? WHERE id=?`, pack(vecs[k]), sig, out[i].ID)
				}
			}
		}
	}
	return out, nil
}

// Stats summarises the corpus.
type Stats struct {
	Live         int   `json:"live"`         // live situations
	Seed         int   `json:"seed"`         // seed situations
	WithUseful   int   `json:"with_useful"`  // situations with a useful expected memory
	Unresolved   int   `json:"unresolved"`   // firings waiting for an outcome
	Expectations int   `json:"expectations"` // remembered (situation, memory) pairs
	Oldest       int64 `json:"oldest"`       // unix seconds
	Newest       int64 `json:"newest"`
}

// Stats counts the corpus.
func (s *Store) Stats() (Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st Stats
	s.db.QueryRow(`SELECT COUNT(*) FROM situations WHERE source='live'`).Scan(&st.Live)
	s.db.QueryRow(`SELECT COUNT(*) FROM situations WHERE source='seed'`).Scan(&st.Seed)
	s.db.QueryRow(`SELECT COUNT(DISTINCT sit) FROM expect WHERE weight>=?`, UsefulWeight).Scan(&st.WithUseful)
	s.db.QueryRow(`SELECT COUNT(*) FROM fired WHERE resolved=0`).Scan(&st.Unresolved)
	s.db.QueryRow(`SELECT COUNT(*) FROM expect`).Scan(&st.Expectations)
	var lo, hi sql.NullInt64
	s.db.QueryRow(`SELECT MIN(last_ts), MAX(last_ts) FROM situations`).Scan(&lo, &hi)
	st.Oldest, st.Newest = lo.Int64, hi.Int64
	return st, nil
}

func pack(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func unpack(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}
