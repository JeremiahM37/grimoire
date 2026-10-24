package connectors

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

// Configured connectors and what they have already pulled.

// Connector is one configured source.
type Connector struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Config Config `json:"config"`
	// Secret names a credential in the vault. The VALUE is never stored here,
	// never returned by the API and never written into a note — the vault is
	// already the one place secrets live, and a connector holding its own copy
	// would quietly become a second one.
	Secret   string `json:"secret"`
	Prefix   string `json:"prefix"`
	Interval int    `json:"interval"`
	Enabled  bool   `json:"enabled"`
	Cursor   string `json:"cursor"`
	LastRun  string `json:"last_run"`
	LastOK   bool   `json:"last_ok"`
	LastErr  string `json:"last_error"`
	Docs     int    `json:"docs"`
	Created  string `json:"created"`
}

// Due reports whether a scheduled run is owed.
func (c Connector) Due(now time.Time) bool {
	if !c.Enabled || c.Interval <= 0 {
		return false
	}
	if c.LastRun == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339, c.LastRun)
	if err != nil {
		return true
	}
	return now.Sub(last) >= time.Duration(c.Interval)*time.Second
}

// Store persists connector configuration and per-document state.
type Store struct{ DB *db.DB }

func NewStore(database *db.DB) *Store { return &Store{DB: database} }

// Schema is applied by the index's own schema; kept here so the shape lives
// beside the code that reads it.
const Schema = `
CREATE TABLE IF NOT EXISTS connectors(
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, name TEXT NOT NULL,
  config TEXT NOT NULL DEFAULT '{}', secret TEXT DEFAULT '',
  prefix TEXT NOT NULL, interval INTEGER DEFAULT 0, enabled INTEGER DEFAULT 1,
  cursor TEXT DEFAULT '', last_run TEXT DEFAULT '', last_ok INTEGER DEFAULT 1,
  last_error TEXT DEFAULT '', docs INTEGER DEFAULT 0, created TEXT
);
-- What each connector has already written, so a re-sync updates a note rather
-- than creating a second copy of the same document under a new name.
CREATE TABLE IF NOT EXISTS connector_docs(
  connector TEXT NOT NULL, external_id TEXT NOT NULL, path TEXT NOT NULL,
  hash TEXT, updated TEXT, PRIMARY KEY(connector, external_id)
);
CREATE INDEX IF NOT EXISTS idx_connector_docs_path ON connector_docs(path);
CREATE TABLE IF NOT EXISTS connector_health(
  connector TEXT PRIMARY KEY, last_success TEXT DEFAULT '', last_failure TEXT DEFAULT '',
  last_error TEXT DEFAULT '', failures INTEGER DEFAULT 0, state TEXT DEFAULT 'ok',
  since TEXT DEFAULT ''
);
`

func (s *Store) Save(c Connector) error {
	cfg, err := json.Marshal(c.Config)
	if err != nil {
		return err
	}
	enabled, lastOK := 0, 0
	if c.Enabled {
		enabled = 1
	}
	if c.LastOK {
		lastOK = 1
	}
	return s.DB.Exec(
		"INSERT OR REPLACE INTO connectors(id,kind,name,config,secret,prefix,interval,"+
			"enabled,cursor,last_run,last_ok,last_error,docs,created) "+
			"VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		c.ID, c.Kind, c.Name, string(cfg), c.Secret, c.Prefix, c.Interval,
		enabled, c.Cursor, c.LastRun, lastOK, c.LastErr, c.Docs, c.Created)
}

