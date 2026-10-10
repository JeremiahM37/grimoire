package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/bank"
	"github.com/JeremiahM37/grimoire/go/internal/index"
)

// The prefix block. Its contract is about bytes: the same memory gives the
// same bytes, a new memory only appends, a changed earlier memory is reported as
// a rewrite, and the budget only ever cuts the tail. The tests use explicit
// stamps so that "later" is a fact about the data, not about the wall clock.

func stamped(id, text, stamp string, mut func(*index.MemoryHit)) index.MemoryHit {
	h := hit(id, text, mut)
	h.Stamp = stamp
	return h
}

func prefixCorpus() []index.MemoryHit {
	var out []index.MemoryHit
	for i := 0; i < 40; i++ {
		out = append(out, stamped(fmt.Sprintf("f%02d", i),
			fmt.Sprintf("the service %02d listens on port %d and owns queue %02d", i, 8000+i, i),
			fmt.Sprintf("2026-10-01 %02d:%02d", i/60, i%60), nil))
	}
	return out
}

func TestPrefixIsByteIdenticalAcrossRepeatedCallsAndInputOrder(t *testing.T) {
	t.Parallel()
	hits := prefixCorpus()
	now := time.Now()
	a := buildPrefix(hits, now, prefixDefaultTokens)

	// Reversed input, and recall-style noise in the fields the block ignores.
	rev := make([]index.MemoryHit, len(hits))
	for i, h := range hits {
		rev[len(hits)-1-i] = h
		rev[len(hits)-1-i].Score = float64(i)
		rev[len(hits)-1-i].Uses = i
	}
	for round := 0; round < 3; round++ {
		b := buildPrefix(rev, now.Add(time.Duration(round)*time.Minute), prefixDefaultTokens)
		if b.Block != a.Block {
			t.Fatalf("round %d: block bytes differ from the first build", round)
		}
		if b.Version != a.Version || b.AppendOnlySince != a.AppendOnlySince {
			t.Fatalf("round %d: version or token differ", round)
		}
	}
	if !strings.HasPrefix(a.Block, prefixHeader) {
		t.Fatalf("block does not start with the fixed header:\n%s", a.Block)
	}
	if strings.Contains(a.Block, "Score") || strings.Contains(a.Block, "entries") {
		t.Fatalf("a volatile field leaked into the block:\n%s", a.Block)
	}
}

func TestPrefixAppendsWithoutMovingTheHead(t *testing.T) {
	t.Parallel()
	hits := prefixCorpus()
	now := time.Now()
	before := buildPrefix(hits, now, prefixDefaultTokens)

	added := stamped("zzNew", "the new deploy target is staging-9", "2026-10-09 09:00", nil)
	after := buildPrefix(append(append([]index.MemoryHit{}, hits...), added), now, prefixDefaultTokens)

	if !strings.HasPrefix(after.Block, before.Block) {
		t.Fatalf("adding an unrelated memory moved earlier bytes:\nbefore:\n%s\nafter:\n%s", before.Block, after.Block)
	}
	if after.Block == before.Block {
		t.Fatal("the new memory is missing from the block")
	}
	resp := after.respond("user", before.AppendOnlySince)
	if resp.Rewritten || resp.Unchanged {
		t.Fatalf("pure append reported rewritten=%v unchanged=%v", resp.Rewritten, resp.Unchanged)
	}
	if resp.Appended != 1 || resp.Delta != prefixLineFor(prefixEntry{ID: "zzNew", Stamp: "2026-10-09 09:00",
		Text: added.Text}) {
		t.Fatalf("delta = %q (appended %d), want only the new line", resp.Delta, resp.Appended)
	}
	if resp.Block != "" {
		t.Error("a delta response must not repeat the whole block")
	}
}

