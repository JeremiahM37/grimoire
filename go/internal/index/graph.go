package index

import (
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// The entity graph over agent memory.
//
// Entities already score recall (see memory.go). This makes them navigable:
// what does memory know ABOUT a thing, and what is that thing connected to.
// It is the question a person asks about someone else's notes — "what do we
// know about this customer" — and the one an agent asks before acting on a
// name it has only just been given.
//
// The graph is built from the entries the access filter returned, not from a
// second query against memory_entities. Those rows carry no space or reader
// list, so a query straight against them would need its own copy of the access
// rules — and a second copy of an access rule is the one that goes wrong. The
// cost is that the graph is built over the facts a query can see rather than
// streamed, which is the right trade at the scale one person's memory has.

// GraphNode is one entity and how much memory is about it.
type GraphNode struct {
	Entity string `json:"entity"`
	Facts  int    `json:"facts"`
	// Depth is how many hops from the seed. 0 is the seed itself.
	Depth int `json:"depth"`
}

// GraphEdge is two entities that appear in the same fact, and the facts that
// connect them.
type GraphEdge struct {
	From  string   `json:"from"`
	To    string   `json:"to"`
	Facts []string `json:"facts"`
}

// MemoryGraph is a neighbourhood of the entity graph.
type MemoryGraph struct {
	Seed  string      `json:"seed,omitempty"`
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
	// Entries are the facts the neighbourhood was built from, so a caller gets
	// the evidence with the shape rather than having to ask twice.
	Entries []MemoryHit `json:"entries"`
}

// GraphQuery selects a neighbourhood.
type GraphQuery struct {
	// Memory is the access-checked selection the graph is built over. Its
	// Limit bounds how much memory is considered.
	Memory MemoryQuery

	// Seed is the entity to start from. Empty returns the busiest entities in
	// scope, which is the "show me the shape of what I know" case.
	Seed string
	// Depth is how many hops out from the seed. Ignored without one.
	Depth int
	// Limit caps returned nodes.
	Limit int
}

// MemoryGraphFor builds the neighbourhood a principal may see.
func (ix *Index) MemoryGraphFor(q GraphQuery) (*MemoryGraph, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Depth <= 0 {
		q.Depth = 1
	}
	if q.Memory.Limit <= 0 {
		// Bounded, but far above a neighbourhood: the graph is only as
		// complete as the facts it was built from, and silently building it
		// from twenty would look like an answer.
		q.Memory.Limit = 5000
	}
	entries, err := ix.MemoryEntries(q.Memory)
	if err != nil {
		return nil, err
	}

	// entities[i] are the entities of entries[i], computed once.
	entities := make([][]string, len(entries))
	factsOf := map[string][]string{}
	for i, e := range entries {
		entities[i] = memory.Entities(e.Text)
		for _, ent := range entities[i] {
			factsOf[ent] = append(factsOf[ent], e.ID)
		}
	}

	seed := strings.ToLower(strings.TrimSpace(q.Seed))
	if seed == "" {
		return ix.busiest(entries, entities, factsOf, q.Limit), nil
	}
	// A seed nobody spelled exactly still finds its entity: asking about
	// "priya" should reach the facts whose entity is "priya sharma".
	seed = resolveSeed(seed, factsOf)

	depth := map[string]int{seed: 0}
	frontier := []string{seed}
	edgeKey := map[[2]string]map[string]bool{}
	used := map[string]bool{}

	for hop := 1; hop <= q.Depth && len(frontier) > 0; hop++ {
		var next []string
		for _, ent := range frontier {
			for i, e := range entries {
				if !hasEntity(entities[i], ent) {
					continue
				}
				used[e.ID] = true
				for _, other := range entities[i] {
					if other == ent {
						continue
					}
					key := edgePair(ent, other)
					if edgeKey[key] == nil {
						edgeKey[key] = map[string]bool{}
					}
					edgeKey[key][e.ID] = true
					if _, seen := depth[other]; !seen {
						depth[other] = hop
						next = append(next, other)
					}
				}
			}
		}
		frontier = next
	}

	graph := &MemoryGraph{Seed: seed}
	for ent, d := range depth {
		graph.Nodes = append(graph.Nodes, GraphNode{
			Entity: ent, Facts: len(factsOf[ent]), Depth: d})
	}
	// Closest first, then busiest, then by name so the order is total and a
	// test can rely on it.
	sort.Slice(graph.Nodes, func(i, j int) bool {
		a, b := graph.Nodes[i], graph.Nodes[j]
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		if a.Facts != b.Facts {
			return a.Facts > b.Facts
		}
		return a.Entity < b.Entity
	})
	if len(graph.Nodes) > q.Limit {
		graph.Nodes = graph.Nodes[:q.Limit]
	}
	kept := map[string]bool{}
	for _, n := range graph.Nodes {
		kept[n.Entity] = true
	}
	for key, ids := range edgeKey {
		if !kept[key[0]] || !kept[key[1]] {
			continue // an edge to a node the limit cut is not an edge
		}
		graph.Edges = append(graph.Edges, GraphEdge{
			From: key[0], To: key[1], Facts: sortedKeys(ids)})
	}
	sort.Slice(graph.Edges, func(i, j int) bool {
		if graph.Edges[i].From != graph.Edges[j].From {
			return graph.Edges[i].From < graph.Edges[j].From
		}
		return graph.Edges[i].To < graph.Edges[j].To
	})
	for _, e := range entries {
		if used[e.ID] {
			graph.Entries = append(graph.Entries, e)
		}
	}
	if graph.Edges == nil {
		graph.Edges = []GraphEdge{}
	}
	if graph.Entries == nil {
		graph.Entries = []MemoryHit{}
	}
	return graph, nil
}

// busiest is the no-seed view: the entities most of memory is about.
func (ix *Index) busiest(entries []MemoryHit, entities [][]string,
	factsOf map[string][]string, limit int) *MemoryGraph {

	graph := &MemoryGraph{Nodes: []GraphNode{}, Edges: []GraphEdge{},
		Entries: []MemoryHit{}}
	for ent, ids := range factsOf {
		graph.Nodes = append(graph.Nodes, GraphNode{Entity: ent, Facts: len(ids)})
	}
	sort.Slice(graph.Nodes, func(i, j int) bool {
		if graph.Nodes[i].Facts != graph.Nodes[j].Facts {
			return graph.Nodes[i].Facts > graph.Nodes[j].Facts
		}
		return graph.Nodes[i].Entity < graph.Nodes[j].Entity
	})
	if len(graph.Nodes) > limit {
		graph.Nodes = graph.Nodes[:limit]
	}
	kept := map[string]bool{}
	for _, n := range graph.Nodes {
		kept[n.Entity] = true
	}
	edges := map[[2]string]map[string]bool{}
	for i, e := range entries {
		for a := range entities[i] {
			for b := a + 1; b < len(entities[i]); b++ {
				x, y := entities[i][a], entities[i][b]
				if !kept[x] || !kept[y] {
					continue
				}
				key := edgePair(x, y)
				if edges[key] == nil {
					edges[key] = map[string]bool{}
				}
				edges[key][e.ID] = true
			}
		}
	}
	for key, ids := range edges {
		graph.Edges = append(graph.Edges, GraphEdge{
			From: key[0], To: key[1], Facts: sortedKeys(ids)})
	}
	sort.Slice(graph.Edges, func(i, j int) bool {
		if graph.Edges[i].From != graph.Edges[j].From {
			return graph.Edges[i].From < graph.Edges[j].From
		}
		return graph.Edges[i].To < graph.Edges[j].To
	})
	return graph
}

// resolveSeed maps a partial name onto the entity memory actually stores, so
// asking about "priya" reaches facts whose entity is "priya sharma". The
// shortest containing entity wins: it is the least specific match, and
// widening beyond what was asked is worse than narrowing.
func resolveSeed(seed string, factsOf map[string][]string) string {
	if _, exact := factsOf[seed]; exact || len(seed) < 3 {
		return seed
	}
	best := ""
	for ent := range factsOf {
		if !strings.Contains(ent, seed) {
			continue
		}
		if best == "" || len(ent) < len(best) || (len(ent) == len(best) && ent < best) {
			best = ent
		}
	}
	if best == "" {
		return seed
	}
	return best
}

// hasEntity is an exact membership test over one fact's entities.
func hasEntity(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func edgePair(a, b string) [2]string {
	if a < b {
		return [2]string{a, b}
	}
	return [2]string{b, a}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
