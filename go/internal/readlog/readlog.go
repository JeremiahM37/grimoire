// Package readlog records who opened a restricted document.
//
// A permission model answers "may they read this". It cannot answer "who did",
// and on a shared deployment that second question is the one that gets asked —
// after somebody leaves, after a document turns out to have been in the wrong
// space for a month, after a connector mirrors a private channel by mistake.
// Reconstructing it from a reverse proxy's access log does not work: the log
// has a URL and an address, not an account, and it cannot tell an allowed read
// from a denied one.
//
// Two deliberate limits keep this from becoming surveillance of ordinary work:
//
//   - Only RESTRICTED documents are recorded — a note with a reader list, or a
//     note in a space that is not the commons. Reading something everyone can
//     read is not an event. On a single-user deployment nothing is restricted,
//     so nothing is ever written.
//   - Only single-document reads are recorded, not search. A hit list is a
//     record of what somebody was looking for, which is a different and more
//     invasive thing than a record of what they opened. Retrieval already
//     filters to what the caller may see; that filtering is not an access.
//
// Denied reads are recorded too, and are the more interesting half: a person
// walking paths they cannot open looks like nothing at all in a model that
// only logs successes.
package readlog

import (
	"database/sql"
	"strings"
	"sync"
	"time"
)

// Event is one attempt to open one restricted document.
type Event struct {
	At   time.Time
	User string // account id, or "" for an unauthenticated caller
	Name string // account name at the time of the read, for readable output
	// Agent is the software that read, when something could establish it.
	// Deliberately separate from Name: an account is a person and an agent is
	// a program acting for one, and a trail that merged them could not answer
	// "which of my machines read this" — which on a single-user deployment,
	// where there is no account at all, is the only question it can answer.
	Agent   string
	Path    string
	Space   string
	Allowed bool
	Route   string
	Addr    string
}

// Row is a recorded event as it comes back out.
type Row struct {
	ID      int64  `json:"id"`
	At      string `json:"at"`
	User    string `json:"user"`
	Name    string `json:"name"`
	Agent   string `json:"agent,omitempty"`
	Path    string `json:"path"`
	Space   string `json:"space"`
	Allowed bool   `json:"allowed"`
	Route   string `json:"route"`
	Addr    string `json:"addr"`
}

// store is the slice of the index database this package needs. Taking an
// interface rather than *sql.DB keeps the write going through the same lock
// every other writer uses — a raw connection would race the indexer's
// transactions instead of queueing behind them.
type store interface {
	Exec(query string, args ...any) error
	Query(query string, args ...any) (*sql.Rows, error)
	Count(query string, args ...any) (int, error)
}

// Log buffers events and writes them on its own goroutine.
//
// The write must not happen on the request's goroutine. Grimoire runs SQLite
// with a single connection, and a handler that writes while iterating a result
// set waits for a connection the cursor is holding — the deadlock that already
// cost this codebase a hung briefing endpoint. Handing the event to a channel
// makes that structurally impossible: by the time the row is written the
// request's cursors are long closed.
type Log struct {
	db store

	ch   chan Event
	once sync.Once
	stop chan struct{}
	done chan struct{}

	mu         sync.Mutex
	dropped    int64
	suppressed int64
	denials    map[string]*bucket
	windowAt   time.Time
}

// bucket is one actor's allowance of DENIED records in the current window.
type bucket struct{ n int }

// DenialsPerWindow bounds how many denied attempts one actor can write per
// window.
//
// A denial is recorded for any path, including one that does not exist —
// that is the point, since walking paths you cannot open is exactly what the
// trail should show. It is also a way to make this server write rows forever:
// a loop over invented paths costs the caller nothing and costs the disk a row
// each. Past the bound the attempts are counted instead of stored, which keeps
// the SIGNAL (this actor is probing) while dropping the volume.
const (
	DenialsPerWindow = 120
	DenialWindow     = time.Minute
)

// maxField truncates caller-controlled strings. A denied read's path comes
// from the URL, and the URL comes from whoever is asking.
const maxField = 512

// Depth is the buffer. A read is a rare event even on a busy instance; this is
// sized so a burst does not block rather than to absorb sustained load.
const Depth = 4096

// New returns a log writing to db. A nil db yields a log that discards, so a
// caller never has to check.
func New(db store) *Log {
	if db == nil {
		return &Log{}
	}
	return &Log{db: db, ch: make(chan Event, Depth), stop: make(chan struct{}), done: make(chan struct{})}
}

// Start begins draining. Safe to call more than once.
func (l *Log) Start() {
	if l == nil || l.db == nil {
		return
	}
	l.once.Do(func() { go l.run() })
}

// Close drains what is buffered and stops.
func (l *Log) Close() {
	if l == nil || l.db == nil {
		return
	}
	select {
	case <-l.stop:
		return
	default:
	}
	close(l.stop)
	<-l.done
}