func TestPrefixSinceCurrentTokenIsUnchanged(t *testing.T) {
	t.Parallel()
	l := buildPrefix(prefixCorpus(), time.Now(), prefixDefaultTokens)
	for _, since := range []string{l.AppendOnlySince, l.Version} {
		resp := l.respond("user", since)
		if !resp.Unchanged || resp.Rewritten || resp.Delta != "" || resp.Appended != 0 {
			t.Errorf("since=%s: want an empty unchanged reply, got %+v", since, resp)
		}
	}
	empty := buildPrefix(nil, time.Now(), prefixDefaultTokens)
	if got := empty.respond("user", prefixGenesis); !got.Unchanged {
		t.Errorf("an empty log since its genesis must be unchanged, got %+v", got)
	}
	if got := buildPrefix(prefixCorpus(), time.Now(), prefixDefaultTokens).respond("user", prefixGenesis); got.Delta == "" {
		t.Error("a caller who has seen nothing must get the whole log as its delta")
	}
}

func TestPrefixReportsSupersessionAndEditsAsRewrites(t *testing.T) {
	t.Parallel()
	hits := prefixCorpus()
	now := time.Now()
	before := buildPrefix(hits, now, prefixDefaultTokens)

	// A newer fact supersedes f05: f05 leaves the log, and so does everything
	// the caller had after it, so the caller must not treat this as an append.
	superseded := append([]index.MemoryHit{}, hits...)
	for i := range superseded {
		if superseded[i].ID == "f05" {
			superseded[i].SupersededBy = "f99"
			superseded[i].SupersededAt = now.Format(time.RFC3339)
		}
	}
	superseded = append(superseded, stamped("f99", "the service 05 moved to port 9005", "2026-10-09 10:00", nil))
	after := buildPrefix(superseded, now, prefixDefaultTokens)
	if strings.Contains(after.Block, "[mem:f05]") {
		t.Fatal("a superseded fact is still in the block")
	}
	if resp := after.respond("user", before.AppendOnlySince); !resp.Rewritten || resp.Block != after.Block {
		t.Fatalf("supersession must report rewritten with the full block, got rewritten=%v", resp.Rewritten)
	}

	// An in-place edit of an earlier fact is the same kind of change.
	edited := prefixCorpus()
	edited[3].Text = "the service 03 listens on port 1"
	if resp := buildPrefix(edited, now, prefixDefaultTokens).respond("user", before.AppendOnlySince); !resp.Rewritten {
		t.Fatal("an edited earlier fact must report rewritten")
	}

	// A token the log never produced is also a rewrite, not a silent empty delta.
	if resp := before.respond("user", "not-a-real-token"); !resp.Rewritten || resp.Block == "" {
		t.Fatalf("an unknown token must report rewritten with the full block, got %+v", resp)
	}
}

func TestPrefixBudgetTruncatesFromTheEnd(t *testing.T) {
	t.Parallel()
	hits := prefixCorpus()
	now := time.Now()
	full := buildPrefix(hits, now, prefixMaxTokens)
	if full.Omitted() != 0 {
		t.Fatalf("the large budget should hold everything, omitted %d", full.Omitted())
	}

	for _, budget := range []int{prefixMinTokens, 120, 300} {
		l := buildPrefix(hits, now, budget)
		if l.Tokens > budget || bank.CountTokens(l.Block) > budget {
			t.Errorf("budget %d: block costs %d tokens", budget, l.Tokens)
		}
		if !strings.HasPrefix(full.Block, l.Block) {
			t.Errorf("budget %d: the truncated block is not a byte prefix of the full block", budget)
		}
		if !strings.HasSuffix(l.Block, "\n") {
			t.Errorf("budget %d: the block ends mid-line", budget)
		}
		if l.Omitted() == 0 {
			t.Errorf("budget %d: nothing was cut", budget)
		}
		if l.Entries[0].ID != "f00" {
			t.Errorf("budget %d: truncation must keep the head, first entry %s", budget, l.Entries[0].ID)
		}
	}

	// Growing the log past a full budget leaves the block byte-identical.
	small := buildPrefix(hits, now, 300)
	grown := append(append([]index.MemoryHit{}, hits...),
		stamped("zz1", "a late fact that will not fit", "2026-10-10 00:00", nil))
	if got := buildPrefix(grown, now, 300); got.Block != small.Block || got.Version != small.Version {
		t.Error("a fact that falls off the tail changed the block")
	}
	if resp := small.respond("user", small.AppendOnlySince); resp.Unchanged != true {
		t.Error("a caller at a full budget should be told nothing is visible yet")
	}
}

