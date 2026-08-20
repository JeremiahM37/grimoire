package index

import (
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// memNote writes a memory note whose bullets are the given facts, stamped a
// day apart so recency ordering is unambiguous.
func memNote(t *testing.T, ix *Index, rel string, entries ...memory.Entry) {
	t.Helper()
	body := "# Memory\n\n"
	for _, e := range entries {
		body += e.Format() + "\n"
	}
	write(t, ix, rel, body)
	if _, err := ix.Upsert(rel); err != nil {
		t.Fatal(err)
	}
}

func entry(id, stamp, text string) memory.Entry {
	return memory.Entry{ID: id, Stamp: stamp, Agent: "claude", Text: text}
}

func ids(hits []MemoryHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

func TestMemoryRowsAreDerivedFromBullets(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/prefs.md",
		entry("e1", "2026-08-14 09:00", "user prefers tabs"),
		entry("e2", "2026-08-15 09:00", "the server runs proxmox"))

	hits, err := ix.MemoryEntries(MemoryQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d entries, want 2: %v", len(hits), ids(hits))
	}
	if hits[0].Note != "memory/prefs.md" {
		t.Errorf("note = %q", hits[0].Note)
	}
	// Newest first with no query.
	if hits[0].ID != "e2" {
		t.Errorf("order = %v, want e2 first", ids(hits))
	}
}

func TestMemoryRowsRebuildOnEdit(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/prefs.md", entry("e1", "2026-08-14 09:00", "one"))
	memNote(t, ix, "memory/prefs.md",
		entry("e1", "2026-08-14 09:00", "one"),
		entry("e2", "2026-08-15 09:00", "two"))

	hits, _ := ix.MemoryEntries(MemoryQuery{Limit: 10})
	if len(hits) != 2 {
		t.Fatalf("rewrite did not re-derive rows: %v", ids(hits))
	}
	// And removing a bullet removes its row rather than leaving a ghost.
	memNote(t, ix, "memory/prefs.md", entry("e2", "2026-08-15 09:00", "two"))
	hits, _ = ix.MemoryEntries(MemoryQuery{Limit: 10})
	if len(hits) != 1 || hits[0].ID != "e2" {
		t.Fatalf("stale row survived a delete: %v", ids(hits))
	}
}

func TestMemoryRowsGoAwayWithTheNote(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/prefs.md", entry("e1", "2026-08-14 09:00", "one"))
	if err := ix.Remove("memory/prefs.md"); err != nil {
		t.Fatal(err)
	}
	hits, _ := ix.MemoryEntries(MemoryQuery{Limit: 10})
	if len(hits) != 0 {
		t.Fatalf("rows outlived their note: %v", ids(hits))
	}
}

func TestOnlyMemoryNotesProduceEntries(t *testing.T) {
	ix := testIndex(t)
	// The same bullet syntax in an ordinary note is a person's bullet list,
	// not agent memory, and must not become a fact.
	write(t, ix, "journal/2026-08-14.md",
		"# Journal\n\n"+entry("x1", "2026-08-14 09:00", "not a memory").Format()+"\n")
	if _, err := ix.Upsert("journal/2026-08-14.md"); err != nil {
		t.Fatal(err)
	}
	hits, _ := ix.MemoryEntries(MemoryQuery{Limit: 10})
	if len(hits) != 0 {
		t.Fatalf("a non-memory note produced entries: %v", ids(hits))
	}
}

func TestReindexRebuildsMemoryRows(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/prefs.md",
		entry("e1", "2026-08-14 09:00", "user prefers tabs"))
	if err := ix.DB.Exec("DELETE FROM memory_entries"); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	hits, _ := ix.MemoryEntries(MemoryQuery{Limit: 10})
	if len(hits) != 1 {
		t.Fatalf("reindex did not rebuild memory rows: %v", ids(hits))
	}
}

