package index

import (
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Hidden facts are excluded in SQL, before the scan bound, unless a read asks
// for them. The exclusion is the index's, so every surface inherits it.

func TestHiddenFactsAreLeftOutUnlessRequested(t *testing.T) {
	ix := testIndex(t)
	open := entry("o1", daysAgo(1), "the deploy host is prod-1")
	priv := entry("p1", daysAgo(2), "the deploy key is in the vault")
	priv.Visibility = memory.VisPrivate
	sens := entry("s1", daysAgo(3), "the deploy password is on the card")
	sens.Visibility = memory.VisSensitive
	memNote(t, ix, "memory/ops.md", open, priv, sens)

	hits, err := ix.MemoryEntries(MemoryQuery{Query: "deploy", Now: fixedNow, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(hits); len(got) != 1 || got[0] != "o1" {
		t.Fatalf("default read returned %v, want only the normal fact", got)
	}

	hits, err = ix.MemoryEntries(MemoryQuery{Query: "deploy", IncludePrivate: true, Now: fixedNow, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("include-private read returned %v, want all three", ids(hits))
	}
	if h, _ := hitByID(hits, "p1"); h.Visibility != memory.VisPrivate {
		t.Errorf("private fact lost its visibility in the index: %q", h.Visibility)
	}
	if h, _ := hitByID(hits, "s1"); h.Visibility != memory.VisSensitive {
		t.Errorf("sensitive fact lost its visibility in the index: %q", h.Visibility)
	}
}

func TestVisibilitySurvivesReindex(t *testing.T) {
	ix := testIndex(t)
	sens := entry("s1", daysAgo(2), "the payroll login sits in the safe")
	sens.Visibility = memory.VisSensitive
	memNote(t, ix, "memory/ops.md", sens)
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.MemoryEntries(MemoryQuery{Note: "memory/ops.md", Now: fixedNow, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("a sensitive fact came back after reindex without include-private: %v", ids(hits))
	}
	hits, err = ix.MemoryEntries(MemoryQuery{Note: "memory/ops.md", IncludePrivate: true, Now: fixedNow, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Visibility != memory.VisSensitive {
		t.Fatalf("after reindex: %+v", hits)
	}
}
