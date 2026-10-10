package index

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// memNoteFM is memNote with frontmatter, for notes that carry a reader list or
// are private.
func memNoteFM(t *testing.T, ix *Index, rel string, fm func(*markdown.Frontmatter), entries ...memory.Entry) {
	t.Helper()
	body := "# Memory\n\n"
	for _, e := range entries {
		body += e.Format() + "\n"
	}
	f := markdown.NewFrontmatter()
	if fm != nil {
		fm(f)
	}
	if _, err := ix.Vault.Write(rel, body, f); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Upsert(rel); err != nil {
		t.Fatal(err)
	}
}

var expandNow = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

func expandedIDs(hits []ExpandedHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

func hitByID(t *testing.T, hits []ExpandedHit, id string) ExpandedHit {
	t.Helper()
	for _, h := range hits {
		if h.ID == id {
			return h
		}
	}
	t.Fatalf("%s not in %v", id, expandedIDs(hits))
	return ExpandedHit{}
}

func TestRecallExpandedOffIsPlainRecall(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/team.md",
		entry("priya", "2026-08-14 09:00", "Priya Sharma owns the release checklist"),
		entry("marco", "2026-08-15 09:00", "Marco Diaz owns the release checklist"),
		entry("disk", "2026-08-16 09:00", "the NAS disk is at 80 percent"))
	q := MemoryQuery{Query: "what does Priya own", Limit: 5, Now: expandNow}

	plain, err := ix.MemoryEntries(q)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ix.RecallExpanded(q, ExpandOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(plain) {
		t.Fatalf("off returned %d, plain %d", len(got), len(plain))
	}
	for i := range plain {
		if got[i].MemoryHit != plain[i] {
			t.Errorf("hit %d differs: %+v vs %+v", i, got[i].MemoryHit, plain[i])
		}
		if got[i].Variants != nil || got[i].Via != "" || got[i].Hop != 0 || got[i].Fused != 0 {
			t.Errorf("hit %d carries expansion state with the stages off: %+v", i, got[i])
		}
	}
}

// The keyword case: the query says "deploying", the fact says "deploy". No
// literal term matches, so the literal ranking puts the fact below the
// distractors that share "proxy" and a small limit drops it. The stem variant
// searches "deploy" and finds it.
func TestExpansionFindsAStemMissTheLiteralQueryMisses(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/ops.md",
		entry("deploy", "2026-08-10 09:00", "deploy failed on staging host"),
		entry("p1", "2026-08-14 09:00", "proxy config lives in nginx"),
		entry("p2", "2026-08-15 09:00", "proxy certs renew monthly"),
		entry("p3", "2026-08-16 09:00", "proxy listens on 443"))
	q := MemoryQuery{Query: "deploying proxy", Limit: 2, Now: expandNow}

	plain, _ := ix.MemoryEntries(q)
	for _, h := range plain {
		if h.ID == "deploy" {
			t.Fatalf("literal query already finds the stem fact; test premise broken: %v", expandedIDs(toExpanded(plain)))
		}
	}
	got, err := ix.RecallExpanded(q, ExpandOptions{Expand: true})
	if err != nil {
		t.Fatal(err)
	}
	h := hitByID(t, got, "deploy")
	if !hasVariant(h.Variants, VariantKeyword) && !hasVariant(h.Variants, VariantAlias) {
		t.Errorf("stem fact variants = %v, want a keyword or alias variant", h.Variants)
	}
	if h.Fused <= 0 || h.Score <= 0 || h.Score > 1 {
		t.Errorf("fused=%v score=%v, want fused>0 and score in (0,1]", h.Fused, h.Score)
	}
}

// The alias case: "Dana" alone is the query. The fact is about "Dana Kim", the
// newer distractors are about "Dana", and the limit is two. Only the alias
// variant ("dana kim") gives the target a second term to match.
func TestExpansionFindsAnAliasTheLiteralQueryMisses(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/team.md",
		entry("kim", "2026-08-10 09:00", "Dana Kim owns the billing cutover"),
		entry("async1", "2026-08-14 09:00", "Dana prefers async standups"),
		entry("async2", "2026-08-15 09:00", "Dana prefers written updates"),
		entry("async3", "2026-08-16 09:00", "Dana prefers short meetings"))
	q := MemoryQuery{Query: "Dana", Limit: 2, Now: expandNow}

	plain, _ := ix.MemoryEntries(q)
	for _, h := range plain {
		if h.ID == "kim" {
			t.Fatalf("literal query already finds the alias fact: %v", expandedIDs(toExpanded(plain)))
		}
	}
	got, err := ix.RecallExpanded(q, ExpandOptions{Expand: true})
	if err != nil {
		t.Fatal(err)
	}
	h := hitByID(t, got, "kim")
	if !hasVariant(h.Variants, VariantAlias) {
		t.Errorf("alias fact variants = %v, want %s", h.Variants, VariantAlias)
	}
}