func TestMemoryQueryFilters(t *testing.T) {
	ix := testIndex(t)
	a := memory.Entry{ID: "a1", Stamp: "2026-08-14 09:00", Agent: "alice",
		Session: "run-1", Category: "preference", Text: "alice prefers tabs"}
	b := memory.Entry{ID: "b1", Stamp: "2026-08-15 09:00", Agent: "bob",
		Session: "run-2", Category: "fact", Text: "bob found the bug"}
	memNote(t, ix, "memory/team.md", a, b)

	cases := []struct {
		name string
		q    MemoryQuery
		want []string
	}{
		{"by agent", MemoryQuery{Agent: "alice"}, []string{"a1"}},
		{"by session", MemoryQuery{Session: "run-2"}, []string{"b1"}},
		{"by category", MemoryQuery{Category: "preference"}, []string{"a1"}},
		{"by id", MemoryQuery{ID: "b1"}, []string{"b1"}},
		{"by note", MemoryQuery{Note: "memory/team.md"}, []string{"b1", "a1"}},
		{"by missing agent", MemoryQuery{Agent: "carol"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hits, err := ix.MemoryEntries(c.q)
			if err != nil {
				t.Fatal(err)
			}
			got := ids(hits)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestMemoryExcludesSupersededAndExpiredByDefault(t *testing.T) {
	ix := testIndex(t)
	live := entry("live", "2026-08-14 09:00", "current belief")
	dead := memory.Entry{ID: "dead", Stamp: "2026-08-13 09:00", Agent: "claude",
		Text: "old belief", SupersededBy: "live"}
	gone := memory.Entry{ID: "gone", Stamp: "2026-08-13 09:00", Agent: "claude",
		Text: "temporary", Expires: "2026-08-14T00:00:00Z"}
	memNote(t, ix, "memory/prefs.md", live, dead, gone)

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	hits, _ := ix.MemoryEntries(MemoryQuery{Now: now, Limit: 10})
	if len(hits) != 1 || hits[0].ID != "live" {
		t.Fatalf("default recall returned %v, want [live]", ids(hits))
	}

	hits, _ = ix.MemoryEntries(MemoryQuery{Now: now, Limit: 10, IncludeSuperseded: true})
	if len(hits) != 2 {
		t.Fatalf("IncludeSuperseded returned %v, want live+dead", ids(hits))
	}
	hits, _ = ix.MemoryEntries(MemoryQuery{Now: now, Limit: 10, IncludeExpired: true})
	if len(hits) != 2 {
		t.Fatalf("IncludeExpired returned %v, want live+gone", ids(hits))
	}
}

func TestMemoryExpiryIsEvaluatedAtQueryTime(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/prefs.md", memory.Entry{ID: "ttl",
		Stamp: "2026-08-14 09:00", Agent: "claude", Text: "on call this week",
		Expires: "2026-08-21T00:00:00Z"})

	before := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	if hits, _ := ix.MemoryEntries(MemoryQuery{Now: before}); len(hits) != 1 {
		t.Fatal("entry expired before its expiry")
	}
	after := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	if hits, _ := ix.MemoryEntries(MemoryQuery{Now: after}); len(hits) != 0 {
		t.Fatal("entry outlived its expiry")
	}
}

func TestMemoryRankingUsesKeywordSignal(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/facts.md",
		entry("kw", "2026-08-14 09:00", "the deploy script lives at /usr/local/bin/deploy.sh"),
		entry("other", "2026-08-15 09:00", "the cat is named marmalade"))

	hits, err := ix.MemoryEntries(MemoryQuery{Query: "deploy script", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].ID != "kw" {
		t.Fatalf("keyword query ranked %v first", ids(hits))
	}
	if hits[0].Keyword == 0 {
		t.Error("keyword component did not contribute")
	}
}

func TestMemoryRankingUsesEntitySignal(t *testing.T) {
	ix := testIndex(t)
	// Both facts are lexically close to the query except for the name, so the
	// entity signal is what has to separate them.
	memNote(t, ix, "memory/team.md",
		entry("priya", "2026-08-14 09:00", "Priya Sharma owns the release checklist"),
		entry("marco", "2026-08-15 09:00", "Marco Diaz owns the release checklist"))

	hits, err := ix.MemoryEntries(MemoryQuery{Query: "what does Priya own", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].ID != "priya" {
		t.Fatalf("entity query ranked %v first", ids(hits))
	}
	if hits[0].Entity == 0 {
		t.Error("entity component did not contribute")
	}
}

func TestMemoryRankingBreaksTiesByRecency(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/facts.md",
		entry("old", "2026-01-14 09:00", "the backup runs nightly"),
		entry("new", "2026-08-15 09:00", "the backup runs nightly"))

	hits, _ := ix.MemoryEntries(MemoryQuery{Query: "backup", Limit: 5,
		Now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.Local)})
	if len(hits) < 2 {
		t.Fatalf("want both, got %v", ids(hits))
	}
	if hits[0].ID != "new" {
		t.Errorf("identical facts not ordered by recency: %v", ids(hits))
	}
	if hits[0].Recency <= hits[1].Recency {
		t.Errorf("recency component did not decay: %v vs %v", hits[0].Recency, hits[1].Recency)
	}
}

func TestMemoryScoreComponentsAreReported(t *testing.T) {
	// The explain surface is only as good as the components it can show.
	ix := testIndex(t)
	memNote(t, ix, "memory/facts.md",
		entry("e1", "2026-08-14 09:00", "Priya owns the deploy script"))
	hits, _ := ix.MemoryEntries(MemoryQuery{Query: "who owns the deploy script", Limit: 5})
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(hits))
	}
	h := hits[0]
	if h.Score <= 0 {
		t.Errorf("score = %v", h.Score)
	}
	sum := wSemantic*h.Semantic + wKeyword*h.Keyword + wEntity*h.Entity + wRecency*h.Recency
	if diff := h.Score - sum; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("score %v is not the weighted sum of its parts %v", h.Score, sum)
	}
	for name, v := range map[string]float64{"semantic": h.Semantic,
		"keyword": h.Keyword, "entity": h.Entity, "recency": h.Recency} {
		if v < 0 || v > 1 {
			t.Errorf("%s component out of range: %v", name, v)
		}
	}
}

