package adherence

// The utilization trace (docs/MEMORY_TRACE.md): for every injection, the chain
// exposure -> uptake -> influence (linked to a concrete action) -> outcome ->
// benefit. It lives in the adherence database, is derived telemetry, keeps no
// text (hashes, ids and small codes only) and is bounded like the injection log.
//
//	injections        one row per memory shown (exposure) with its uptake evidence
//	                  (cited, fp_hit, meaning) and, at the action stage, the pending
//	                  action it was shown for (pend) and what became of it (rem)
//	trace_actions     one row per executed tool call, with outcome codes
//	trace_links       injection -> action, with the evidence that linked them
//	trace_withheld    holdout: the item would have been injected and was not
//	trace_sessions    prompt boundaries, to attach a user correction to the
//	                  actions of the turn it corrects

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"time"
)

// Caps on the trace tables, whatever the retention says.
const (
	maxActions  = 300000
	maxLinks    = 600000
	maxWithheld = 200000
)

// Windows. A memory's uptake evidence links to actions this long after the
// exposure; a reminder for a pending action waits this long for the call to
// show up (a person may be deciding an ask).
const (
	LinkWindow     = 2 * time.Hour
	ReminderWindow = 30 * time.Minute
	// sequenceWindow bounds matching a reminder to the next call when the
	// agent supplies no tool_use id.
	sequenceWindow = 120 * time.Second
)

// Link evidence kinds.
const (
	EvFingerprint = "fp"      // a fingerprint of the memory appeared in the action
	EvTag         = "tag"     // the action carries the memory's tag
	EvCheck       = "check"   // the memory's require check was met by the action
	EvChanged     = "changed" // the pending action was changed after the reminder
)

// Reminder resolutions (injections.rem).
const (
	RemUnchanged = "unchanged" // the pending action ran as it was
	RemChanged   = "changed"   // the next executed action differed
	RemAbandoned = "abandoned" // the pending action never ran
)

var traceSchema = []string{
	`CREATE TABLE IF NOT EXISTS trace_actions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session TEXT NOT NULL, seq INTEGER NOT NULL, tu TEXT NOT NULL DEFAULT '',
		tool TEXT NOT NULL, target_hash TEXT NOT NULL, region TEXT NOT NULL DEFAULT '',
		ts INTEGER NOT NULL, stage TEXT NOT NULL DEFAULT 'early',
		failed INTEGER NOT NULL DEFAULT -1, exit_code INTEGER,
		tests_pass INTEGER NOT NULL DEFAULT -1, tests_fail INTEGER NOT NULL DEFAULT -1,
		reedit INTEGER NOT NULL DEFAULT 0, revert INTEGER NOT NULL DEFAULT 0,
		thrash INTEGER NOT NULL DEFAULT 0, denied INTEGER NOT NULL DEFAULT 0,
		correction INTEGER NOT NULL DEFAULT 0)`,
	`CREATE INDEX IF NOT EXISTS trace_actions_session ON trace_actions(session, seq)`,
	`CREATE INDEX IF NOT EXISTS trace_actions_region ON trace_actions(session, region)`,
	`CREATE INDEX IF NOT EXISTS trace_actions_ts ON trace_actions(ts)`,
	`CREATE TABLE IF NOT EXISTS trace_links (
		injection_id INTEGER NOT NULL, action_id INTEGER NOT NULL,
		evidence TEXT NOT NULL, ts INTEGER NOT NULL,
		PRIMARY KEY (injection_id, action_id, evidence))`,
	`CREATE INDEX IF NOT EXISTS trace_links_action ON trace_links(action_id)`,
	`CREATE TABLE IF NOT EXISTS trace_withheld (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session TEXT NOT NULL, tag TEXT NOT NULL, key TEXT NOT NULL, target TEXT NOT NULL,
		kind TEXT NOT NULL DEFAULT '', stage TEXT NOT NULL DEFAULT '', tool TEXT NOT NULL DEFAULT '',
		relevance REAL NOT NULL DEFAULT 0, p_withhold REAL NOT NULL DEFAULT 0, ts INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS trace_withheld_target ON trace_withheld(target, ts)`,
	`CREATE TABLE IF NOT EXISTS trace_sessions (
		session TEXT PRIMARY KEY, prev_ts INTEGER NOT NULL DEFAULT 0, last_ts INTEGER NOT NULL DEFAULT 0)`,
}

