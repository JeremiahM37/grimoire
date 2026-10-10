package adherence

import (
	"database/sql"
	"time"
)

// UsedMin is how many of a memory's fingerprints must be matched for it to
// count as used. One: a fingerprint is already a rare token of that memory.
// docs/MEMORY_ADHERENCE.md reports what two would cost in recall.
const UsedMin = 1

// migrateFingerprints adds the fingerprint columns to an existing database.
// fp_n is -1 on rows from before fingerprints, which keeps their old
// classification (ignored, not unknown).
func migrateFingerprints(db *sql.DB) error {
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
	for col, ddl := range map[string]string{
		"fp_n":   `ALTER TABLE injections ADD COLUMN fp_n INTEGER NOT NULL DEFAULT -1`,
		"fp_hit": `ALTER TABLE injections ADD COLUMN fp_hit INTEGER NOT NULL DEFAULT 0`,
	} {
		if !have[col] {
			if _, err := db.Exec(ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

// addFingerprint folds one finished row into the coverage counters.
func (s *Stats) addFingerprint(r Row) {
	if r.FPN < 0 {
		return
	}
	s.FPKnown++
	if r.FPN == 0 {
		return
	}
	s.FPCovered++
	if r.Finalized {
		s.FPDone++
		if r.FPHit >= UsedMin {
			s.FPHit++
		}
	}
}

// Coverage is the share of injections (with fingerprint data) that had at
// least one fingerprint.
func (s Stats) Coverage() float64 {
	if s.FPKnown == 0 {
		return 0
	}
	return float64(s.FPCovered) / float64(s.FPKnown)
}

// UsedRate is the share of finished, fingerprintable injections whose
// fingerprint appeared in what the agent did or said.
func (s Stats) UsedRate() float64 {
	if s.FPDone == 0 {
		return 0
	}
	return float64(s.FPHit) / float64(s.FPDone)
}

// MarkFingerprints records how many fingerprints the client matched for each
// tag, on the session's newest injection of that tag. Counts only grow. A row
// already finalised is still updated (the use window runs past the turn that
// finalised it), and is returned so the caller can credit the late upgrade;
// rows that were not yet final are settled by Finalize as usual.
func (s *Store) MarkFingerprints(session string, counts map[string]int, since time.Time) (late []Row, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tag, n := range counts {
		rs, qerr := s.db.Query(`SELECT `+rowCols+` FROM injections WHERE session=? AND tag=? AND ts>=? ORDER BY id DESC LIMIT 1`,
			session, tag, since.Unix())
		if qerr != nil {
			return late, qerr
		}
		rows, qerr := scanRows(rs)
		if qerr != nil {
			return late, qerr
		}
		if len(rows) == 0 || rows[0].FPN <= 0 || n <= rows[0].FPHit {
			continue
		}
		if n > rows[0].FPN {
			n = rows[0].FPN // a client cannot match more than were sent
		}
		before := rows[0].Outcome()
		if _, err := s.db.Exec(`UPDATE injections SET fp_hit=? WHERE id=?`, n, rows[0].ID); err != nil {
			return late, err
		}
		rows[0].FPHit = n
		if rows[0].Finalized && before != rows[0].Outcome() {
			late = append(late, rows[0])
		}
	}
	return late, nil
}