func (l *Log) run() {
	defer close(l.done)
	for {
		select {
		case e := <-l.ch:
			l.write(e)
		case <-l.stop:
			// Drain: an event recorded a millisecond before shutdown is
			// exactly the one an investigation will go looking for.
			for {
				select {
				case e := <-l.ch:
					l.write(e)
				default:
					return
				}
			}
		}
	}
}

func (l *Log) write(e Event) {
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	allowed := 0
	if e.Allowed {
		allowed = 1
	}
	_ = l.db.Exec(
		`INSERT INTO read_audit(at, user, name, agent, path, space, allowed, route, addr) VALUES(?,?,?,?,?,?,?,?,?)`,
		at.UTC().Format(time.RFC3339), e.User, e.Name, e.Agent, e.Path, e.Space, allowed, e.Route, e.Addr)
}

// Record queues an event. It never blocks: a full buffer drops the event and
// counts the drop, because a request must not wait on an audit write and a
// silent drop is worse than a counted one.
func (l *Log) Record(e Event) {
	if l == nil || l.db == nil {
		return
	}
	e.Path, e.Route, e.Addr = clip(e.Path), clip(e.Route), clip(e.Addr)
	if !e.Allowed && !l.allowDenial(e) {
		return
	}
	select {
	case l.ch <- e:
	default:
		l.mu.Lock()
		l.dropped++
		l.mu.Unlock()
	}
}

func clip(s string) string {
	if len(s) > maxField {
		return s[:maxField]
	}
	return s
}

// allowDenial reports whether this denied attempt should be stored, resetting
// the allowance once a window has passed.
func (l *Log) allowDenial(e Event) bool {
	who := e.User
	if who == "" {
		who = "addr:" + e.Addr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.denials == nil || time.Since(l.windowAt) > DenialWindow {
		l.denials, l.windowAt = map[string]*bucket{}, time.Now()
	}
	b := l.denials[who]
	if b == nil {
		b = &bucket{}
		l.denials[who] = b
	}
	b.n++
	if b.n > DenialsPerWindow {
		l.suppressed++
		return false
	}
	return true
}

// Suppressed is how many denied attempts were counted rather than stored,
// because one actor produced more than the window allows.
func (l *Log) Suppressed() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.suppressed
}

// Dropped is how many events were lost to a full buffer.
func (l *Log) Dropped() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}

// Flush blocks until the buffer is empty. For tests and for the CLI, which
// exits immediately after a read.
func (l *Log) Flush() {
	if l == nil || l.db == nil {
		return
	}
	for i := 0; i < 2000; i++ {
		if len(l.ch) == 0 {
			// The drain goroutine may still be inside write() for the last
			// event; a short settle is cheaper than a second channel.
			time.Sleep(2 * time.Millisecond)
			if len(l.ch) == 0 {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
}

// Query narrows a listing.
type Query struct {
	Path   string
	User   string
	Denied bool // only denied attempts
	Limit  int
}

// Recent returns matching events, newest first.
func (l *Log) Recent(q Query) ([]Row, error) {
	if l == nil || l.db == nil {
		return nil, nil
	}
	sqlText := `SELECT id, at, user, name, agent, path, space, allowed, route, addr FROM read_audit`
	var where []string
	var args []any
	if p := strings.TrimSpace(q.Path); p != "" {
		where = append(where, "path LIKE ?")
		args = append(args, "%"+p+"%")
	}
	if u := strings.TrimSpace(q.User); u != "" {
		where = append(where, "(user = ? OR name = ?)")
		args = append(args, u, u)
	}
	if q.Denied {
		where = append(where, "allowed = 0")
	}
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	sqlText += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := l.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var allowed int
		if err := rows.Scan(&r.ID, &r.At, &r.User, &r.Name, &r.Agent, &r.Path, &r.Space, &allowed, &r.Route, &r.Addr); err != nil {
			return nil, err
		}
		r.Allowed = allowed == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// RetentionDays is how long records are kept by default. An audit trail that
// grows forever becomes its own liability — it is a list of exactly which
// people looked at exactly which sensitive documents.
const RetentionDays = 90

// Prune drops records older than days. A value <= 0 keeps everything.
func (l *Log) Prune(days int) (int64, error) {
	if l == nil || l.db == nil || days <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	n, err := l.db.Count(`SELECT count(*) FROM read_audit WHERE at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	if err := l.db.Exec(`DELETE FROM read_audit WHERE at < ?`, cutoff); err != nil {
		return 0, err
	}
	return int64(n), nil
}

// Count is how many records the trail holds.
//
// It answers a question an empty anomaly scan cannot: on a single-user
// instance nothing is restricted, so nothing is ever recorded, and "no
// anomalies" there means "not applicable" rather than "all clear". A surface
// that cannot tell those apart reassures people about a check that never ran.
func (l *Log) Count() int {
	if l == nil || l.db == nil {
		return 0
	}
	n, err := l.db.Count("SELECT COUNT(*) FROM read_audit")
	if err != nil {
		return 0
	}
	return n
}