func (s *Store) Get(id string) (Connector, error) {
	rows, err := s.DB.Query("SELECT id,kind,name,config,secret,prefix,interval,enabled,"+
		"cursor,last_run,last_ok,last_error,docs,created FROM connectors WHERE id=?", id)
	if err != nil {
		return Connector{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Connector{}, fmt.Errorf("no such connector: %s", id)
	}
	return scanConnector(rows)
}

func (s *Store) List() ([]Connector, error) {
	rows, err := s.DB.Query("SELECT id,kind,name,config,secret,prefix,interval,enabled," +
		"cursor,last_run,last_ok,last_error,docs,created FROM connectors ORDER BY created, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Connector
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Delete(id string) error {
	if err := s.DB.Exec("DELETE FROM connector_docs WHERE connector=?", id); err != nil {
		return err
	}
	if err := s.DB.Exec("DELETE FROM connector_health WHERE connector=?", id); err != nil {
		return err
	}
	return s.DB.Exec("DELETE FROM connectors WHERE id=?", id)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanConnector(rows scanner) (Connector, error) {
	var c Connector
	var cfg string
	var enabled, lastOK int
	if err := rows.Scan(&c.ID, &c.Kind, &c.Name, &cfg, &c.Secret, &c.Prefix, &c.Interval,
		&enabled, &c.Cursor, &c.LastRun, &lastOK, &c.LastErr, &c.Docs, &c.Created); err != nil {
		return Connector{}, err
	}
	c.Enabled, c.LastOK = enabled != 0, lastOK != 0
	c.Config = Config{}
	if strings.TrimSpace(cfg) != "" {
		_ = json.Unmarshal([]byte(cfg), &c.Config)
	}
	return c, nil
}

// docRecord is what a connector has written for one source document.
type docRecord struct {
	Path    string
	Hash    string
	Updated string
}

func (s *Store) docFor(connector, externalID string) (docRecord, bool) {
	var d docRecord
	err := s.DB.QueryRow(
		"SELECT path, hash, updated FROM connector_docs WHERE connector=? AND external_id=?",
		connector, externalID).Scan(&d.Path, &d.Hash, &d.Updated)
	return d, err == nil
}

func (s *Store) putDoc(connector, externalID string, d docRecord) error {
	return s.DB.Exec(
		"INSERT OR REPLACE INTO connector_docs(connector,external_id,path,hash,updated) "+
			"VALUES(?,?,?,?,?)", connector, externalID, d.Path, d.Hash, d.Updated)
}

// docsFor returns everything a connector has written, keyed by external id.
func (s *Store) docsFor(connector string) (map[string]docRecord, error) {
	rows, err := s.DB.Query(
		"SELECT external_id, path, hash, updated FROM connector_docs WHERE connector=?",
		connector)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]docRecord{}
	for rows.Next() {
		var id string
		var d docRecord
		if err := rows.Scan(&id, &d.Path, &d.Hash, &d.Updated); err != nil {
			return nil, err
		}
		out[id] = d
	}
	return out, rows.Err()
}

func (s *Store) forgetDoc(connector, externalID string) error {
	return s.DB.Exec("DELETE FROM connector_docs WHERE connector=? AND external_id=?",
		connector, externalID)
}

// Paths lists every note a connector has written, for cleanup on delete.
func (s *Store) Paths(connector string) ([]string, error) {
	rows, err := s.DB.Query("SELECT path FROM connector_docs WHERE connector=?", connector)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Health returns a connector's health record. A connector that has never run
// has none, which reads as StateOK with no history.
func (s *Store) Health(id string) (Health, error) {
	h := Health{Connector: id, State: StateOK}
	err := s.DB.QueryRow(
		"SELECT last_success, last_failure, last_error, failures, state, since "+
			"FROM connector_health WHERE connector=?", id).
		Scan(&h.LastSuccess, &h.LastFailure, &h.LastError, &h.Failures, &h.State, &h.Since)
	if errors.Is(err, sql.ErrNoRows) {
		// No row is the normal state of a connector that has not run yet.
		return Health{Connector: id, State: StateOK}, nil
	}
	return h, err
}

// AllHealth returns every connector's health record, keyed by connector id.
func (s *Store) AllHealth() (map[string]Health, error) {
	rows, err := s.DB.Query("SELECT connector, last_success, last_failure, last_error, " +
		"failures, state, since FROM connector_health")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Health{}
	for rows.Next() {
		var h Health
		if err := rows.Scan(&h.Connector, &h.LastSuccess, &h.LastFailure, &h.LastError,
			&h.Failures, &h.State, &h.Since); err != nil {
			return nil, err
		}
		out[h.Connector] = h
	}
	return out, rows.Err()
}

// RecordRun applies one run's outcome to a connector's health.
func (s *Store) RecordRun(id string, runErr error, now time.Time) error {
	h, err := s.Health(id)
	if err != nil {
		return err
	}
	return s.putHealth(nextHealth(h, runErr, now))
}

func (s *Store) putHealth(h Health) error {
	return s.DB.Exec(
		"INSERT OR REPLACE INTO connector_health(connector,last_success,last_failure,"+
			"last_error,failures,state,since) VALUES(?,?,?,?,?,?,?)",
		h.Connector, h.LastSuccess, h.LastFailure, h.LastError, h.Failures, h.State, h.Since)
}

// ResetHealth clears a connector's record. Called when its credential is
// replaced, so a reconnected connector is retried on its normal schedule at
// once instead of waiting out the old failure.
func (s *Store) ResetHealth(id string) error {
	return s.DB.Exec("DELETE FROM connector_health WHERE connector=?", id)
}