func TestPrefixSelection(t *testing.T) {
	t.Parallel()
	now := time.Now()
	hits := []index.MemoryHit{
		stamped("keep", "the team uses postgres", "2026-10-01 10:00", nil),
		stamped("person", "I want short replies", "2026-10-01 10:01", func(h *index.MemoryHit) {
			h.Human = true
			h.Importance = 1
		}),
		stamped("lowAgent", "the agent noticed a typo", "2026-10-01 10:02", func(h *index.MemoryHit) {
			h.Importance = 1
		}),
		stamped("superseded", "the team uses mysql", "2026-10-01 10:03", func(h *index.MemoryHit) {
			h.SupersededBy = "keep"
		}),
		stamped("disputed", "the team uses sqlite", "2026-10-01 10:04", func(h *index.MemoryHit) {
			h.Challenges = "keep"
		}),
		stamped("pulled", "the team uses a redis fork", "2026-10-01 10:05", func(h *index.MemoryHit) {
			h.Origin = "connector:web"
		}),
		stamped("expired", "the team uses oracle", "2026-10-01 10:06", func(h *index.MemoryHit) {
			h.Expires = now.Add(-time.Minute).UTC().Format(time.RFC3339)
		}),
	}
	l := buildPrefix(hits, now, prefixDefaultTokens)
	var got []string
	for _, e := range l.Entries {
		got = append(got, e.ID)
	}
	if strings.Join(got, ",") != "keep,person" {
		t.Fatalf("selection = %v, want the live facts in stamp order, human or not", got)
	}
}

func TestPrefixEndpoint(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "text": "the team uses postgres", "agent": "probe", "importance": 4})

	var first, second prefixResponse
	decode(t, do(t, h, "GET", "/api/memory/prefix", nil), &first)
	decode(t, do(t, h, "GET", "/api/memory/prefix", nil), &second)
	if first.Block == "" || !strings.Contains(first.Block, "the team uses postgres") {
		t.Fatalf("the durable fact is missing from the block:\n%s", first.Block)
	}
	if first.Block != second.Block || first.Version != second.Version {
		t.Fatal("two identical requests returned different bytes")
	}
	if first.Bytes != len(first.Block) || first.Tokens != bank.CountTokens(first.Block) {
		t.Errorf("bytes/tokens misreported: %d/%d", first.Bytes, first.Tokens)
	}

	var same prefixResponse
	decode(t, do(t, h, "GET", "/api/memory/prefix?since="+first.AppendOnlySince, nil), &same)
	if !same.Unchanged || same.Delta != "" || same.Block != "" {
		t.Errorf("since the current token want unchanged, got %+v", same)
	}

	remember(t, h, map[string]any{"topic": "prefs", "text": "the team deploys on fridays", "agent": "probe", "importance": 4})
	var grown prefixResponse
	decode(t, do(t, h, "GET", "/api/memory/prefix?since="+first.AppendOnlySince, nil), &grown)
	if grown.Rewritten || grown.Appended != 1 || !strings.Contains(grown.Delta, "deploys on fridays") {
		t.Errorf("a new fact should arrive as a one-line delta, got %+v", grown)
	}
	if grown.Block != "" {
		t.Error("a delta must not carry a block")
	}

	var stale prefixResponse
	decode(t, do(t, h, "GET", "/api/memory/prefix?since=bogus", nil), &stale)
	if !stale.Rewritten || stale.Block == "" {
		t.Errorf("an unknown since must come back rewritten with the block, got %+v", stale)
	}

	for _, bad := range []string{"max_tokens=5", "max_tokens=lots", "max_tokens=999999"} {
		if w := do(t, h, "GET", "/api/memory/prefix?"+bad, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, w.Code)
		}
	}
}
