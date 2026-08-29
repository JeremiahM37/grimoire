package readlog

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

func open(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestRecordsBothOutcomes(t *testing.T) {
	l := New(open(t))
	l.Start()
	l.Record(Event{User: "u1", Name: "ada", Path: "hr/reviews.md", Space: "hr", Allowed: true, Route: "GET /api/note"})
	l.Record(Event{User: "u2", Name: "bob", Path: "hr/reviews.md", Space: "hr", Allowed: false, Route: "GET /api/note"})
	l.Flush()
	l.Close()

	rows, err := l.Recent(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	// Newest first.
	if rows[0].Name != "bob" || rows[0].Allowed {
		t.Fatalf("denied read not recorded first: %+v", rows[0])
	}
	if rows[1].Name != "ada" || !rows[1].Allowed {
		t.Fatalf("allowed read not recorded: %+v", rows[1])
	}
}

func TestDeniedFilterFindsProbing(t *testing.T) {
	l := New(open(t))
	l.Start()
	for i := 0; i < 5; i++ {
		l.Record(Event{User: "u2", Name: "bob", Path: "hr/salaries.md", Allowed: false})
	}
	l.Record(Event{User: "u1", Name: "ada", Path: "hr/salaries.md", Allowed: true})
	l.Flush()
	l.Close()

	denied, err := l.Recent(Query{Denied: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(denied) != 5 {
		t.Fatalf("want 5 denied attempts, got %d", len(denied))
	}
	byUser, err := l.Recent(Query{User: "ada"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byUser) != 1 || !byUser[0].Allowed {
		t.Fatalf("per-user filter wrong: %+v", byUser)
	}
	byPath, err := l.Recent(Query{Path: "salaries"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byPath) != 6 {
		t.Fatalf("want 6 by path, got %d", len(byPath))
	}
}

func TestPruneKeepsRecentDropsOld(t *testing.T) {
	d := open(t)
	l := New(d)
	// Written directly so their timestamps can be old.
	old := time.Now().UTC().AddDate(0, 0, -200).Format(time.RFC3339)
	if err := d.Exec(`INSERT INTO read_audit(at,user,name,path,space,allowed,route,addr) VALUES(?,?,?,?,?,?,?,?)`,
		old, "u1", "ada", "old.md", "hr", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	l.Start()
	l.Record(Event{User: "u1", Name: "ada", Path: "new.md", Allowed: true})
	l.Flush()
	l.Close()

	n, err := l.Prune(90)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 pruned, got %d", n)
	}
	rows, _ := l.Recent(Query{})
	if len(rows) != 1 || rows[0].Path != "new.md" {
		t.Fatalf("prune took the wrong row: %+v", rows)
	}
	// A retention of zero is "keep everything", not "delete everything".
	if n, err := l.Prune(0); err != nil || n != 0 {
		t.Fatalf("Prune(0) should be a no-op, got %d %v", n, err)
	}
	if rows, _ := l.Recent(Query{}); len(rows) != 1 {
		t.Fatalf("Prune(0) deleted rows")
	}
}

// A nil database must not panic: the CLI builds a Log before it knows whether
// this deployment has one.
func TestNilIsInert(t *testing.T) {
	var l *Log
	l.Record(Event{Path: "x"})
	l.Start()
	l.Flush()
	l.Close()
	if rows, err := l.Recent(Query{}); err != nil || rows != nil {
		t.Fatalf("nil log should read empty: %v %v", rows, err)
	}
	if l.Dropped() != 0 {
		t.Fatal("nil log counted a drop")
	}
	empty := New(nil)
	empty.Record(Event{Path: "x"})
	empty.Start()
	empty.Close()
}

// Record must never block a request goroutine, even with nothing draining.
func TestRecordNeverBlocks(t *testing.T) {
	l := New(open(t)) // deliberately not Started
	done := make(chan struct{})
	go func() {
		// Allowed reads: the throttle applies only to denials, so these are
		// what can actually fill the buffer.
		for i := 0; i < Depth*2; i++ {
			l.Record(Event{User: "u", Name: "u", Path: "x", Space: "hr", Allowed: true})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked when the buffer filled")
	}
	if l.Dropped() == 0 {
		t.Fatal("overflow was not counted")
	}
}

// Probing must not be able to write rows forever. The bound keeps the signal —
// that this actor was refused — while dropping the volume.
func TestDenialFloodIsBoundedButAllowedReadsAreNot(t *testing.T) {
	l := New(open(t))
	l.Start()
	for i := 0; i < DenialsPerWindow*3; i++ {
		l.Record(Event{User: "u2", Name: "bob", Path: "hr/invented.md", Allowed: false})
	}
	// An allowed read is never throttled: it is a real access, and losing one
	// is losing the record the trail exists for.
	for i := 0; i < DenialsPerWindow+50; i++ {
		l.Record(Event{User: "u1", Name: "ada", Path: "hr/real.md", Space: "hr", Allowed: true})
	}
	l.Flush()
	l.Close()

	denied, err := l.Recent(Query{Denied: true, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(denied) != DenialsPerWindow {
		t.Fatalf("stored %d denials, want the bound of %d", len(denied), DenialsPerWindow)
	}
	if l.Suppressed() != int64(DenialsPerWindow*2) {
		t.Fatalf("suppressed = %d, want %d", l.Suppressed(), DenialsPerWindow*2)
	}
	allowed, err := l.Recent(Query{User: "ada", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) != DenialsPerWindow+50 {
		t.Fatalf("allowed reads were throttled: %d of %d", len(allowed), DenialsPerWindow+50)
	}
}

// Two actors each get their own allowance; one probing must not blind the
// trail to another.
func TestDenialBoundIsPerActor(t *testing.T) {
	l := New(open(t))
	l.Start()
	for i := 0; i < DenialsPerWindow*2; i++ {
		l.Record(Event{User: "noisy", Name: "noisy", Path: "x.md", Allowed: false})
	}
	l.Record(Event{User: "quiet", Name: "quiet", Path: "y.md", Allowed: false})
	l.Flush()
	l.Close()

	rows, err := l.Recent(Query{User: "quiet"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("a second actor's denial was lost to the first's flood: %+v", rows)
	}
}

// Caller-controlled fields are truncated before they reach the database.
func TestOversizedFieldsAreClipped(t *testing.T) {
	l := New(open(t))
	l.Start()
	l.Record(Event{User: "u", Name: "u", Path: strings.Repeat("a", maxField*4), Allowed: false})
	l.Flush()
	l.Close()
	rows, err := l.Recent(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Path) != maxField {
		t.Fatalf("path not clipped: %d bytes", len(rows[0].Path))
	}
}

// The trail has to say WHAT read a note, not only on whose account.
//
// On a single-user deployment there is no account at all, so an entry with
// only a user column answers none of the question. And an agent is not a
// person: merging the two into one name could not distinguish "I opened this"
// from "something running as me opened this", which is the distinction the
// trail exists for once agents are doing the reading.
func TestTheAgentIsRecordedSeparatelyFromTheAccount(t *testing.T) {
	l := New(open(t))
	l.Start()
	l.Record(Event{User: "u1", Name: "ada", Agent: "laptop",
		Path: "hr/reviews.md", Space: "hr", Allowed: true, Route: "GET /api/notes"})
	// No account, which is every single-user install.
	l.Record(Event{Agent: "MediaServer",
		Path: "hr/reviews.md", Space: "hr", Allowed: true, Route: "GET /api/notes"})
	l.Flush()
	l.Close()

	rows, err := l.Recent(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].Agent != "MediaServer" {
		t.Errorf("agent = %q, want MediaServer — with no account this is the "+
			"only thing that says who read", rows[0].Agent)
	}
	if rows[0].Name != "" {
		t.Errorf("name = %q; an agent must not be written into the account column", rows[0].Name)
	}
	if rows[1].Agent != "laptop" || rows[1].Name != "ada" {
		t.Errorf("row = %+v, want both the account (ada) and the agent (laptop)", rows[1])
	}
}