func TestMemoryLimitApplies(t *testing.T) {
	ix := testIndex(t)
	var es []memory.Entry
	for i := 0; i < 10; i++ {
		es = append(es, entry(string(rune('a'+i)), "2026-08-14 09:0"+string(rune('0'+i)), "fact number"))
	}
	memNote(t, ix, "memory/many.md", es...)
	hits, _ := ix.MemoryEntries(MemoryQuery{Limit: 3})
	if len(hits) != 3 {
		t.Fatalf("limit ignored: got %d", len(hits))
	}
	hits, _ = ix.MemoryEntries(MemoryQuery{Query: "fact", Limit: 3})
	if len(hits) != 3 {
		t.Fatalf("limit ignored on a ranked query: got %d", len(hits))
	}
}

func TestMemoryEntitiesTableIsPopulated(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/team.md",
		entry("e1", "2026-08-14 09:00", "Priya Sharma runs the deploy on AIServer"))
	rows, err := ix.DB.Query("SELECT entity FROM memory_entities WHERE id='e1'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	if len(got) == 0 {
		t.Fatal("no entities recorded")
	}
	if !strings.Contains(strings.Join(got, ","), "priya sharma") {
		t.Errorf("entities = %v, want priya sharma", got)
	}
}

func TestMemoryPrivateEntriesNeedOptIn(t *testing.T) {
	ix := testIndex(t)
	fm := markdown.NewFrontmatter()
	fm.Set("private", true)
	body := "# Memory\n\n" + entry("p1", "2026-08-14 09:00", "a private fact").Format() + "\n"
	if _, err := ix.Vault.Write("memory/private.md", body, fm); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Upsert("memory/private.md"); err != nil {
		t.Fatal(err)
	}
	if hits, _ := ix.MemoryEntries(MemoryQuery{Limit: 10}); len(hits) != 0 {
		t.Fatalf("private memory returned without opt-in: %v", ids(hits))
	}
	hits, _ := ix.MemoryEntries(MemoryQuery{Limit: 10,
		Filter: Filter{IncludePrivate: true}})
	if len(hits) != 1 {
		t.Fatalf("IncludePrivate did not return the entry: %v", ids(hits))
	}
}

