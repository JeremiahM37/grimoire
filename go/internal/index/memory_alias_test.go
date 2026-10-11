package index

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

var aliasNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// sessEntry is entry() with a session, which is what alias scope reads.
func sessEntry(id, session, stamp, text string) memory.Entry {
	return memory.Entry{ID: id, Session: session, Stamp: stamp, Agent: "claude", Text: text}
}

// linkCount counts rows in the link table matching a condition.
func linkCount(t *testing.T, ix *Index, where string, args ...any) int {
	t.Helper()
	rows, err := ix.DB.Query("SELECT COUNT(*) FROM memory_links WHERE "+where, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// linkSnapshot is every link row, in a stable order, for comparing two builds.
func linkSnapshot(t *testing.T, ix *Index) []string {
	t.Helper()
	rows, err := ix.DB.Query("SELECT note,id,alias,entity,scope,conf FROM memory_links ORDER BY note,id,alias")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var note, id, alias, entity, scope string
		var conf float64
		if err := rows.Scan(&note, &id, &alias, &entity, &scope, &conf); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s|%g", note, id, alias, entity, scope, conf))
	}
	return out
}

func aliasHit(t *testing.T, hits []MemoryHit, id string) MemoryHit {
	t.Helper()
	for _, h := range hits {
		if h.ID == id {
			return h
		}
	}
	t.Fatalf("%s not in %v", id, ids(hits))
	return MemoryHit{}
}

func TestSessionAliasLinksAndRanksWithTheFullName(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/team.md",
		sessEntry("full", "s1", "2026-08-14 09:00", "Priya N. Schmidt owns the release checklist"),
		sessEntry("alias", "s1", "2026-08-14 09:05", "Priya prefers tabs over spaces"),
		sessEntry("other", "", "2026-08-14 09:10", "the NAS disk is at 80 percent"))

	hits, err := ix.MemoryEntries(MemoryQuery{Query: "what does Priya N. Schmidt prefer", Limit: 5, Now: aliasNow})
	if err != nil {
		t.Fatal(err)
	}
	alias := aliasHit(t, hits, "alias")
	if alias.LinkScope != memory.ScopeSession {
		t.Errorf("link scope = %q, want session", alias.LinkScope)
	}
	if want := memory.LinkScore(1.0); alias.Link != want || alias.Entity < want {
		t.Errorf("link = %v, entity = %v, want link %v", alias.Link, alias.Entity, want)
	}
	if other := aliasHit(t, hits, "other"); other.Link != 0 || other.LinkScope != "" {
		t.Errorf("unlinked fact got link %v / %q", other.Link, other.LinkScope)
	}
	if n := linkCount(t, ix, "note=? AND id=? AND alias=? AND entity=?", "memory/team.md", "alias", "priya", "priya n schmidt"); n != 1 {
		t.Errorf("link rows = %d, want 1", n)
	}
}

func TestAliasWithoutAFullNameInTheQueryGetsNoBoost(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/team.md",
		sessEntry("full", "s1", "2026-08-14 09:00", "Priya N. Schmidt owns the release checklist"),
		sessEntry("alias", "s1", "2026-08-14 09:05", "Priya prefers tabs over spaces"))
	hits, err := ix.MemoryEntries(MemoryQuery{Query: "what does Priya prefer", Limit: 5, Now: aliasNow})
	if err != nil {
		t.Fatal(err)
	}
	if alias := aliasHit(t, hits, "alias"); alias.Link != 0 {
		t.Errorf("link = %v for a query with no full name, want 0", alias.Link)
	}
}

func TestAmbiguousSessionLeavesTheAliasUnlinked(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/team.md",
		sessEntry("a", "s1", "2026-08-14 09:00", "Priya N. Schmidt owns the release checklist"),
		sessEntry("b", "s1", "2026-08-14 09:01", "Priya A. Sharma owns the backup rotation"),
		sessEntry("alias", "s1", "2026-08-14 09:02", "Priya prefers tabs over spaces"))
	if n := linkCount(t, ix, "id=?", "alias"); n != 0 {
		t.Errorf("ambiguous alias produced %d link rows, want 0", n)
	}
	hits, err := ix.MemoryEntries(MemoryQuery{Query: "what does Priya N. Schmidt prefer", Limit: 5, Now: aliasNow})
	if err != nil {
		t.Fatal(err)
	}
	if alias := aliasHit(t, hits, "alias"); alias.Link != 0 {
		t.Errorf("ambiguous alias was linked: %v", alias.Link)
	}
}

func TestLaterFullNameInAnotherSessionDoesNotRelinkAnEarlierFact(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/early.md",
		sessEntry("alias", "s1", "2026-08-01 09:00", "Ilya prefers dark mode"))
	memNote(t, ix, "memory/late.md",
		sessEntry("full", "s2", "2026-08-20 09:00", "Ilya K. Novak owns the backups"))
	if n := linkCount(t, ix, "id=?", "alias"); n != 0 {
		t.Fatalf("a later full name relinked an earlier fact: %d rows", n)
	}
	hits, err := ix.MemoryEntries(MemoryQuery{Query: "what does Ilya K. Novak prefer", Limit: 5, Now: aliasNow})
	if err != nil {
		t.Fatal(err)
	}
	if alias := aliasHit(t, hits, "alias"); alias.Link != 0 {
		t.Errorf("earlier fact picked up link %v from a later session", alias.Link)
	}
}