func TestEntityAliasesFollowTheBankRule(t *testing.T) {
	got := memory.EntityAliases([]string{"dana", "dana kim", "priya", "priya sharma", "priya lee", "marco"})
	if _, ok := got["dana"]; !ok || got["dana"] != "dana kim" {
		t.Errorf("dana -> %q, want dana kim", got["dana"])
	}
	if _, ok := got["priya"]; ok {
		t.Errorf("priya is ambiguous (two full names) and must not alias: %v", got)
	}
	if _, ok := got["marco"]; ok {
		t.Errorf("marco has no full name and must not alias: %v", got)
	}
}

func TestGraphWalkReachesAOneHopNeighbour(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/team.md",
		entry("seed", "2026-08-16 09:00", "Priya Sharma owns the release checklist"),
		entry("nbr", "2026-08-10 09:00", "Priya Sharma moved the strix box to Lane Office"),
		entry("far", "2026-08-11 09:00", "the proxy listens on 443"))

	got, err := ix.RecallExpanded(MemoryQuery{Query: "release checklist", Limit: 1, Now: expandNow},
		ExpandOptions{Hops: 1})
	if err != nil {
		t.Fatal(err)
	}
	n := hitByID(t, got, "nbr")
	if n.Via != "graph" || n.Hop != 1 || n.Connect != "priya sharma" {
		t.Errorf("neighbour via=%q hop=%d connect=%q, want graph/1/priya sharma", n.Via, n.Hop, n.Connect)
	}
	seed := hitByID(t, got, "seed")
	if want := 0.5 * seed.Score; n.Score != want {
		t.Errorf("neighbour score = %v, want 0.5 x source %v = %v", n.Score, seed.Score, want)
	}
	for _, h := range got {
		if h.ID == "far" {
			t.Errorf("unconnected fact was added by the walk: %v", expandedIDs(got))
		}
	}
}

func TestGraphWalkTwoHopsDecaysAgain(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/team.md",
		entry("seed", "2026-08-16 09:00", "Priya Sharma owns the release checklist"),
		entry("hop1", "2026-08-12 09:00", "Priya Sharma moved the strix box to Lane Office"),
		entry("hop2", "2026-08-10 09:00", "Lane Office cooling was replaced in March"))

	got, _ := ix.RecallExpanded(MemoryQuery{Query: "release checklist", Limit: 1, Now: expandNow},
		ExpandOptions{Hops: 2})
	h2 := hitByID(t, got, "hop2")
	if h2.Hop != 2 || h2.Connect != "lane office" {
		t.Fatalf("two-hop fact hop=%d connect=%q, want 2/lane office", h2.Hop, h2.Connect)
	}
	seed := hitByID(t, got, "seed")
	if want := 0.25 * seed.Score; h2.Score != want {
		t.Errorf("two-hop score = %v, want 0.25 x source = %v", h2.Score, want)
	}
	// With one hop the two-hop fact must not appear.
	one, _ := ix.RecallExpanded(MemoryQuery{Query: "release checklist", Limit: 1, Now: expandNow},
		ExpandOptions{Hops: 1})
	for _, h := range one {
		if h.ID == "hop2" {
			t.Errorf("hops=1 reached a two-hop fact: %v", expandedIDs(one))
		}
	}
}

