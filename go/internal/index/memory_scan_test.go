package index

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// scanCorpus writes `notes` memory notes of `perNote` facts each. Facts are
// generated from small vocabularies so that queries have real overlap, and a
// deterministic slice of them is superseded, expiring or time-bounded so that
// every filter path has rows to exclude. Stamps run across 2026 so "newest"
// and "old" are both well defined.
func scanCorpus(t *testing.T, ix *Index, notes, perNote int) []memory.Entry {
	t.Helper()
	var all []memory.Entry
	subjects := []string{"Priya", "Marcus", "the AIServer", "the MediaServer", "Dana", "the backup job", "Grafana"}
	verbs := []string{"restarted", "prefers", "migrated", "reviewed", "deployed"}
	objects := []string{"the disk alert", "dark mode", "the sonarr stack", "the restic repo",
		"the nginx vhost", "the kavita library", "the proxmox node", "the librarr config",
		"the tailscale route", "the prometheus scrape", "the ollama model"}
	for n := 0; n < notes; n++ {
		var entries []memory.Entry
		for k := 0; k < perNote; k++ {
			i := n*perNote + k
			month := 1 + i%12
			day := 1 + (i/12)%28
			e := memory.Entry{
				ID:       fmt.Sprintf("f%05d", i),
				Text:     fmt.Sprintf("%s %s %s run %d", subjects[i%len(subjects)], verbs[i%len(verbs)], objects[i%len(objects)], i),
				Agent:    "claude",
				Stamp:    fmt.Sprintf("2026-%02d-%02d %02d:%02d", month, day, i%24, i%60),
				Category: []string{"fact", "preference", "procedure"}[i%3],
			}
			switch {
			case i%23 == 7:
				e.SupersededBy = fmt.Sprintf("f%05d", i+1)
				e.SupersededAt = fmt.Sprintf("2026-%02d-%02d 12:00", 1+(month%12), day)
			case i%29 == 11:
				e.Expires = "2026-02-01T00:00:00Z"
			case i%31 == 13:
				e.ValidFrom = fmt.Sprintf("2026-%02d-01T00:00:00Z", month)
				e.ValidTo = fmt.Sprintf("2026-%02d-01T00:00:00Z", 1+month%12)
			}
			entries = append(entries, e)
		}
		memNote(t, ix, fmt.Sprintf("memory/topic%03d.md", n), entries...)
		all = append(all, entries...)
	}
	return all
}

// goldenHit is the subset of a hit that the golden file pins. Scores are
// stored as exact float64 text so a one-ULP change in ranking is visible.
type goldenHit struct {
	ID       string  `json:"id"`
	Note     string  `json:"note"`
	Score    float64 `json:"score"`
	Semantic float64 `json:"semantic"`
	Keyword  float64 `json:"keyword"`
	Entity   float64 `json:"entity"`
	Recency  float64 `json:"recency"`
	Useful   float64 `json:"useful"`
}

type goldenCase struct {
	Name string      `json:"name"`
	Hits []goldenHit `json:"hits"`
}

// goldenQueries are the cases the 500-row golden file pins. They cover plain
// text, entity-heavy text, no text (newest first), a mid-corpus as_of, a
// valid_at, and as_of combined with text. Now is pinned for recency.
func goldenQueries() map[string]MemoryQuery {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	asOf := time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local)
	validAt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	return map[string]MemoryQuery{
		"plain":       {Query: "Priya restarted the disk alert", Limit: 20, Now: now},
		"entities":    {Query: "Grafana and the AIServer", Limit: 20, Now: now},
		"newest":      {Limit: 20, Now: now},
		"as_of":       {Limit: 20, Now: now, AsOf: asOf},
		"valid_at":    {Query: "Dana prefers dark mode", Limit: 20, Now: now, ValidAt: validAt},
		"as_of_text":  {Query: "Marcus migrated the restic repo", Limit: 20, Now: now, AsOf: asOf},
		"private_off": {Query: "Dana deployed", Limit: 20, Now: now, Filter: Filter{IncludePrivate: false}},
		"superseded":  {Query: "the backup job", Limit: 20, Now: now, IncludeSuperseded: true},
	}
}

func runGolden(t *testing.T, ix *Index) []goldenCase {
	t.Helper()
	names := []string{"plain", "entities", "newest", "as_of", "valid_at", "as_of_text", "private_off", "superseded"}
	qs := goldenQueries()
	out := make([]goldenCase, 0, len(names))
	for _, name := range names {
		hits, err := ix.MemoryEntries(qs[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c := goldenCase{Name: name}
		for _, h := range hits {
			c.Hits = append(c.Hits, goldenHit{ID: h.ID, Note: h.Note, Score: h.Score,
				Semantic: h.Semantic, Keyword: h.Keyword, Entity: h.Entity,
				Recency: h.Recency, Useful: h.Useful})
		}
		out = append(out, c)
	}
	return out
}

const goldenPath = "testdata/memory_scan_golden.json"

// TestDumpMemoryScanGolden regenerates the golden file. It only runs when
// GRIMOIRE_WRITE_GOLDEN is set, and it was run once against the pre-candidate
// code so that the file records what the window path returned before any
// change in this package touched it.
func TestDumpMemoryScanGolden(t *testing.T) {
	if os.Getenv("GRIMOIRE_WRITE_GOLDEN") == "" {
		t.Skip("set GRIMOIRE_WRITE_GOLDEN=1 to regenerate " + goldenPath)
	}
	ix := testIndex(t)
	scanCorpus(t, ix, 50, 10)
	b, err := json.MarshalIndent(runGolden(t, ix), "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goldenPath, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBelowScanLimitMatchesGolden is the byte-identity guard: on a 500-fact
// corpus, far below DefaultScanLimit, every recall must return exactly what the
// pre-candidate window path returned.
func TestBelowScanLimitMatchesGolden(t *testing.T) {
	ix := testIndex(t)
	scanCorpus(t, ix, 50, 10)
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var want []goldenCase
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	got := runGolden(t, ix)
	gb, _ := json.MarshalIndent(got, "", " ")
	wb, _ := json.MarshalIndent(want, "", " ")
	if string(gb) != string(wb) {
		t.Fatalf("results below the scan limit changed from the golden file\n--- want\n%s\n--- got\n%s", wb, gb)
	}
}
