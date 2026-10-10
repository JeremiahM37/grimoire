package index

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Importance, usage and eviction. The rules here are small and each one is a
// promise to a person reading the vault: unrated facts rank exactly as before,
// a fact a person wrote is never evicted, and use is counted without writing
// anything into the note.

var fixedNow = time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)

// daysAgo is a bullet stamp that many days before fixedNow.
func daysAgo(d int) string {
	return fixedNow.Add(-time.Duration(d) * 24 * time.Hour).Format(memory.StampFormat)
}

func rated(id, stamp, text string, imp int) memory.Entry {
	e := entry(id, stamp, text)
	e.Importance = imp
	return e
}

func hitByID(hits []MemoryHit, id string) (MemoryHit, bool) {
	for _, h := range hits {
		if h.ID == id {
			return h, true
		}
	}
	return MemoryHit{}, false
}

// legacyScore is the ranking score as it was before importance existed,
// recomputed from the components a hit reports. Used as the baseline.
func legacyScore(h MemoryHit, now time.Time) float64 {
	return wSemantic*h.Semantic + wKeyword*h.Keyword + wEntity*h.Entity +
		wRecency*recencyScore(h.Stamp, now) + wUseful*h.Useful
}

func TestUnratedRankingMatchesThePreImportanceBaseline(t *testing.T) {
	ix := testIndex(t)
	// Unrated agent facts only: the store as it was before this feature.
	memNote(t, ix, "memory/ops.md",
		entry("u1", daysAgo(1), "the deploy host is prod-1"),
		entry("u2", daysAgo(30), "deploy the server with make release"),
		entry("u3", daysAgo(200), "the deploy window is thursday"),
		entry("u4", daysAgo(5), "the server runs proxmox"),
		entry("u5", daysAgo(90), "deploy keys live in the vault"),
		entry("u6", daysAgo(400), "unrelated note about tea"))

	hits, err := ix.MemoryEntries(MemoryQuery{Query: "deploy server", Now: fixedNow, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 6 {
		t.Fatalf("got %d hits, want 6", len(hits))
	}
	for _, h := range hits {
		if h.ImportanceFactor != 1 {
			t.Errorf("%s: importance factor = %v, want exactly 1", h.ID, h.ImportanceFactor)
		}
		if h.Reuse != 0 {
			t.Errorf("%s: reuse = %v, want 0", h.ID, h.Reuse)
		}
		if want := legacyScore(h, fixedNow); h.Score != want {
			t.Errorf("%s: score = %.17g, baseline %.17g", h.ID, h.Score, want)
		}
	}
	// The order must be the order the baseline formula gives, ties included.
	base := append([]MemoryHit(nil), hits...)
	for i := range base {
		base[i].Score = legacyScore(base[i], fixedNow)
	}
	for i := 1; i < len(base); i++ {
		a, b := base[i-1], base[i]
		if a.Score < b.Score || (a.Score == b.Score && a.Stamp < b.Stamp) {
			t.Fatalf("baseline order broken at %d: %s (%.6f) before %s (%.6f)", i, a.ID, a.Score, b.ID, b.Score)
		}
	}
	for i := range hits {
		if hits[i].ID != base[i].ID {
			t.Fatalf("order changed at %d: got %s, baseline %s", i, hits[i].ID, base[i].ID)
		}
	}
}

func TestImportanceFactorIsNeutralAtThreeAndBounded(t *testing.T) {
	if importanceFactor(3) != 1 {
		t.Fatalf("factor at 3 = %v, want exactly 1", importanceFactor(3))
	}
	if math.Abs(importanceFactor(1)-0.8) > 1e-12 || math.Abs(importanceFactor(5)-1.2) > 1e-12 {
		t.Errorf("factor(1)=%v factor(5)=%v, want 0.8 and 1.2", importanceFactor(1), importanceFactor(5))
	}
	for imp := 1; imp <= 5; imp++ {
		if f := importanceFactor(imp); f < 0.8-1e-12 || f > 1.2+1e-12 {
			t.Errorf("factor(%d) = %v escapes the 0.8..1.2 bound", imp, f)
		}
	}
}

func TestImportanceStretchesOrShrinksRecency(t *testing.T) {
	if halfLifeFor(5) != 2*recencyHalfLife || halfLifeFor(4) != 2*recencyHalfLife {
		t.Errorf("important facts must double the half-life")
	}
	if halfLifeFor(1) != recencyHalfLife/2 || halfLifeFor(2) != recencyHalfLife/2 {
		t.Errorf("trivial facts must halve the half-life")
	}
	if halfLifeFor(3) != recencyHalfLife {
		t.Errorf("neutral facts must keep the base half-life")
	}

	ix := testIndex(t)
	memNote(t, ix, "memory/ops.md",
		rated("big", daysAgo(180), "the deploy window is thursday", 5),
		rated("tiny", daysAgo(180), "deploy window note from before", 1),
		entry("plain", daysAgo(180), "deploy window note from before"))
	hits, err := ix.MemoryEntries(MemoryQuery{Query: "deploy window", Now: fixedNow, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	big, _ := hitByID(hits, "big")
	tiny, _ := hitByID(hits, "tiny")
	plain, _ := hitByID(hits, "plain")
	if !(big.Recency > plain.Recency && plain.Recency > tiny.Recency) {
		t.Errorf("recency at 180 days: important %.4f, unrated %.4f, trivial %.4f; want strictly decreasing",
			big.Recency, plain.Recency, tiny.Recency)
	}
}

func TestHigherImportanceWinsAnOtherwiseEqualTie(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/ops.md",
		rated("lo", daysAgo(3), "the backup runs at night", 1),
		rated("hi", daysAgo(3), "the backup runs at night", 5))
	hits, err := ix.MemoryEntries(MemoryQuery{Query: "backup night", Now: fixedNow, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(hits); got[0] != "hi" {
		t.Errorf("order = %v, want the importance-5 fact first", got)
	}
}

func TestHumanAuthoredUnratedFactsRankAsFour(t *testing.T) {
	human := entry("h1", daysAgo(3), "the backup runs at night")
	human.Human = true
	agent := entry("a1", daysAgo(3), "the backup runs at night")

	if got := human.EffectiveImportance(); got != 4 {
		t.Errorf("human unrated effective importance = %d, want 4", got)
	}
	if got := agent.EffectiveImportance(); got != 3 {
		t.Errorf("agent unrated effective importance = %d, want 3", got)
	}
	// A person's correction with no trailer at all is still a person's.
	raw := "- **" + daysAgo(3) + " · claude** — the backup runs at night"
	parsed, ok := memory.ParseLine(raw)
	if !ok || parsed.EffectiveImportance() != 4 {
		t.Errorf("trailer-less bullet effective importance = %d, want 4", parsed.EffectiveImportance())
	}
	// Declared importance always wins over the human default.
	declared := human
	declared.Importance = 2
	if declared.EffectiveImportance() != 2 {
		t.Errorf("declared importance must override the human default")
	}

	ix := testIndex(t)
	memNote(t, ix, "memory/ops.md", agent, human)
	hits, err := ix.MemoryEntries(MemoryQuery{Query: "backup night", Now: fixedNow, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(hits); got[0] != "h1" {
		t.Errorf("order = %v, want the person's fact outranking the agent's", got)
	}
}

func TestImportanceRoundTripsAndOldBulletsDoNotChange(t *testing.T) {
	// A bullet an older build wrote, with a trailer that matches its own text.
	stamp, agent, text := "2026-08-14 09:00", "claude", "prefers tabs"
	id := memory.DeriveID(stamp, agent, text)
	old := "- **" + stamp + " · " + agent + "** — " + text + " <!--m id=" + id + " cat=preference-->"
	e, ok := memory.ParseLine(old)
	if !ok {
		t.Fatal("old bullet did not parse")
	}
	if e.Importance != 0 {
		t.Errorf("a bullet without imp= parsed as importance %d, want unrated", e.Importance)
	}
	if got := e.Format(); got != old {
		t.Errorf("old bullet changed on round trip:\n got %q\nwant %q", got, old)
	}

	rated := e
	rated.Importance = 5
	line := rated.Format()
	if !strings.Contains(line, "imp=5") {
		t.Fatalf("rated bullet lacks imp=5: %q", line)
	}
	back, _ := memory.ParseLine(line)
	if back.Importance != 5 || back.Format() != line {
		t.Errorf("rated bullet did not round trip: %q -> %d", line, back.Importance)
	}

	// Out-of-range is malformed hand-editing: it parses as unrated and is not
	// stored, rather than making the whole bullet unreadable.
	bad, ok := memory.ParseLine(strings.Replace(old, "cat=preference", "cat=preference imp=9", 1))
	if !ok || bad.Importance != 0 {
		t.Errorf("imp=9 parsed as %d, want unrated", bad.Importance)
	}
}

func TestImportanceIsIndexedAndSurvivesReindex(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/ops.md", rated("r1", daysAgo(2), "the deploy host is prod-1", 5))
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.MemoryEntries(MemoryQuery{Note: "memory/ops.md", Now: fixedNow, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Importance != 5 || !hits[0].Live(fixedNow) {
		t.Fatalf("after reindex: %+v", hits)
	}
}

func TestRecordUseCountsWithoutTouchingTheMarkdown(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/ops.md",
		entry("used", daysAgo(3), "the deploy host is prod-1"),
		entry("fresh", daysAgo(3), "the deploy host is prod-1 too"))
	before, err := os.ReadFile(filepath.Join(ix.Vault.Root, "memory", "ops.md"))
	if err != nil {
		t.Fatal(err)
	}

	at := fixedNow
	for i := 0; i < 3; i++ {
		if err := ix.RecordUse("memory/ops.md", "used", at); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(filepath.Join(ix.Vault.Root, "memory", "ops.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("recording use changed the markdown:\n%s", after)
	}

	hits, err := ix.MemoryEntries(MemoryQuery{Query: "deploy host", Now: fixedNow, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	used, _ := hitByID(hits, "used")
	if used.Uses != 3 || used.LastUsed == "" {
		t.Errorf("uses = %d, last_used = %q; want 3 and set", used.Uses, used.LastUsed)
	}
	if used.Reuse <= 0 {
		t.Errorf("used fact earned no reuse bonus")
	}
	if got := ids(hits); got[0] != "used" {
		t.Errorf("order = %v, want the used fact first on a tie", got)
	}
}

func TestUseSurvivesRewritesOfItsNote(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/ops.md", entry("used", daysAgo(3), "the deploy host is prod-1"))
	if err := ix.RecordUse("memory/ops.md", "used", fixedNow); err != nil {
		t.Fatal(err)
	}
	// Another fact lands in the same note: the whole note's rows are rewritten.
	memNote(t, ix, "memory/ops.md",
		entry("used", daysAgo(3), "the deploy host is prod-1"),
		entry("other", daysAgo(1), "the cache is redis"))
	hits, err := ix.MemoryEntries(MemoryQuery{Note: "memory/ops.md", Now: fixedNow, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if used, _ := hitByID(hits, "used"); used.Uses != 1 {
		t.Errorf("uses after a rewrite = %d, want 1", used.Uses)
	}
}

func TestReuseBonusIsZeroUnusedAndBounded(t *testing.T) {
	if reuseBonus(0) != 0 || reuseBonus(-1) != 0 {
		t.Fatal("an unused fact must earn no bonus")
	}
	prev := 0.0
	for n := 1; n <= 50; n++ {
		b := reuseBonus(n)
		if b <= prev {
			t.Fatalf("bonus not increasing at %d uses", n)
		}
		if b >= wReuse {
			t.Fatalf("bonus %v at %d uses reaches the cap %v", b, n, wReuse)
		}
		prev = b
	}
}

func TestPruneCandidatesNeverTakeAProtectedFact(t *testing.T) {
	ix := testIndex(t)
	old := daysAgo(200)
	recent := daysAgo(10)
	// The only candidate: agent-written, low, unused, and older than the cutoff.
	low := rated("low", old, "the cache is redis", 1)
	human := rated("human", old, "the cache is memcached", 1)
	human.Human = true
	immutable := rated("immutable", old, "the cache must stay redis", 1)
	immutable.Immutable = true
	important := rated("important", old, "the database is postgres", 4)
	critical := rated("critical", old, "the backup key is in the vault", 5)
	unrated := entry("unrated", old, "the mailer is postfix")
	recentLow := rated("recentlow", recent, "the queue is rabbit", 1)
	helpful := rated("helpful", old, "the proxy is caddy", 1)
	helpful.Helpful = 1
	challenged := rated("challenged", old, "the dns is unbound", 1)
	challenged.Challenges = "low"
	superseded := rated("superseded", old, "the old host is pi", 1)
	superseded.SupersededBy = "low"
	superseded.SupersededAt = old
	memNote(t, ix, "memory/ops.md", low, human, immutable, important, critical,
		unrated, recentLow, helpful, challenged, superseded)
	// A hand-written bullet: no trailer at all, so a person typed it.
	write(t, ix, "memory/hand.md", "# Memory\n\n- **"+old+" · claude** — the search is meili <!--m id=zzz-->\n- **"+old+" · claude** — the wiki is outline\n")
	if _, err := ix.Upsert("memory/hand.md"); err != nil {
		t.Fatal(err)
	}
	// A used fact: counted by recall, not in the file.
	memNote(t, ix, "memory/used.md", rated("used", old, "the logs go to loki", 1))
	if err := ix.RecordUse("memory/used.md", "used", fixedNow); err != nil {
		t.Fatal(err)
	}

	cutoff := fixedNow.Add(-90 * 24 * time.Hour)
	got, err := ix.PruneCandidates(2, cutoff, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "low" {
		var all []string
		for _, h := range got {
			all = append(all, h.Note+":"+h.ID)
		}
		t.Fatalf("candidates = %v, want only memory/ops.md:low", all)
	}
}

func TestPruneRespectsTheAgeCutoffAndTheLimit(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/ops.md",
		rated("a", daysAgo(120), "the mailer is postfix", 1),
		rated("b", daysAgo(100), "the cache is redis", 2),
		rated("c", daysAgo(89), "the queue is rabbit", 1),
		rated("d", daysAgo(300), "the proxy is caddy", 3))
	cutoff := fixedNow.Add(-90 * 24 * time.Hour)

	got, err := ix.PruneCandidates(2, cutoff, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got); n != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("candidates = %v, want [a b] (oldest first, 89 days excluded)", ids(got))
	}
	// Explicit 3 is only a candidate when the threshold reaches it.
	if got, _ := ix.PruneCandidates(2, cutoff, 100); len(got) != 2 {
		t.Fatalf("threshold 2 admitted an importance-3 fact")
	}
	if got, _ := ix.PruneCandidates(3, cutoff, 100); len(got) != 3 {
		t.Fatalf("threshold 3 candidates = %d, want 3", len(got))
	}
	if got, _ := ix.PruneCandidates(2, cutoff, 1); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("limit 1 = %v, want the oldest only", ids(got))
	}
	// Thresholds at or above neutral are clamped: a 4 or 5 is never a candidate.
	if got, _ := ix.PruneCandidates(5, cutoff, 100); len(got) != 3 {
		t.Fatalf("threshold 5 candidates = %d, want 3 (clamped to 3)", len(got))
	}
}
