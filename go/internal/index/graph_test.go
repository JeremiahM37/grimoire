package index

import (
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

func nodeNames(g *MemoryGraph) []string {
	out := make([]string, len(g.Nodes))
	for i, n := range g.Nodes {
		out[i] = n.Entity
	}
	return out
}

func hasNode(g *MemoryGraph, want string) bool {
	for _, n := range g.Nodes {
		if n.Entity == want {
			return true
		}
	}
	return false
}

func edgeBetween(g *MemoryGraph, a, b string) *GraphEdge {
	for i, e := range g.Edges {
		if (e.From == a && e.To == b) || (e.From == b && e.To == a) {
			return &g.Edges[i]
		}
	}
	return nil
}

// teamGraph: Priya works with Marco on AIServer; Marco owns the Deploy
// Runbook; an unrelated fact about a cat sits off to one side.
func teamGraph(t *testing.T) *Index {
	t.Helper()
	ix := testIndex(t)
	memNote(t, ix, "memory/team.md",
		entry("f1", "2026-08-14 09:00", "Priya Sharma and Marco Diaz maintain AIServer"),
		entry("f2", "2026-08-15 09:00", "Marco Diaz owns the Deploy Runbook"),
		entry("f3", "2026-08-16 09:00", "Marmalade is a cat"))
	return ix
}

func TestGraphFindsWhatAnEntityIsConnectedTo(t *testing.T) {
	ix := teamGraph(t)
	g, err := ix.MemoryGraphFor(GraphQuery{Seed: "Priya Sharma", Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if g.Nodes[0].Entity != "priya sharma" || g.Nodes[0].Depth != 0 {
		t.Fatalf("seed is not the first node: %v", nodeNames(g))
	}
	for _, want := range []string{"marco diaz", "aiserver"} {
		if !hasNode(g, want) {
			t.Errorf("one hop from priya lost %q: %v", want, nodeNames(g))
		}
	}
	// Two hops away, so not at depth 1.
	if hasNode(g, "deploy runbook") {
		t.Errorf("depth 1 reached a two-hop entity: %v", nodeNames(g))
	}
	// Never connected at all.
	if hasNode(g, "marmalade") {
		t.Errorf("an unrelated entity appeared: %v", nodeNames(g))
	}
}

func TestGraphDepthTwoReachesFurther(t *testing.T) {
	ix := teamGraph(t)
	g, _ := ix.MemoryGraphFor(GraphQuery{Seed: "priya sharma", Depth: 2})
	if !hasNode(g, "deploy runbook") {
		t.Fatalf("depth 2 did not reach the runbook: %v", nodeNames(g))
	}
	for _, n := range g.Nodes {
		if n.Entity == "deploy runbook" && n.Depth != 2 {
			t.Errorf("runbook reported at depth %d, want 2", n.Depth)
		}
	}
	if hasNode(g, "marmalade") {
		t.Errorf("depth 2 pulled in a disconnected entity: %v", nodeNames(g))
	}
}

func TestGraphEdgesNameTheFactsThatConnect(t *testing.T) {
	// An edge with no evidence is an assertion; the point of the graph is that
	// you can read the fact that made the connection.
	ix := teamGraph(t)
	g, _ := ix.MemoryGraphFor(GraphQuery{Seed: "priya sharma", Depth: 1})
	edge := edgeBetween(g, "priya sharma", "marco diaz")
	if edge == nil {
		t.Fatalf("no edge between the two people: %+v", g.Edges)
	}
	if len(edge.Facts) != 1 || edge.Facts[0] != "f1" {
		t.Errorf("edge cites %v, want [f1]", edge.Facts)
	}
	var found bool
	for _, e := range g.Entries {
		if e.ID == "f1" {
			found = true
		}
	}
	if !found {
		t.Error("the graph did not carry the fact its edge cites")
	}
}

func TestGraphResolvesAPartialName(t *testing.T) {
	// Asking about "priya" has to reach facts whose entity is "priya sharma",
	// or the graph is only usable by someone who already knows the full name.
	ix := teamGraph(t)
	g, _ := ix.MemoryGraphFor(GraphQuery{Seed: "priya", Depth: 1})
	if g.Seed != "priya sharma" {
		t.Fatalf("seed resolved to %q", g.Seed)
	}
	if !hasNode(g, "marco diaz") {
		t.Errorf("partial name found nothing: %v", nodeNames(g))
	}
}

func TestGraphOnAnUnknownEntityIsEmptyNotAnError(t *testing.T) {
	ix := teamGraph(t)
	g, err := ix.MemoryGraphFor(GraphQuery{Seed: "nobody-by-that-name", Depth: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 1 || len(g.Edges) != 0 {
		t.Errorf("unknown seed produced %v / %v", nodeNames(g), g.Edges)
	}
}

func TestGraphWithNoSeedRanksTheBusiestEntities(t *testing.T) {
	ix := teamGraph(t)
	g, err := ix.MemoryGraphFor(GraphQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if g.Nodes[0].Entity != "marco diaz" {
		t.Errorf("busiest entity = %q, want marco diaz: %v", g.Nodes[0].Entity, nodeNames(g))
	}
	if g.Nodes[0].Facts != 2 {
		t.Errorf("fact count = %d, want 2", g.Nodes[0].Facts)
	}
	if !hasNode(g, "marmalade") {
		t.Error("the overview dropped a disconnected entity")
	}
}

func TestGraphLimitDropsEdgesToDroppedNodes(t *testing.T) {
	// An edge pointing at a node that was cut is not an edge.
	ix := teamGraph(t)
	g, _ := ix.MemoryGraphFor(GraphQuery{Limit: 1})
	if len(g.Nodes) != 1 {
		t.Fatalf("limit ignored: %v", nodeNames(g))
	}
	for _, e := range g.Edges {
		if !hasNode(g, e.From) || !hasNode(g, e.To) {
			t.Errorf("edge %v-%v survived its node being cut", e.From, e.To)
		}
	}
}

func TestGraphExcludesSupersededFactsByDefault(t *testing.T) {
	// A connection that only a replaced belief ever asserted is not a
	// connection any more.
	ix := testIndex(t)
	memNote(t, ix, "memory/team.md",
		memory.Entry{ID: "old", Stamp: "2026-08-14 09:00", Agent: "claude",
			Text: "Priya Sharma maintains AIServer", SupersededBy: "new",
			SupersededAt: "2026-08-15 09:00"},
		entry("new", "2026-08-15 09:00", "Marco Diaz maintains AIServer"))

	g, _ := ix.MemoryGraphFor(GraphQuery{Seed: "aiserver", Depth: 1})
	if hasNode(g, "priya sharma") {
		t.Errorf("a superseded connection is still in the graph: %v", nodeNames(g))
	}
	if !hasNode(g, "marco diaz") {
		t.Errorf("the current connection is missing: %v", nodeNames(g))
	}
}

func TestGraphRespectsTheAccessFilter(t *testing.T) {
	// The graph is built from the entries the filter returned, precisely so
	// there is no second copy of the access rules to get wrong.
	ix := testIndex(t)
	fm := markdown.NewFrontmatter()
	fm.Set("private", true)
	body := "# Memory\n\n" + entry("p1", "2026-08-14 09:00",
		"Priya Sharma runs the Secret Project").Format() + "\n"
	if _, err := ix.Vault.Write("memory/private.md", body, fm); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Upsert("memory/private.md"); err != nil {
		t.Fatal(err)
	}
	g, _ := ix.MemoryGraphFor(GraphQuery{Seed: "priya sharma", Depth: 2})
	if hasNode(g, "secret project") {
		t.Fatalf("a private fact's entity leaked into the graph: %v", nodeNames(g))
	}
	g, _ = ix.MemoryGraphFor(GraphQuery{
		Memory: MemoryQuery{Filter: Filter{IncludePrivate: true}},
		Seed:   "priya sharma", Depth: 2})
	if !hasNode(g, "secret project") {
		t.Errorf("opting in did not include it: %v", nodeNames(g))
	}
}
