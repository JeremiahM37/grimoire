package index

import (
	"sort"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

func mustRFC(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func validFact(id, text, from, to string) memory.Entry {
	e := entry(id, "2026-08-10 09:00", text)
	e.ValidFrom, e.ValidTo = from, to
	return e
}

func TestValidAtSelectsFactsTrueInTheWorld(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/world.md",
		validFact("spring", "the office is on floor three", "2026-03-01", "2026-09-01T00:00:00Z"),
		validFact("autumn", "the office is on floor four", "2026-09-01T00:00:00Z", ""),
		entry("always", "2026-08-10 09:00", "the company name is Acme"))

	cases := []struct {
		at   string
		want []string
	}{
		{"2026-02-01T00:00:00Z", []string{"always"}},
		{"2026-06-01T00:00:00Z", []string{"always", "spring"}},
		{"2026-09-01T00:00:00Z", []string{"always", "autumn"}}, // boundary: spring ended, autumn began
		{"2027-01-01T00:00:00Z", []string{"always", "autumn"}},
	}
	for _, c := range cases {
		hits, err := ix.MemoryEntries(MemoryQuery{ValidAt: mustRFC(t, c.at), Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if got := sorted(ids(hits)); !equal(got, sorted(c.want)) {
			t.Errorf("valid at %s: got %v, want %v", c.at, got, c.want)
		}
	}
}

func TestValidAtAgreesWithTheEntryMethod(t *testing.T) {
	// The SQL predicate and Entry.ValidAt are two implementations of one rule.
	// They must agree on every boundary, or a query and a single fact disagree.
	ix := testIndex(t)
	facts := []memory.Entry{
		validFact("a", "alpha", "2026-03-01", "2026-03-05"),
		validFact("b", "beta", "2026-03-05T12:00:00Z", ""),
		validFact("c", "gamma", "", "2026-03-03T00:00:00Z"),
	}
	memNote(t, ix, "memory/agree.md", facts...)
	for _, at := range []string{
		"2026-02-28T23:59:59Z", "2026-03-01T00:00:00Z", "2026-03-03T00:00:00Z",
		"2026-03-04T00:00:00Z", "2026-03-05T00:00:00Z", "2026-03-05T12:00:00Z",
	} {
		when := mustRFC(t, at)
		hits, err := ix.MemoryEntries(MemoryQuery{ValidAt: when, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		inQuery := map[string]bool{}
		for _, h := range hits {
			inQuery[h.ID] = true
		}
		for _, f := range facts {
			if f.ValidAt(when) != inQuery[f.ID] {
				t.Errorf("at %s, fact %s: entry says %v but query says %v",
					at, f.ID, f.ValidAt(when), inQuery[f.ID])
			}
		}
	}
}

func TestValidRangeSelectsOverlappingFacts(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/range.md",
		validFact("early", "early", "2026-01-01", "2026-02-01"),
		validFact("mid", "mid", "2026-03-01", "2026-03-10"),
		validFact("late", "late", "2026-06-01", ""))

	s := mustRFC(t, "2026-02-15T00:00:00Z")
	u := mustRFC(t, "2026-03-05T00:00:00Z")
	hits, _ := ix.MemoryEntries(MemoryQuery{ValidSince: s, ValidUntil: u, Limit: 10})
	if got := sorted(ids(hits)); !equal(got, []string{"mid"}) {
		t.Errorf("range overlap: got %v, want [mid]", got)
	}
	hits, _ = ix.MemoryEntries(MemoryQuery{ValidSince: mustRFC(t, "2026-03-05T00:00:00Z"), Limit: 10})
	if got := sorted(ids(hits)); !equal(got, []string{"late", "mid"}) {
		t.Errorf("open-ended since: got %v, want [late mid]", got)
	}
	hits, _ = ix.MemoryEntries(MemoryQuery{ValidUntil: mustRFC(t, "2026-01-15T00:00:00Z"), Limit: 10})
	if got := sorted(ids(hits)); !equal(got, []string{"early"}) {
		t.Errorf("open-start until: got %v, want [early]", got)
	}
}

func TestAsOfCombinedWithValidAtIsTheBitemporalQuery(t *testing.T) {
	// "What did we believe on Aug 20 about what was true on Mar 1?"
	ix := testIndex(t)
	oldBelief := memory.Entry{ID: "old", Stamp: "2026-08-10 09:00", Agent: "claude",
		Text: "the office is on floor two", ValidFrom: "2026-01-01",
		SupersededBy: "new", SupersededAt: "2026-08-15 09:00"}
	newBelief := memory.Entry{ID: "new", Stamp: "2026-08-15 09:00", Agent: "claude",
		Text: "the office was on floor three from March", ValidFrom: "2026-03-01"}
	memNote(t, ix, "memory/office.md", oldBelief, newBelief)

	at := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	mar := mustRFC(t, "2026-03-15T00:00:00Z")

	// On Aug 12 we still believed the old fact, which was true in March.
	hits, _ := ix.MemoryEntries(MemoryQuery{AsOf: at("2026-08-12 09:00"), ValidAt: mar, Limit: 10})
	if got := ids(hits); !equal(got, []string{"old"}) {
		t.Errorf("believed on Aug 12 about March: got %v, want [old]", got)
	}
	// On Aug 20 the belief had been replaced, and the replacement is current.
	hits, _ = ix.MemoryEntries(MemoryQuery{AsOf: at("2026-08-20 09:00"), ValidAt: mar, Limit: 10})
	if got := ids(hits); !equal(got, []string{"new"}) {
		t.Errorf("believed on Aug 20 about March: got %v, want [new]", got)
	}
	// Current beliefs, valid in March: only the replacement.
	hits, _ = ix.MemoryEntries(MemoryQuery{ValidAt: mar, Limit: 10})
	if got := ids(hits); !equal(got, []string{"new"}) {
		t.Errorf("current beliefs about March: got %v, want [new]", got)
	}
}

func TestClosedValidityIsIndexed(t *testing.T) {
	ix := testIndex(t)
	old := memory.Entry{ID: "old", Stamp: "2026-08-10 09:00", Agent: "claude",
		Text: "the office is on floor two", ValidFrom: "2026-01-01T00:00:00Z",
		ValidTo: "2026-03-01T00:00:00Z"}
	memNote(t, ix, "memory/close.md", old)
	// The row mirrors the bullet, including a validity someone already closed.
	hits, _ := ix.MemoryEntries(MemoryQuery{IncludeSuperseded: true, Limit: 10})
	if len(hits) != 1 || hits[0].ValidTo != "2026-03-01T00:00:00Z" {
		t.Fatalf("validity not indexed: %+v", hits)
	}
}

func TestClosureIsNotVisibleBeforeItHappened(t *testing.T) {
	// The as-of path applies validity in Go, so this exercises the belief rule
	// through the index rather than only the entry method.
	ix := testIndex(t)
	old := memory.Entry{ID: "old", Stamp: "2026-08-10 09:00", Agent: "claude",
		Text: "floor two", ValidFrom: "2026-01-01T00:00:00Z", ValidTo: "2026-03-01T00:00:00Z",
		SupersededBy: "new", SupersededAt: "2026-08-15 09:00"}
	memNote(t, ix, "memory/close2.md", old)
	march := mustRFC(t, "2026-03-15T00:00:00Z")
	at := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	hits, _ := ix.MemoryEntries(MemoryQuery{AsOf: at("2026-08-13 09:00"), ValidAt: march, Limit: 10})
	if got := ids(hits); !equal(got, []string{"old"}) {
		t.Errorf("as of Aug 13 about March: got %v, want [old]", got)
	}
	hits, _ = ix.MemoryEntries(MemoryQuery{AsOf: at("2026-08-20 09:00"), ValidAt: march, Limit: 10})
	if got := ids(hits); len(got) != 0 {
		t.Errorf("as of Aug 20 about March: got %v, want none", got)
	}
}

func TestEntriesReturnTheirValidityWhenSet(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/out.md", validFact("v", "dated", "2026-03-01", ""),
		entry("n", "2026-08-10 09:00", "undated"))
	hits, _ := ix.MemoryEntries(MemoryQuery{Limit: 10})
	byID := map[string]MemoryHit{}
	for _, h := range hits {
		byID[h.ID] = h
	}
	if byID["v"].ValidFrom != "2026-03-01T00:00:00Z" {
		t.Errorf("valid_from = %q, want canonical RFC3339", byID["v"].ValidFrom)
	}
	if byID["n"].ValidFrom != "" || byID["n"].ValidTo != "" {
		t.Errorf("an undated fact grew validity: %+v", byID["n"].Entry)
	}
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
