package adherence

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"time"
)

// Exposure logging for the offline ranking learner (internal/rankfit).
//
// Each eligible memory in a response is one slate item. It records the slate it
// was ranked in, its 1-based position in that slate as displayed, whether the
// slate's order was randomised for exploration, its feature vector, and, for a
// shown item, the holdout probability it faced (p_withhold, already logged).
// The columns are additive with defaults that mean "not logged", so rows
// written with the ips experiment off are unchanged in meaning.

// ipsColumns are added to both the injection log and the withheld log on open.
var ipsColumns = map[string]map[string]string{
	"injections": {
		"pos":      `ALTER TABLE injections ADD COLUMN pos INTEGER NOT NULL DEFAULT 0`,
		"slate":    `ALTER TABLE injections ADD COLUMN slate TEXT NOT NULL DEFAULT ''`,
		"explored": `ALTER TABLE injections ADD COLUMN explored INTEGER NOT NULL DEFAULT 0`,
		"feats":    `ALTER TABLE injections ADD COLUMN feats TEXT NOT NULL DEFAULT ''`,
	},
	"trace_withheld": {
		"pos":      `ALTER TABLE trace_withheld ADD COLUMN pos INTEGER NOT NULL DEFAULT 0`,
		"slate":    `ALTER TABLE trace_withheld ADD COLUMN slate TEXT NOT NULL DEFAULT ''`,
		"explored": `ALTER TABLE trace_withheld ADD COLUMN explored INTEGER NOT NULL DEFAULT 0`,
		"feats":    `ALTER TABLE trace_withheld ADD COLUMN feats TEXT NOT NULL DEFAULT ''`,
	},
}

// migrateIPS adds the exposure columns that are missing. It is idempotent.
func migrateIPS(db *sql.DB) error {
	for table, cols := range ipsColumns {
		have, err := columnSet(db, table)
		if err != nil {
			return err
		}
		for col, ddl := range cols {
			if have[col] {
				continue
			}
			if _, err := db.Exec(ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

func columnSet(db *sql.DB, table string) (map[string]bool, error) {
	rs, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	have := map[string]bool{}
	for rs.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rs.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		have[name] = true
	}
	return have, rs.Err()
}

// ExposureRow is one logged slate item as the learner reads it. Shown is false
// for a withheld item, which has no outcome.
type ExposureRow struct {
	Table     string // "injections" (shown) or "trace_withheld" (withheld)
	Session   string
	Slate     string
	Pos       int
	Explored  bool
	Shown     bool
	PWithhold float64
	Used      bool // the agent cited the memory (shown rows only)
	Feats     string
	TS        int64
}

// ExposureCounts summarises what the logs hold, for the learner's report.
type ExposureCounts struct {
	Injections, Withheld int // total rows
	Positioned           int // rows with a logged position
	WithFeatures         int // rows with a feature vector
	Slates               int // distinct slate ids among positioned rows
	Explored             int // rows from explored slates
	Cited                int // shown rows whose memory was cited
	Note                 string
}

// OpenReadOnly opens the adherence database in dir without writing to it: no
// schema creation, no migration, no pruning. A missing file is an error, not an
// empty log.
func OpenReadOnly(dir string) (*sql.DB, error) {
	path := filepath.Join(dir, "adherence.db")
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ReadExposures returns every logged exposure since the cutoff, oldest first.
// Columns that predate the ips experiment read as their defaults, so a log
// written before it yields rows with Pos 0 and no features.
func ReadExposures(db *sql.DB, since time.Time) ([]ExposureRow, ExposureCounts, error) {
	var c ExposureCounts
	cols, err := columnSet(db, "injections")
	if err != nil {
		return nil, c, err
	}
	if !cols["pos"] {
		c.Note = "positions were not logged yet (the ips experiment has not run)"
	}
	var rows []ExposureRow
	q := func(table string, shown bool) error {
		have, err := columnSet(db, table)
		if err != nil {
			return err
		}
		sel := "0, '', 0, ''"
		if have["pos"] {
			sel = "pos, slate, explored, feats"
		}
		cited := "0"
		if shown {
			cited = "cited"
		}
		rs, err := db.Query(fmt.Sprintf(`SELECT session, %s, %s, p_withhold, ts FROM %s WHERE ts>=? ORDER BY ts, rowid`,
			sel, cited, table), since.Unix())
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r ExposureRow
			var explored, citedV int
			var pos sql.NullInt64
			var slate sql.NullString
			var feats sql.NullString
			if err := rs.Scan(&r.Session, &pos, &slate, &explored, &feats, &citedV, &r.PWithhold, &r.TS); err != nil {
				return err
			}
			r.Table, r.Shown = table, shown
			r.Slate, r.Feats = slate.String, feats.String
			r.Pos, r.Explored, r.Used = int(pos.Int64), explored == 1, citedV == 1
			rows = append(rows, r)
		}
		return rs.Err()
	}
	if err := q("injections", true); err != nil {
		return nil, c, err
	}
	if err := q("trace_withheld", false); err != nil {
		return nil, c, err
	}
	slates := map[string]bool{}
	for _, r := range rows {
		if r.Shown {
			c.Injections++
		} else {
			c.Withheld++
		}
		if r.Pos > 0 {
			c.Positioned++
			slates[r.Slate] = true
		}
		if r.Feats != "" {
			c.WithFeatures++
		}
		if r.Explored && r.Pos > 0 {
			c.Explored++
		}
		if r.Shown && r.Used {
			c.Cited++
		}
	}
	c.Slates = len(slates)
	return rows, c, nil
}
