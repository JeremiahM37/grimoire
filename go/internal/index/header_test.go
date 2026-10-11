package index

import (
	"reflect"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
)

func TestRenderHeaderChoosesFieldsAndOmitsEmpty(t *testing.T) {
	ents := []string{"priya n schmidt", "pgbouncer"}
	cases := []struct {
		fields map[string]bool
		want   string
	}{
		{map[string]bool{"title": true}, "Memory"},
		{map[string]bool{"title": true, "entities": true}, "Memory. priya n schmidt, pgbouncer"},
		{map[string]bool{"title": true, "entities": true, "date": true},
			"Memory. priya n schmidt, pgbouncer. 2026-08-14"},
		{map[string]bool{"date": true}, "2026-08-14"},
		{map[string]bool{}, ""},
	}
	for _, c := range cases {
		if got := renderHeader(c.fields, "Memory", ents, "2026-08-14"); got != c.want {
			t.Errorf("fields %v: got %q, want %q", c.fields, got, c.want)
		}
	}
	if got := renderHeader(map[string]bool{"title": true}, "", nil, ""); got != "" {
		t.Errorf("empty title should give an empty header, got %q", got)
	}
	if got := withHeader("", "the NAS is 80 percent"); got != "the NAS is 80 percent" {
		t.Errorf("empty header must leave the text alone, got %q", got)
	}
}

func TestHeaderOffLeavesTheIndexUnchanged(t *testing.T) {
	restore := exp.Set()
	defer restore()
	ix := testIndex(t)
	memNote(t, ix, "memory/team.md",
		sessEntry("full", "s1", "2026-08-14 09:00", "Priya N. Schmidt owns the release checklist"),
		sessEntry("alias", "s1", "2026-08-14 09:05", "Priya prefers tabs over spaces"))
	if n := tableCount(t, ix, "sqlite_master", "type='table' AND name='memory_headers'"); n != 0 {
		t.Fatalf("memory_headers exists with the flag off")
	}
}

func TestHeaderOnStoresHeaderAndRecallReturnsTheFactText(t *testing.T) {
	restore := exp.Set(headerFlag)
	defer restore()
	ix := testIndex(t)
	memNote(t, ix, "memory/team.md",
		sessEntry("full", "s1", "2026-08-14 09:00", "Priya N. Schmidt owns the release checklist"),
		sessEntry("alias", "s1", "2026-08-14 09:05", "Priya prefers tabs over spaces"))

	h := headerFor(t, ix, "memory/team.md", "alias")
	for _, want := range []string{"Memory", "priya n schmidt", "2026-08-14"} {
		if !strings.Contains(h, want) {
			t.Errorf("header %q lacks %q", h, want)
		}
	}
	hits, err := ix.MemoryEntries(MemoryQuery{Query: "what does Priya N. Schmidt prefer", Limit: 5, Now: aliasNow})
	if err != nil {
		t.Fatal(err)
	}
	if got := aliasHit(t, hits, "alias").Text; got != "Priya prefers tabs over spaces" {
		t.Errorf("recall text changed: %q", got)
	}
}

func TestHeaderIsRefreshedWhenALinkAppears(t *testing.T) {
	restore := exp.Set(headerFlag)
	defer restore()
	ix := testIndex(t)
	memNote(t, ix, "memory/a.md",
		sessEntry("alias", "s1", "2026-08-14 09:05", "Priya prefers tabs over spaces"))
	before := headerFor(t, ix, "memory/a.md", "alias")
	if strings.Contains(before, "priya n schmidt") {
		t.Fatalf("no full name exists yet, header should not name one: %q", before)
	}
	memNote(t, ix, "memory/b.md",
		sessEntry("full", "s1", "2026-08-14 09:00", "Priya N. Schmidt owns the release checklist"))

	after := headerFor(t, ix, "memory/a.md", "alias")
	if !strings.Contains(after, "priya n schmidt") {
		t.Fatalf("link appeared but header was not refreshed: %q", after)
	}
	// The stored vector must be the one for the header the index now holds.
	want := ix.Emb.Embed([]string{withHeader(after, "Priya prefers tabs over spaces")})[0]
	var got []float32
	var blob []byte
	if err := ix.DB.QueryRow("SELECT embedding FROM memory_entries WHERE note=? AND id=?",
		"memory/a.md", "alias").Scan(&blob); err != nil {
		t.Fatal(err)
	}
	got = Unpack(blob)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stored embedding is not the embedding of header+text")
	}
}

// headerFor reads the stored header of one fact.
func headerFor(t *testing.T, ix *Index, note, id string) string {
	t.Helper()
	var h string
	if err := ix.DB.QueryRow("SELECT header FROM memory_headers WHERE note=? AND id=?",
		note, id).Scan(&h); err != nil {
		t.Fatalf("no header row for %s/%s: %v", note, id, err)
	}
	return h
}

func tableCount(t *testing.T, ix *Index, table, where string) int {
	t.Helper()
	var n int
	if err := ix.DB.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE " + where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
