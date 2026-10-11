package api

import (
	"sort"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Source lineage for dispute scoring (experiment flag "lineage").
//
// Today a dispute counts corroboration as the number of distinct writers, or
// distinct declared origins, asserting the same text. Ten agents that each
// read one web page and cite it as evidence count as ten. With the flag on, a
// side is corroborated by the number of distinct lineages behind the claim:
//
//   - a fact that cites another fact in the population (ev=<entry id>) is
//     derived from it and inherits its lineage; the hop depth is its parent's
//     plus one;
//   - otherwise the lineage is the declared origin (org=), or the first cited
//     source (ev=, a note path or url), or the session it was first stated in,
//     or the writer;
//   - a writer never corroborates its own claim, whichever lineage it carries.
//
// Hop depth is 0 for a human statement or a verified citation, and +1 for each
// derivation or relay (a declared origin, or a citation that did not resolve).
// It is derived here from the dispute population at scoring time. It is not
// stored, and it is not written to the index.
//
// Every function in this file is reached only through exp.On("lineage").

// lineageFlag names the experiment in internal/exp.
const lineageFlag = "lineage"

// maxLineageHops bounds the derivation walk, so a cycle in ev= links ends at a
// root rather than recursing forever.
const maxLineageHops = 32

// lineageNode is one fact's resolved lineage and hop depth.
type lineageNode struct {
	key  string
	hops int
}

// lineageOf resolves a fact's lineage and hop depth against the population
// byID. A cited entry that is not in byID is treated as an opaque source.
func lineageOf(h index.MemoryHit, byID map[string]index.MemoryHit, depth int) lineageNode {
	refs := memory.SplitEvidence(h.Evidence)
	if depth < maxLineageHops {
		for _, r := range refs {
			p, ok := byID[r]
			if !ok || r == h.ID {
				continue
			}
			up := lineageOf(p, byID, depth+1)
			hops := up.hops + 1
			if h.Human {
				hops = 0
			}
			return lineageNode{key: up.key, hops: hops}
		}
	}

	var n lineageNode
	switch {
	case h.Origin != "":
		n = lineageNode{key: "org:" + h.Origin, hops: 1}
	case len(refs) > 0:
		first := append([]string(nil), refs...)
		sort.Strings(first)
		n.key = "ev:" + first[0]
		if h.Verification != memory.VerifiedEvidence {
			n.hops = 1
		}
	case h.Session != "":
		n.key = "session:" + h.Session
	default:
		n.key = "writer:" + h.Agent
	}
	if h.Human {
		n.hops = 0
	}
	return n
}

// lineageTerms is the per-side result of scoring one dispute by lineage.
type lineageTerms struct {
	corroboration []int
	hops          []int
}

// scoreLineage counts, for each side, the distinct lineages asserting its text.
// The side itself always counts. A side from another writer counts. A side
// from the same writer does not, because a writer never corroborates itself.
func scoreLineage(sides []index.MemoryHit, byID map[string]index.MemoryHit, text func(index.MemoryHit) string) lineageTerms {
	nodes := make([]lineageNode, len(sides))
	texts := make([]string, len(sides))
	for i, s := range sides {
		nodes[i] = lineageOf(s, byID, 0)
		texts[i] = text(s)
	}
	out := lineageTerms{corroboration: make([]int, len(sides)), hops: make([]int, len(sides))}
	for i := range sides {
		seen := map[string]bool{nodes[i].key: true}
		for j := range sides {
			if j == i || texts[j] != texts[i] {
				continue
			}
			if sides[i].Agent != "" && sides[j].Agent == sides[i].Agent {
				continue
			}
			seen[nodes[j].key] = true
		}
		out.corroboration[i] = len(seen)
		out.hops[i] = nodes[i].hops
	}
	return out
}

// lineageHops maps each fact in hits to its hop depth, for explain output. Only
// the facts in hits are visible as parents, so a derivation from outside the
// result set reads as a root.
func lineageHops(hits []index.MemoryHit) map[string]int {
	byID := make(map[string]index.MemoryHit, len(hits))
	for _, h := range hits {
		byID[h.ID] = h
	}
	out := make(map[string]int, len(hits))
	for _, h := range hits {
		out[h.ID] = lineageOf(h, byID, 0).hops
	}
	return out
}
