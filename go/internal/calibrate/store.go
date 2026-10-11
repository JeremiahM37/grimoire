package calibrate

import (
	"database/sql"
	"errors"
	"sort"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

// Store is the persistent side: two counter tables in the index database.
// They are derived telemetry in the same sense as memory_entries.uses. A full
// reindex does not rebuild them from the markdown, because the events that
// fill them (a challenge, a settled dispute) are not recorded in the markdown
// as counts. Losing them loses the learned trust, not any fact.
//
// Every write is a single statement and runs on the request that caused the
// event. There are no background goroutines.
type Store struct {
	db *db.DB
}

// Open creates the counter tables if they are missing and returns a Store.
// It is idempotent, so callers open one wherever they need it.
func Open(d *db.DB) (*Store, error) {
	if d == nil {
		return nil, errors.New("calibrate: nil database")
	}
	if err := d.Exec(`CREATE TABLE IF NOT EXISTS calibrate_agents(
		agent TEXT PRIMARY KEY,
		confirmed INTEGER NOT NULL DEFAULT 0,
		challenged INTEGER NOT NULL DEFAULT 0,
		superseded_human INTEGER NOT NULL DEFAULT 0,
		deleted INTEGER NOT NULL DEFAULT 0)`); err != nil {
		return nil, err
	}
	if err := d.Exec(`CREATE TABLE IF NOT EXISTS calibrate_facts(
		note TEXT NOT NULL, id TEXT NOT NULL,
		recalled INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY(note, id))`); err != nil {
		return nil, err
	}
	return &Store{db: d}, nil
}

// Bump records one event against an agent. An empty agent is a no-op: a fact
// nothing wrote has nobody to be scored.
func (s *Store) Bump(agent string, ev Event) error {
	if agent == "" {
		return nil
	}
	var c Counts
	c.Add(ev)
	return s.db.Exec(`INSERT INTO calibrate_agents(agent,confirmed,challenged,superseded_human,deleted)
		VALUES(?,?,?,?,?)
		ON CONFLICT(agent) DO UPDATE SET
		confirmed=confirmed+excluded.confirmed,
		challenged=challenged+excluded.challenged,
		superseded_human=superseded_human+excluded.superseded_human,
		deleted=deleted+excluded.deleted`,
		agent, c.Confirmed, c.Challenged, c.SupersededByHuman, c.Deleted)
}

// Counts reads one agent's accumulated events. An unknown agent has zero.
func (s *Store) Counts(agent string) (Counts, error) {
	var c Counts
	err := s.db.QueryRow(`SELECT confirmed,challenged,superseded_human,deleted
		FROM calibrate_agents WHERE agent=?`, agent).
		Scan(&c.Confirmed, &c.Challenged, &c.SupersededByHuman, &c.Deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return Counts{}, nil
	}
	return c, err
}

// AgentRow is one agent's record, for listings.
type AgentRow struct {
	Agent       string  `json:"agent"`
	Counts      Counts  `json:"-"`
	Confirmed   int     `json:"confirmed"`
	Challenged  int     `json:"challenged"`
	Superseded  int     `json:"superseded_by_human"`
	Deleted     int     `json:"deleted"`
	Mean        float64 `json:"mean"`
	Reliability float64 `json:"reliability"`
}

// Agents lists every agent with at least one event, lowest reliability first,
// so the agents most in need of review come first.
func (s *Store) Agents() ([]AgentRow, error) {
	rows, err := s.db.Query(`SELECT agent,confirmed,challenged,superseded_human,deleted
		FROM calibrate_agents`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentRow
	for rows.Next() {
		var r AgentRow
		if err := rows.Scan(&r.Agent, &r.Counts.Confirmed, &r.Counts.Challenged,
			&r.Counts.SupersededByHuman, &r.Counts.Deleted); err != nil {
			return nil, err
		}
		out = append(out, r.with())
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Reliability != out[j].Reliability {
			return out[i].Reliability < out[j].Reliability
		}
		return out[i].Agent < out[j].Agent
	})
	return out, nil
}

// with fills the derived columns of a row from its counts.
func (r AgentRow) with() AgentRow {
	r.Confirmed = r.Counts.Confirmed
	r.Challenged = r.Counts.Challenged
	r.Superseded = r.Counts.SupersededByHuman
	r.Deleted = r.Counts.Deleted
	r.Mean = Update(r.Counts.Good(), r.Counts.Bad()).Mean()
	r.Reliability = Reliability(r.Counts)
	return r
}

// Reliability is the persisted conservative reliability of one agent.
func (s *Store) Reliability(agent string) (float64, error) {
	c, err := s.Counts(agent)
	if err != nil {
		return 0, err
	}
	return Reliability(c), nil
}

// NoteRecalled counts one recall of each fact. It is the only write on the
// read path, and it is one statement per fact returned.
func (s *Store) NoteRecalled(note string, ids ...string) error {
	for _, id := range ids {
		if id == "" {
			continue
		}
		if err := s.db.Exec(`INSERT INTO calibrate_facts(note,id,recalled) VALUES(?,?,1)
			ON CONFLICT(note,id) DO UPDATE SET recalled=recalled+1`, note, id); err != nil {
			return err
		}
	}
	return nil
}

// Recalled reads how many times a fact has been recalled. Zero if never.
func (s *Store) Recalled(note, id string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT recalled FROM calibrate_facts WHERE note=? AND id=?`,
		note, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}