// Graph-added facts must pass exactly the filters a direct hit passes. Each
// case plants a neighbour that the caller must NOT see and checks it stays
// hidden, and that the same neighbour appears for a caller who may see it.
func TestGraphWalkRespectsTheAccessFilters(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/team.md",
		entry("seed", "2026-08-20 09:00", "Priya Sharma owns the release checklist"))
	memNoteFM(t, ix, "memory/acl.md", func(f *markdown.Frontmatter) { f.Set("readers", "bob") },
		entry("acl", "2026-08-10 09:00", "Priya Sharma has the vault key rotation"))
	memNoteFM(t, ix, "memory/priv.md", func(f *markdown.Frontmatter) { f.Set("private", true) },
		entry("priv", "2026-08-10 09:00", "Priya Sharma moved the backups"))
	memNote(t, ix, "memory/old.md",
		memory.Entry{ID: "sup", Stamp: "2026-08-10 09:00", Agent: "claude",
			Text: "Priya Sharma used the old pager", SupersededBy: "new"},
		entry("new", "2026-08-19 09:00", "Priya Sharma replaced the pager"))
	memNote(t, ix, "memory/exp.md",
		memory.Entry{ID: "exp", Stamp: "2026-08-10 09:00", Agent: "claude",
			Text: "Priya Sharma rotated a temporary token", Expires: "2026-08-15T00:00:00Z"})

	base := MemoryQuery{Query: "release checklist", Limit: 1, Now: expandNow}

	// Alice: not on acl's reader list, no private opt-in. Only the seed's own
	// neighbours that she may see can appear.
	alice := base
	alice.Filter = Filter{User: "alice"}
	got, err := ix.RecallExpanded(alice, ExpandOptions{Hops: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range got {
		switch h.ID {
		case "acl", "priv", "sup", "exp":
			t.Errorf("alice received graph-added %q: %v", h.ID, expandedIDs(got))
		}
	}
	for _, id := range []string{"seed"} {
		hitByID(t, got, id)
	}

	// Bob is on the reader list, so acl is reachable through the walk; the
	// private note still needs an explicit opt-in.
	bob := base
	bob.Filter = Filter{User: "bob"}
	got, _ = ix.RecallExpanded(bob, ExpandOptions{Hops: 1})
	hitByID(t, got, "acl")
	for _, h := range got {
		if h.ID == "priv" {
			t.Errorf("private fact reached without IncludePrivate: %v", expandedIDs(got))
		}
	}

	// Superseded and expired facts stay out unless the caller asks for them,
	// exactly as in a direct recall.
	got, _ = ix.RecallExpanded(MemoryQuery{Query: "release checklist", Limit: 1, Now: expandNow,
		Filter: Filter{IncludePrivate: true, IgnoreACLs: true}}, ExpandOptions{Hops: 1})
	for _, h := range got {
		if h.ID == "sup" || h.ID == "exp" {
			t.Errorf("walk returned a dead fact %q: %v", h.ID, expandedIDs(got))
		}
	}
	got, _ = ix.RecallExpanded(MemoryQuery{Query: "release checklist", Limit: 1, Now: expandNow,
		IncludeSuperseded: true, IncludeExpired: true,
		Filter: Filter{IncludePrivate: true, IgnoreACLs: true}}, ExpandOptions{Hops: 1})
	hitByID(t, got, "sup")
	hitByID(t, got, "exp")
}

func TestGraphWalkIsBounded(t *testing.T) {
	ix := testIndex(t)
	entries := []memory.Entry{entry("seed", "2026-09-01 09:00", "Priya Sharma owns the release checklist")}
	for i := 0; i < 50; i++ {
		entries = append(entries, entry(fmt.Sprintf("n%02d", i), "2026-08-01 09:00",
			fmt.Sprintf("Priya Sharma note number %d", i)))
	}
	memNote(t, ix, "memory/many.md", entries...)

	got, err := ix.RecallExpanded(MemoryQuery{Query: "release checklist", Limit: 5, Now: expandNow},
		ExpandOptions{Hops: 2})
	if err != nil {
		t.Fatal(err)
	}
	added := 0
	for _, h := range got {
		if h.Via == "graph" {
			added++
		}
	}
	if added != graphMaxAdded {
		t.Errorf("graph added %d, want exactly %d", added, graphMaxAdded)
	}
	if len(got) > 5+graphMaxAdded {
		t.Errorf("returned %d, want at most %d", len(got), 5+graphMaxAdded)
	}
}

func TestExpandedRecallIsDeterministic(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/team.md",
		entry("seed", "2026-08-16 09:00", "Priya Sharma owns the release checklist"),
		entry("a", "2026-08-10 09:00", "Priya Sharma moved the strix box"),
		entry("b", "2026-08-10 09:00", "Priya Sharma reviewed the strix plan"),
		entry("c", "2026-08-11 09:00", "the strix box runs ollama"))
	q := MemoryQuery{Query: "release checklist", Limit: 5, Now: expandNow}
	first, _ := ix.RecallExpanded(q, ExpandOptions{Expand: true, Hops: 2})
	for i := 0; i < 5; i++ {
		again, _ := ix.RecallExpanded(q, ExpandOptions{Expand: true, Hops: 2})
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differs:\n%v\n%v", i, expandedIDs(first), expandedIDs(again))
		}
	}
}

func toExpanded(hits []MemoryHit) []ExpandedHit { return wrapHits(hits) }

func hasVariant(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