func TestDeletingTheEstablishingNoteRemovesItsLinks(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/alias.md",
		sessEntry("alias", "s1", "2026-08-14 09:05", "Priya prefers tabs over spaces"))
	memNote(t, ix, "memory/full.md",
		sessEntry("full", "s1", "2026-08-14 09:00", "Priya N. Schmidt owns the release checklist"))
	if n := linkCount(t, ix, "id=?", "alias"); n != 1 {
		t.Fatalf("before delete: %d link rows, want 1", n)
	}
	if err := ix.Remove("memory/full.md"); err != nil {
		t.Fatal(err)
	}
	if n := linkCount(t, ix, "id=?", "alias"); n != 0 {
		t.Errorf("after deleting the establishing note: %d link rows, want 0", n)
	}
}

func TestRewritingTheEstablishingFactRemovesItsLinks(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/alias.md",
		sessEntry("alias", "s1", "2026-08-14 09:05", "Priya prefers tabs over spaces"))
	memNote(t, ix, "memory/full.md",
		sessEntry("full", "s1", "2026-08-14 09:00", "Priya N. Schmidt owns the release checklist"))
	if n := linkCount(t, ix, "id=?", "alias"); n != 1 {
		t.Fatalf("before rewrite: %d link rows, want 1", n)
	}
	// The same note, rewritten without the full name.
	memNote(t, ix, "memory/full.md",
		sessEntry("full", "s1", "2026-08-14 09:00", "the release checklist moved to Friday"))
	if n := linkCount(t, ix, "id=?", "alias"); n != 0 {
		t.Errorf("after rewrite: %d link rows, want 0", n)
	}
}

func TestServiceAliasLinksWithinASession(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/ops.md",
		sessEntry("svc", "s7", "2026-08-14 09:00", "auth-gateway-03 listens on port 8443"),
		sessEntry("alias", "s7", "2026-08-14 09:01", "auth-gateway runs on db01.prod"))
	memNote(t, ix, "memory/other.md",
		sessEntry("decoy", "s9", "2026-08-02 09:00", "auth-gateway-07 listens on port 9000"))
	hits, err := ix.MemoryEntries(MemoryQuery{Query: "where does auth-gateway-03 run", Limit: 5, Now: aliasNow})
	if err != nil {
		t.Fatal(err)
	}
	alias := aliasHit(t, hits, "alias")
	if alias.LinkScope != memory.ScopeSession {
		t.Errorf("service alias scope = %q, want session", alias.LinkScope)
	}
	// "auth-gateway" and its first component "auth" are both short forms of the
	// service, so the one fact gets one link per form, and both point at the
	// same entity. The service's own fact links to nothing: it names itself.
	if n := linkCount(t, ix, "id=? AND entity=? AND alias IN ('auth','auth-gateway')", "alias", "auth-gateway-03"); n != 2 {
		t.Errorf("link rows to auth-gateway-03 = %d, want 2 (auth, auth-gateway)", n)
	}
	if n := linkCount(t, ix, "id=?", "svc"); n != 0 {
		t.Errorf("the service's own fact has %d link rows, want 0", n)
	}
}

// A rebuild must produce exactly the links the incremental writes produced.
// That is the property that makes the tables derived rather than state.
func TestIncrementalLinksEqualARebuild(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/a.md",
		sessEntry("alias1", "s1", "2026-08-01 09:00", "Priya prefers tabs over spaces"),
		sessEntry("alias2", "s2", "2026-08-02 09:00", "Ilya prefers dark mode"),
		sessEntry("svc", "s3", "2026-08-03 09:00", "gamarr listens on port 5057"))
	memNote(t, ix, "memory/b.md",
		sessEntry("full1", "s1", "2026-08-04 09:00", "Priya N. Schmidt owns the release checklist"),
		sessEntry("full2", "s3", "2026-08-05 09:00", "gamarr-191 listens on port 5057"))
	memNote(t, ix, "memory/c.md",
		sessEntry("full3", "s2", "2026-08-20 09:00", "Ilya K. Novak owns the backups"))
	// Drop one establishing fact and move another, so the flush has to unlink
	// as well as link.
	memNote(t, ix, "memory/b.md",
		sessEntry("full1", "s1", "2026-08-04 09:00", "Priya N. Schmidt owns the release checklist"),
		sessEntry("full2", "s4", "2026-08-05 09:00", "gamarr-191 listens on port 5057"))

	incremental := linkSnapshot(t, ix)
	if len(incremental) == 0 {
		t.Fatal("scenario produced no links; the comparison would prove nothing")
	}
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	rebuilt := linkSnapshot(t, ix)
	if !reflect.DeepEqual(incremental, rebuilt) {
		t.Errorf("incremental links differ from a rebuild:\n incremental %v\n rebuilt     %v", incremental, rebuilt)
	}
}