func TestIsMemoryPath(t *testing.T) {
	for path, want := range map[string]bool{
		"memory/a.md":     true,
		"memory/sub/b.md": true,
		"memories/a.md":   false,
		"a/memory/b.md":   false,
		"memory.md":       false,
		"journal/2026.md": false,
	} {
		if got := IsMemoryPath(path); got != want {
			t.Errorf("IsMemoryPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestMemoryAsOfReturnsWhatWasBelievedThen(t *testing.T) {
	ix := testIndex(t)
	old := memory.Entry{ID: "old", Stamp: "2026-08-10 09:00", Agent: "claude",
		Text: "the user prefers spaces", SupersededBy: "new",
		SupersededAt: "2026-08-15 09:00"}
	current := entry("new", "2026-08-15 09:00", "the user prefers tabs")
	later := entry("later", "2026-08-18 09:00", "the office moved floors")
	memNote(t, ix, "memory/prefs.md", old, current, later)

	at := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	cases := []struct {
		when string
		want []string
	}{
		{"2026-08-09 09:00", nil},                      // before anything was written
		{"2026-08-12 09:00", []string{"old"}},          // while the old belief stood
		{"2026-08-16 09:00", []string{"new"}},          // after it was replaced
		{"2026-08-19 09:00", []string{"later", "new"}}, // newest first
	}
	for _, c := range cases {
		hits, err := ix.MemoryEntries(MemoryQuery{AsOf: at(c.when), Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		got := ids(hits)
		if len(got) != len(c.want) {
			t.Errorf("as of %s: got %v, want %v", c.when, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("as of %s: got %v, want %v", c.when, got, c.want)
				break
			}
		}
	}
}

func TestMemoryAsOfExcludesFactsExpiredByThen(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/oncall.md", memory.Entry{ID: "ttl",
		Stamp: "2026-08-10 09:00", Agent: "claude", Text: "priya is on call",
		Expires: "2026-08-14T00:00:00Z"})

	before, _ := time.Parse(time.RFC3339, "2026-08-12T00:00:00Z")
	after, _ := time.Parse(time.RFC3339, "2026-08-16T00:00:00Z")
	if hits, _ := ix.MemoryEntries(MemoryQuery{AsOf: before, Limit: 10}); len(hits) != 1 {
		t.Error("a fact was missing from the window it was true in")
	}
	if hits, _ := ix.MemoryEntries(MemoryQuery{AsOf: after, Limit: 10}); len(hits) != 0 {
		t.Error("an already-expired fact appeared in a later historical view")
	}
}

func TestMemoryFilterByTask(t *testing.T) {
	// task is free-form provenance — a ticket id, or the key an external
	// store addressed the fact by — so it has to be selectable exactly.
	ix := testIndex(t)
	a := memory.Entry{ID: "a1", Stamp: "2026-08-14 09:00", Agent: "claude",
		Task: "ticket-4", Text: "the first fact"}
	b := memory.Entry{ID: "b1", Stamp: "2026-08-15 09:00", Agent: "claude",
		Task: "ticket-9", Text: "the second fact"}
	memNote(t, ix, "memory/work.md", a, b)

	hits, err := ix.MemoryEntries(MemoryQuery{Task: "ticket-9", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "b1" {
		t.Fatalf("task filter returned %v", ids(hits))
	}
	if hits, _ := ix.MemoryEntries(MemoryQuery{Task: "ticket-none"}); len(hits) != 0 {
		t.Errorf("unknown task returned %v", ids(hits))
	}
}

func TestDuplicateBulletsDoNotBreakIndexing(t *testing.T) {
	// A note holding the same bullet twice is exactly what consolidation
	// exists to clean up — so it has to be indexable. Identical bullets derive
	// identical ids, and rejecting the second one failed the whole note write.
	ix := testIndex(t)
	// A legacy pair: no trailer, so both derive the same id from their content.
	legacy := "- **2026-08-14 09:00 · claude** — force-recreate after any VPN change"
	write(t, ix, "memory/ops.md", "# Memory\n\n"+legacy+"\n"+legacy+"\n")
	if _, err := ix.Upsert("memory/ops.md"); err != nil {
		t.Fatalf("indexing a note with a duplicate bullet failed: %v", err)
	}
	hits, err := ix.MemoryEntries(MemoryQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	// One fact, not two: the same stamp, agent and text is one belief stated
	// twice, whatever the file looks like.
	if len(hits) != 1 {
		t.Fatalf("got %d entries for one duplicated fact: %v", len(hits), ids(hits))
	}
}