// traceColumns are added to injections on open.
var traceColumns = map[string]string{
	"kind":       `ALTER TABLE injections ADD COLUMN kind TEXT NOT NULL DEFAULT ''`,
	"p_withhold": `ALTER TABLE injections ADD COLUMN p_withhold REAL NOT NULL DEFAULT 0`,
	"tool":       `ALTER TABLE injections ADD COLUMN tool TEXT NOT NULL DEFAULT ''`,
	"pend":       `ALTER TABLE injections ADD COLUMN pend TEXT NOT NULL DEFAULT ''`,
	"tu":         `ALTER TABLE injections ADD COLUMN tu TEXT NOT NULL DEFAULT ''`,
	"ask":        `ALTER TABLE injections ADD COLUMN ask INTEGER NOT NULL DEFAULT 0`,
	"rem":        `ALTER TABLE injections ADD COLUMN rem TEXT NOT NULL DEFAULT ''`,
	"meaning":    `ALTER TABLE injections ADD COLUMN meaning REAL NOT NULL DEFAULT -1`,
}

func migrateTrace(db *sql.DB) error {
	for _, stmt := range traceSchema {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	have := map[string]bool{}
	rs, err := db.Query(`PRAGMA table_info(injections)`)
	if err != nil {
		return err
	}
	for rs.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rs.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rs.Close()
			return err
		}
		have[name] = true
	}
	rs.Close()
	for col, ddl := range traceColumns {
		if !have[col] {
			if _, err := db.Exec(ddl); err != nil {
				return err
			}
		}
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS injections_pend ON injections(session, rem)`)
	return err
}

// PendingHash identifies an action without keeping it: sha256 of the tool and
// the target with surrounding space trimmed, 16 hex characters. A delegation
// target from the context hook ("launch subagent ...") is normalised to what
// the outcome hook reports.
func PendingHash(tool, target string) string {
	target = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(target), "launch subagent "))
	sum := sha256.Sum256([]byte(tool + "\x00" + target))
	return hex.EncodeToString(sum[:8])
}

// StageOf buckets an action's position in its session so influenced and
// comparison actions are matched on how far into the work they are.
func StageOf(seq int) string {
	switch {
	case seq <= 10:
		return "early"
	case seq <= 40:
		return "mid"
	}
	return "late"
}

// ActionIn is one executed tool call as the outcome hook reports it. Outcome
// fields use -1 for "not observed".
type ActionIn struct {
	Session, TU, Tool, Target string
	Region                    string   // id of the content this call produced (edits)
	Failed                    int      // -1 unknown, 0 ok, 1 tool error or non-zero exit
	ExitCode                  *int     // when the agent reports one
	TestsPass, TestsFail      int      // -1 unknown
	Thrash                    int      // times this command has now run in the session
	Denied                    bool     // the user or harness refused it
	Reedit, Revert            []string // regions of EARLIER calls this call edited again / undid
	Ev                        map[string]int
	Cited                     []string // tags the call itself carries
	TS                        time.Time
}

// RecordAction stores an executed call and does the three things that need it:
// resolve reminders for pending actions (the natural experiment), mark earlier
// edits that this call re-edited or reverted, and link the memories whose
// uptake evidence the call carries. It returns the action id and the injection
// rows linked by evidence (the caller adds check links).
func (s *Store) RecordAction(a ActionIn) (int64, []Row, error) {
	if a.TS.IsZero() {
		a.TS = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()

	hash := PendingHash(a.Tool, a.Target)
	var id int64
	if a.TU != "" {
		_ = tx.QueryRow(`SELECT id FROM trace_actions WHERE session=? AND tu=?`, a.Session, a.TU).Scan(&id)
	}
	var exit any
	if a.ExitCode != nil {
		exit = *a.ExitCode
	}
	if id == 0 {
		var seq int
		_ = tx.QueryRow(`SELECT COALESCE(MAX(seq),0)+1 FROM trace_actions WHERE session=?`, a.Session).Scan(&seq)
		res, err := tx.Exec(`INSERT INTO trace_actions(session,seq,tu,tool,target_hash,region,ts,stage,failed,exit_code,tests_pass,tests_fail,thrash,denied)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.Session, seq, a.TU, a.Tool, hash, a.Region, a.TS.Unix(), StageOf(seq),
			a.Failed, exit, a.TestsPass, a.TestsFail, a.Thrash, b2i(a.Denied))
		if err != nil {
			return 0, nil, err
		}
		id, _ = res.LastInsertId()
	} else {
		// A repeated report of the same call fills in what the first lacked.
		if _, err := tx.Exec(`UPDATE trace_actions SET failed=MAX(failed,?), tests_pass=MAX(tests_pass,?), tests_fail=MAX(tests_fail,?),
			thrash=MAX(thrash,?), denied=MAX(denied,?) WHERE id=?`, a.Failed, a.TestsPass, a.TestsFail, a.Thrash, b2i(a.Denied), id); err != nil {
			return 0, nil, err
		}
	}

	for kind, regions := range map[string][]string{"reedit": a.Reedit, "revert": a.Revert} {
		for _, r := range regions {
			if r == "" {
				continue
			}
			if _, err := tx.Exec(`UPDATE trace_actions SET `+kind+`=1 WHERE session=? AND region=? AND id<>?`, a.Session, r, id); err != nil {
				return 0, nil, err
			}
		}
	}

	if err := resolveReminders(tx, a, hash, id); err != nil {
		return 0, nil, err
	}

	// Link the memories whose uptake evidence this call carries: a fingerprint
	// of the memory in the call, or the memory's tag.
	var linked []Row
	seen := map[int64]bool{}
	link := func(tag, ev string) error {
		rs, err := tx.Query(`SELECT `+rowCols+` FROM injections WHERE session=? AND tag=? AND ts>=? AND ts<=? ORDER BY id DESC LIMIT 1`,
			a.Session, tag, a.TS.Add(-LinkWindow).Unix(), a.TS.Unix())
		if err != nil {
			return err
		}
		rows, err := scanRows(rs)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO trace_links(injection_id,action_id,evidence,ts) VALUES(?,?,?,?)`,
				r.ID, id, ev, a.TS.Unix()); err != nil {
				return err
			}
			if !seen[r.ID] {
				seen[r.ID] = true
				linked = append(linked, r)
			}
		}
		return nil
	}
	for tag, n := range a.Ev {
		if n > 0 {
			if err := link(tag, EvFingerprint); err != nil {
				return 0, nil, err
			}
		}
	}
	for _, tag := range a.Cited {
		if err := link(tag, EvTag); err != nil {
			return 0, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	return id, linked, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// resolveReminders settles the pending-action reminders of the session against
// an executed call. With tool_use ids the match is exact; without them the
// call that follows (within sequenceWindow) is the one: the same hash is
// "unchanged", anything else "changed". A hash that equals any pending
// reminder's resolves that one, so parallel calls do not read as changes.
func resolveReminders(tx *sql.Tx, a ActionIn, hash string, actionID int64) error {
	rs, err := tx.Query(`SELECT id, tool, tu, pend, ask, ts FROM injections
		WHERE session=? AND rem='' AND pend<>'' AND stage='action' AND ts>=? AND ts<=? ORDER BY id`,
		a.Session, a.TS.Add(-ReminderWindow).Unix(), a.TS.Unix())
	if err != nil {
		return err
	}
	type rem struct {
		id       int64
		tool, tu string
		pend     string
		ask      int
		ts       int64
	}
	var all []rem
	for rs.Next() {
		var r rem
		if err := rs.Scan(&r.id, &r.tool, &r.tu, &r.pend, &r.ask, &r.ts); err != nil {
			rs.Close()
			return err
		}
		all = append(all, r)
	}
	rs.Close()
	if len(all) == 0 {
		return nil
	}
	set := func(r rem, res string) error {
		if _, err := tx.Exec(`UPDATE injections SET rem=? WHERE id=?`, res, r.id); err != nil {
			return err
		}
		if res == RemChanged {
			_, err := tx.Exec(`INSERT OR IGNORE INTO trace_links(injection_id,action_id,evidence,ts) VALUES(?,?,?,?)`,
				r.id, actionID, EvChanged, a.TS.Unix())
			return err
		}
		return nil
	}
	if a.TU != "" {
		for _, r := range all {
			if r.tu != a.TU {
				continue
			}
			res := RemChanged
			if r.pend == hash {
				res = RemUnchanged
			}
			if err := set(r, res); err != nil {
				return err
			}
		}
	}
	// Reminders that name no tool_use id (or a call that names none): match by
	// sequence.
	var seq []rem
	for _, r := range all {
		if a.TU != "" && r.tu != "" {
			continue // exact matching above; a different id is a different call
		}
		age := time.Duration(a.TS.Unix()-r.ts) * time.Second
		if r.ask == 0 && age > sequenceWindow {
			continue
		}
		seq = append(seq, r)
	}
	matched := false
	for _, r := range seq {
		if r.pend == hash {
			matched = true
			if err := set(r, RemUnchanged); err != nil {
				return err
			}
		}
	}
	if matched || len(seq) == 0 {
		return nil
	}
	oldest := seq[0]
	for _, r := range seq {
		if r.pend == oldest.pend && r.tu == oldest.tu {
			if err := set(r, RemChanged); err != nil {
				return err
			}
		}
	}
	return nil
}

// FinishReminders closes the session's unresolved reminders at the end of a
// turn: the pending action never ran.
func (s *Store) FinishReminders(session string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE injections SET rem=? WHERE session=? AND rem='' AND pend<>'' AND stage='action'`, RemAbandoned, session)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Link records evidence that an injection influenced an action.
func (s *Store) Link(injectionID, actionID int64, evidence string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT OR IGNORE INTO trace_links(injection_id,action_id,evidence,ts) VALUES(?,?,?,?)`,
		injectionID, actionID, evidence, now.Unix())
	return err
}

// SetMeaning records a meaning-match score (0..1) for an injection of the
// session with this tag, from a client or judge that compared the memory with
// what the agent did. Only the larger score is kept.
func (s *Store) SetMeaning(session, tag string, score float64, since time.Time) error {
	if score < 0 || score > 1 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE injections SET meaning=? WHERE id=(SELECT id FROM injections WHERE session=? AND tag=? AND ts>=? ORDER BY id DESC LIMIT 1) AND meaning<?`,
		score, session, tag, since.Unix(), score)
	return err
}

