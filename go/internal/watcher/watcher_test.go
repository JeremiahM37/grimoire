package watcher

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

type recorder struct {
	mu       sync.Mutex
	upserted []string
	removed  []string
}

func (r *recorder) Upsert(rel string) (*vault.Note, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upserted = append(r.upserted, rel)
	return nil, nil
}

func (r *recorder) Remove(rel string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, rel)
	return nil
}

func (r *recorder) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.upserted), len(r.removed)
}

func (r *recorder) sawUpsert(rel string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, u := range r.upserted {
		if u == rel {
			return true
		}
	}
	return false
}

func setup(t *testing.T) (*vault.Vault, *recorder, *Watcher) {
	t.Helper()
	v, err := vault.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	w := New(v, rec, 100*time.Millisecond)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	time.Sleep(150 * time.Millisecond) // let the watch establish
	return v, rec, w
}

func waitFor(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestExternalWriteIsIndexed(t *testing.T) {
	v, rec, _ := setup(t)
	p := filepath.Join(v.Root, "external.md")
	if err := os.WriteFile(p, []byte("# External\n\nwritten outside grimoire\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return rec.sawUpsert("external.md") }, 3*time.Second) {
		t.Error("external write was not indexed")
	}
}

func TestExternalDeleteIsRemoved(t *testing.T) {
	v, rec, _ := setup(t)
	p := filepath.Join(v.Root, "doomed.md")
	if err := os.WriteFile(p, []byte("# Doomed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(func() bool { return rec.sawUpsert("doomed.md") }, 3*time.Second)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	ok := waitFor(func() bool {
		_, removed := rec.counts()
		return removed > 0
	}, 3*time.Second)
	if !ok {
		t.Error("deleted note was not removed from the index")
	}
}

// REGRESSION (from the Python implementation): indexing reads the file, and a
// watcher that reacts to reads re-queues the note it just indexed — forever.
func TestReadingANoteDoesNotRequeueIt(t *testing.T) {
	v, rec, _ := setup(t)
	p := filepath.Join(v.Root, "readme.md")
	if err := os.WriteFile(p, []byte("# Read Me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(func() bool { return rec.sawUpsert("readme.md") }, 3*time.Second)
	time.Sleep(300 * time.Millisecond)
	before, _ := rec.counts()

	for i := 0; i < 10; i++ {
		if _, err := os.ReadFile(p); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(600 * time.Millisecond) // well past the debounce window

	after, _ := rec.counts()
	if after != before {
		t.Errorf("reads re-queued the note: %d upserts before, %d after", before, after)
	}
}

// Reserved dirs hold the index and CRDT state; reacting to them would be its
// own feedback loop.
func TestReservedDirsAreIgnored(t *testing.T) {
	v, rec, _ := setup(t)
	for _, dir := range vault.ReservedDirs {
		d := filepath.Join(v.Root, dir)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "internal.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(600 * time.Millisecond)
	if up, _ := rec.counts(); up != 0 {
		t.Errorf("reserved-dir writes triggered %d upserts", up)
	}
}

// A burst of saves must collapse into one pass, not one per event.
func TestBurstIsDebounced(t *testing.T) {
	v, rec, _ := setup(t)
	p := filepath.Join(v.Root, "burst.md")
	for i := 0; i < 12; i++ {
		if err := os.WriteFile(p, []byte("# Burst\n\nrevision\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitFor(func() bool { return rec.sawUpsert("burst.md") }, 3*time.Second)
	time.Sleep(400 * time.Millisecond)
	if up, _ := rec.counts(); up > 4 {
		t.Errorf("12 rapid writes produced %d upserts; debounce is not collapsing them", up)
	}
}

// Notes created in a directory that did not exist at start must still index.
func TestNewSubdirectoryIsWatched(t *testing.T) {
	v, rec, _ := setup(t)
	sub := filepath.Join(v.Root, "later")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(sub, "new.md"), []byte("# New\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return rec.sawUpsert("later/new.md") }, 3*time.Second) {
		t.Error("note in a newly created directory was not indexed")
	}
}

// A contended database must not cost a note its place in the index.
//
// The watcher used to log the error and move on, so a note edited while another
// process held the write lock was simply never indexed. That failure is
// invisible — it reads as the agent not knowing something, not as a fault.
// Observed in production as seven "database is locked" lines in one second
// while a CLI command held the lock.

type flakyIndex struct {
	failures int // how many times to report a locked database first
	calls    int
	upserted []string
}

func (f *flakyIndex) Upsert(rel string) (*vault.Note, error) {
	f.calls++
	if f.calls <= f.failures {
		return nil, errors.New("database is locked (5) (SQLITE_BUSY)")
	}
	f.upserted = append(f.upserted, rel)
	return &vault.Note{Path: rel}, nil
}

func (f *flakyIndex) Remove(rel string) error { return nil }

func TestUpsertRetriesALockedDatabase(t *testing.T) {
	w := &Watcher{index: &flakyIndex{failures: 2}}
	if _, err := w.upsertWithRetry("a.md"); err != nil {
		t.Fatalf("gave up on a transient lock: %v", err)
	}
	idx := w.index.(*flakyIndex)
	if len(idx.upserted) != 1 {
		t.Errorf("note not indexed after the lock cleared: %v", idx.upserted)
	}
	if idx.calls != 3 {
		t.Errorf("attempts = %d, want 3 (two locked, one through)", idx.calls)
	}
}

func TestUpsertGivesUpEventually(t *testing.T) {
	w := &Watcher{index: &flakyIndex{failures: 99}}
	if _, err := w.upsertWithRetry("a.md"); err == nil {
		t.Fatal("a permanently locked database reported success")
	}
	if calls := w.index.(*flakyIndex).calls; calls != busyRetries {
		t.Errorf("attempts = %d, want exactly %d", calls, busyRetries)
	}
}

// A real error must fail immediately — retrying a malformed note three times
// just delays the same failure and muddies the log.
func TestNonBusyErrorsAreNotRetried(t *testing.T) {
	w := &Watcher{index: &permanentErrIndex{}}
	if _, err := w.upsertWithRetry("a.md"); err == nil {
		t.Fatal("a permanent error was swallowed")
	}
	if c := w.index.(*permanentErrIndex).calls; c != 1 {
		t.Errorf("attempts = %d, want 1 — a parse error is not contention", c)
	}
}

type permanentErrIndex struct{ calls int }

func (p *permanentErrIndex) Upsert(rel string) (*vault.Note, error) {
	p.calls++
	return nil, errors.New("malformed frontmatter")
}
func (p *permanentErrIndex) Remove(rel string) error { return nil }

func TestIsBusyRecognisesTheDriversWording(t *testing.T) {
	for _, s := range []string{
		"database is locked (5) (SQLITE_BUSY)",
		"database table is locked",
		"SQLITE_BUSY",
	} {
		if !isBusy(errors.New(s)) {
			t.Errorf("%q not recognised as contention", s)
		}
	}
	for _, s := range []string{"malformed frontmatter", "no such table: notes", ""} {
		if isBusy(errors.New(s)) {
			t.Errorf("%q wrongly treated as contention", s)
		}
	}
}
