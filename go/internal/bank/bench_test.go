package bank

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// syntheticBank writes nDocs facts files of perDoc facts straight into the
// vault — the shape a long-lived bank reaches — and indexes them.
func syntheticBank(t testing.TB, h *harness, bankID string, nDocs, perDoc int) {
	t.Helper()
	rng := rand.New(rand.NewSource(7))
	people := []string{"Alice", "Bob", "Carol", "Dana", "Erin", "Farid", "Grace", "Hiro", "Ines", "Jonas",
		"Kemal", "Lena", "Mateo", "Nora", "Omar", "Priya", "Quinn", "Rosa", "Sven", "Tara"}
	places := []string{"Lyon", "Berlin", "Toronto", "Osaka", "Lisbon", "Austin", "Nairobi", "Oslo", "Lima", "Hanoi"}
	verbs := []string{"visited", "moved to", "gave a talk in", "bought a flat in", "ran a marathon in",
		"started a job in", "adopted a dog in", "learned pottery in", "met an old friend in", "fixed a bike in"}
	topics := []string{"gardening", "jazz", "chess", "climbing", "photography", "baking", "kayaking", "poetry",
		"astronomy", "woodworking", "sourdough", "birdwatching", "salsa", "go", "origami"}
	base := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := h.e.CreateBank(NewProfile(bankID)); err != nil {
		t.Fatal(err)
	}
	for d := 0; d < nDocs; d++ {
		doc := fmt.Sprintf("session-%04d", d)
		when := base.Add(time.Duration(d) * 41 * time.Hour)
		var ff FactsFile
		for i := 0; i < perDoc; i++ {
			p, q := people[rng.Intn(len(people))], people[rng.Intn(len(people))]
			pl, tp := places[rng.Intn(len(places))], topics[rng.Intn(len(topics))]
			var f Fact
			if i%3 == 0 {
				day := when.AddDate(0, 0, -rng.Intn(20))
				text := fmt.Sprintf("%s %s %s with %s on %s", p, verbs[rng.Intn(len(verbs))], pl, q, day.Format("January 2, 2006"))
				f = Fact{Text: text, Type: "world", Kind: "event", OccStart: day, OccEnd: day,
					Entities: []string{p, pl, q}}
			} else {
				text := fmt.Sprintf("%s has been into %s since meeting %s, and talks about it often", p, tp, q)
				f = Fact{Text: text, Type: "world", Kind: "conversation", Entities: []string{p, tp, q}}
			}
			f.Chunk, f.Mentioned = i/10, when
			f.ID = FactID(bankID, doc, f.Chunk, i, f.Text)
			ff.Facts = append(ff.Facts, f)
		}
		rel := FactsPath(bankID, doc)
		if _, err := h.v.Write(rel, FormatFacts(doc, ff), factsFrontmatter(bankID, doc)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRecallLatencyAt20kFacts measures what DECISIONS.md reports. Opt in with
// GRIMOIRE_BANK_BENCH=1; it writes 20,000 facts and takes tens of seconds.
func TestRecallLatencyAt20kFacts(t *testing.T) {
	if os.Getenv("GRIMOIRE_BANK_BENCH") == "" {
		t.Skip("set GRIMOIRE_BANK_BENCH=1 to measure recall at 20k facts")
	}
	h := newHarness(t, false)
	syntheticBank(t, h, "big", 400, 50)

	t0 := time.Now()
	if _, err := h.ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	cold := time.Since(t0)
	t0 = time.Now()
	if _, err := h.ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	warm := time.Since(t0)
	t0 = time.Now()
	c, err := h.e.cache("big")
	if err != nil {
		t.Fatal(err)
	}
	load := time.Since(t0)
	if len(c.units) != 20000 {
		t.Fatalf("bank holds %d facts", len(c.units))
	}

	queries := []string{
		"what is Alice into",
		"who moved to Lyon",
		"what did Dana do in Berlin last spring",
		"which friends did Priya meet in March 2022",
		"who has been doing pottery or woodworking",
		"what happened between May and July 2021",
		"Omar marathon",
		"tell me everything about Grace and Hiro and their hobbies like jazz, chess, climbing and astronomy",
	}
	ref := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	run := func(budget string) []time.Duration {
		var ds []time.Duration
		for rep := 0; rep < 5; rep++ {
			for _, q := range queries {
				s := time.Now()
				if _, err := h.e.Recall(context.Background(), "big", RecallRequest{Query: q, Budget: budget, QueryTimestamp: &ref}); err != nil {
					t.Fatal(err)
				}
				ds = append(ds, time.Since(s))
			}
		}
		sort.Slice(ds, func(a, b int) bool { return ds[a] < ds[b] })
		return ds
	}
	pct := func(ds []time.Duration, p float64) time.Duration { return ds[int(p*float64(len(ds)-1))] }
	var lines []string
	lines = append(lines, fmt.Sprintf("reindex cold %v, warm %v, cache load %v (dim %d)", cold.Round(time.Millisecond),
		warm.Round(time.Millisecond), load.Round(time.Millisecond), c.dim))
	for _, b := range []string{"low", "mid", "high"} {
		ds := run(b)
		lines = append(lines, fmt.Sprintf("recall budget=%s: p50 %v p95 %v max %v", b, pct(ds, 0.5).Round(100*time.Microsecond),
			pct(ds, 0.95).Round(100*time.Microsecond), ds[len(ds)-1].Round(100*time.Microsecond)))
	}
	for _, q := range queries {
		r, _ := h.e.Recall(context.Background(), "big", RecallRequest{Query: q, Trace: true, QueryTimestamp: &ref})
		lines = append(lines, fmt.Sprintf("  %-40.40q %v", q, r.Trace.TimingsMS))
	}
	out := strings.Join(lines, "\n")
	t.Log("\n" + out)
	if p := os.Getenv("GRIMOIRE_BANK_BENCH_OUT"); p != "" {
		_ = os.WriteFile(filepath.Clean(p), []byte(out+"\n"), 0o644)
	}
}