// PromptMergeWindow is how close two reports of a prompt boundary must be to
// count as one prompt (the hook and the server both see each prompt).
var PromptMergeWindow = 5 * time.Second

// TracePrompt marks a prompt boundary. When the prompt corrects the agent
// (the hook's local classifier or the server's re-tell detection), the actions
// since the previous boundary are marked corrected. Two reports of the same
// prompt (hook and server) land within a few seconds and count once.
func (s *Store) TracePrompt(session string, corrected bool, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var prev, last int64
	err := s.db.QueryRow(`SELECT prev_ts, last_ts FROM trace_sessions WHERE session=?`, session).Scan(&prev, &last)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	if now.Sub(time.Unix(last, 0)) >= PromptMergeWindow {
		prev, last = last, now.Unix()
	}
	if _, err := s.db.Exec(`INSERT INTO trace_sessions(session,prev_ts,last_ts) VALUES(?,?,?)
		ON CONFLICT(session) DO UPDATE SET prev_ts=excluded.prev_ts, last_ts=excluded.last_ts`, session, prev, last); err != nil {
		return 0, err
	}
	if !corrected {
		return 0, nil
	}
	res, err := s.db.Exec(`UPDATE trace_actions SET correction=1 WHERE session=? AND ts>=? AND ts<?`, session, prev, last)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Withheld is a holdout decision: an item that would have been injected.
type Withheld struct {
	Session, Tag, Key, Target, Kind, Stage, Tool string
	Relevance, PWithhold                         float64
}

// LogWithheld records would-have-injected rows.
func (s *Store) LogWithheld(items []Withheld, now time.Time) error {
	if len(items) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, w := range items {
		if _, err := tx.Exec(`INSERT INTO trace_withheld(session,tag,key,target,kind,stage,tool,relevance,p_withhold,ts) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			w.Session, w.Tag, w.Key, w.Target, w.Kind, w.Stage, w.Tool, w.Relevance, w.PWithhold, now.Unix()); err != nil {
			tx.Rollback()
			return err
		}
	}
	if now.Sub(s.lastPrune) > time.Minute {
		s.lastPrune = now
		pruneAll(tx, now)
	}
	return tx.Commit()
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// pruneAll applies the retention and the row caps to the injection log and
// every trace table. Orphaned links go with their injection or action.
func pruneAll(tx execer, now time.Time) {
	cut := now.Add(-Retention).Unix()
	tx.Exec(`DELETE FROM injections WHERE ts < ?`, cut)
	tx.Exec(`DELETE FROM injections WHERE id <= (SELECT COALESCE(MAX(id),0) FROM injections) - ?`, maxRows)
	tx.Exec(`DELETE FROM gate_log WHERE ts < ?`, cut)
	tx.Exec(`DELETE FROM trace_actions WHERE ts < ?`, cut)
	tx.Exec(`DELETE FROM trace_actions WHERE id <= (SELECT COALESCE(MAX(id),0) FROM trace_actions) - ?`, maxActions)
	tx.Exec(`DELETE FROM trace_withheld WHERE ts < ?`, cut)
	tx.Exec(`DELETE FROM trace_withheld WHERE id <= (SELECT COALESCE(MAX(id),0) FROM trace_withheld) - ?`, maxWithheld)
	tx.Exec(`DELETE FROM trace_sessions WHERE last_ts < ?`, cut)
	tx.Exec(`DELETE FROM trace_links WHERE ts < ? OR injection_id NOT IN (SELECT id FROM injections) OR action_id NOT IN (SELECT id FROM trace_actions)`, cut)
	tx.Exec(`DELETE FROM trace_links WHERE rowid <= (SELECT COALESCE(MAX(rowid),0) FROM trace_links) - ?`, maxLinks)
}
