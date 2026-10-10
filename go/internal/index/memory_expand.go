package index

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Recall expansion and the entity-graph walk.
//
// Both are off unless asked for, and both are built from the ordinary recall
// path rather than beside it: every variant query and every graph candidate
// comes out of MemoryEntries, so the access rules (space, reader list, private,
// superseded, expired, validity, as_of) are applied exactly once, in one place.
// Nothing in this file re-implements an access check.

const (
	// expandRRFK is the reciprocal-rank-fusion constant. 60 is the value from the
	// original RRF paper and the one used elsewhere in this index.
	expandRRFK = 60
	// expandSeeds is how many of the top hits contribute entities to the walk.
	expandSeeds = 5
	// graphMaxAdded bounds how many graph-reached facts one recall can add.
	graphMaxAdded = 20
	// graphDecay is the per-hop score multiplier: a neighbour one hop out is
	// worth half its source, two hops out a quarter.
	graphDecay = 0.5
)

// Variant names, as they appear in explain output.
const (
	VariantLiteral = "literal"
	VariantEntity  = "entity"
	VariantKeyword = "keyword"
	VariantAlias   = "alias"
)

// ExpandOptions selects the optional recall stages. The zero value is plain
// recall: RecallExpanded then returns exactly what MemoryEntries returns.
type ExpandOptions struct {
	// Expand runs the query variants and fuses them.
	Expand bool
	// Hops is how far the entity-graph walk reaches from the top hits: 0 is
	// off, 1 and 2 are the supported depths.
	Hops int
}

// ExpandedHit is a recalled entry plus how it was reached.
type ExpandedHit struct {
	MemoryHit
	// Variants names the query variants that returned this entry, in variant
	// order. Empty when expansion did not run.
	Variants []string
	// Fused is the raw reciprocal-rank-fusion sum behind Score. Zero when
	// expansion did not run.
	Fused float64
	// Hop is 0 for a direct hit and 1 or 2 for a graph-added fact.
	Hop int
	// Via is "graph" for a graph-added fact, empty otherwise.
	Via string
	// Connect is the entity that linked a graph-added fact to its source.
	Connect string
}

// RecallExpanded runs recall with the optional stages in opt. With the zero
// options it is MemoryEntries, unchanged.
//
// Score is the fused RRF value scaled into (0, 1] when expansion ran, so a
// fact ranked first by every variant scores 1. A graph-added fact's Score is
// its source's score times graphDecay per hop.
func (ix *Index) RecallExpanded(q MemoryQuery, opt ExpandOptions) ([]ExpandedHit, error) {
	if !opt.Expand && opt.Hops <= 0 {
		hits, err := ix.MemoryEntries(q)
		if err != nil {
			return nil, err
		}
		return wrapHits(hits), nil
	}
	if q.Now.IsZero() {
		q.Now = time.Now()
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}

	var uni *recallUniverse
	if opt.Hops > 0 || (opt.Expand && strings.TrimSpace(q.Query) != "") {
		u, err := ix.memoryUniverse(q)
		if err != nil {
			return nil, err
		}
		uni = u
	}

	initial, err := ix.expandedInitial(q, opt.Expand, uni)
	if err != nil {
		return nil, err
	}
	if opt.Hops <= 0 || uni == nil {
		return initial, nil
	}
	return append(initial, uni.walk(initial, opt.Hops)...), nil
}

func wrapHits(hits []MemoryHit) []ExpandedHit {
	out := make([]ExpandedHit, len(hits))
	for i, h := range hits {
		out[i] = ExpandedHit{MemoryHit: h}
	}
	return out
}

// expandedInitial is the direct hit set: the literal query alone, or the
// literal query and its variants fused by reciprocal rank.
func (ix *Index) expandedInitial(q MemoryQuery, expand bool, uni *recallUniverse) ([]ExpandedHit, error) {
	if !expand || strings.TrimSpace(q.Query) == "" {
		hits, err := ix.MemoryEntries(q)
		if err != nil {
			return nil, err
		}
		return wrapHits(hits), nil
	}

	variants := expansionVariants(q.Query, uni)
	if len(variants) == 1 {
		// Nothing distinct to add, so there is nothing to fuse. Return the
		// plain ranking rather than a one-list fusion that would rescale it.
		hits, err := ix.MemoryEntries(q)
		if err != nil {
			return nil, err
		}
		return wrapHits(hits), nil
	}

	// Each variant gets a deeper pool than the final limit. A fact that is
	// fifth for one phrasing is often first for another, and fusion needs the
	// depth to see it.
	pool := q.Limit * 2
	lists := make([][]MemoryHit, len(variants))
	for i, v := range variants {
		vq := q
		vq.Limit = pool
		if i > 0 {
			// The caller's QueryVector embeds the literal text. Every other
			// variant must be embedded from its own text.
			vq.Query = v.text
			vq.QueryVector = nil
		}
		hits, err := ix.MemoryEntries(vq)
		if err != nil {
			return nil, err
		}
		lists[i] = hits
	}
	return fuseVariants(variants, lists, q.Limit), nil
}

// variant is one query phrasing. Index 0 is always the literal query.
type variant struct {
	name string
	text string
}

// expansionVariants builds the distinct phrasings to search with, the literal
// query first. A variant identical to one already present is dropped, so the
// fusion never counts the same ranking twice.
func expansionVariants(query string, uni *recallUniverse) []variant {
	out := []variant{{VariantLiteral, query}}
	add := func(name, text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		norm := strings.Join(strings.Fields(strings.ToLower(text)), " ")
		for _, v := range out {
			if strings.Join(strings.Fields(strings.ToLower(v.text)), " ") == norm {
				return
			}
		}
		out = append(out, variant{name, text})
	}

	if ents := memory.ExpansionEntities(query); len(ents) > 0 {
		add(VariantEntity, strings.Join(ents, " "))
	}
	if kws := memory.ExpansionKeywords(query); len(kws) > 0 {
		add(VariantKeyword, strings.Join(kws, " "))
	}

	// Alias resolution rewrites the words of the query, then stem forms of its
	// content words are appended so that "owns" also searches "own". The
	// alias map comes from the caller's visible entities, so an alias can only
	// be drawn from names the caller is allowed to see.
	var aliases map[string]string
	if uni != nil {
		names := uni.entityNames()
		aliases = memory.EntityAliases(names)
		for w, full := range memory.FirstNameAliases(strings.Fields(query), names) {
			if _, ok := aliases[w]; !ok {
				aliases[w] = full
			}
		}
	}
	rewritten := rewriteAliases(query, aliases)
	stems := stemForms(memory.ExpansionKeywords(rewritten))
	if stems != "" {
		rewritten = strings.TrimSpace(rewritten + " " + stems)
	}
	add(VariantAlias, rewritten)

	return out[:min(len(out), 4)]
}

// rewriteAliases replaces each query word that is a known alias with the full
// name it stands for. Punctuation around a word is kept.
func rewriteAliases(query string, aliases map[string]string) string {
	if len(aliases) == 0 {
		return query
	}
	words := strings.Fields(query)
	for i, w := range words {
		core := strings.ToLower(strings.Trim(w, ".,;:!?'\"()[]"))
		if full, ok := aliases[core]; ok {
			words[i] = full
		}
	}
	return strings.Join(words, " ")
}

// stemForms returns the stem of each keyword that differs from the keyword,
// joined, so that they can be appended to a query.
func stemForms(keywords []string) string {
	var out []string
	seen := map[string]bool{}
	for _, k := range keywords {
		for _, w := range strings.Fields(k) {
			s := memory.StemVariant(w)
			if s != w && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return strings.Join(out, " ")
}

// fuseVariants merges the variant rankings by reciprocal rank:
// sum over variants of 1/(expandRRFK + rank), rank counted from 1. The entry keeps
// the component scores of the variant that ranked it highest.
func fuseVariants(variants []variant, lists [][]MemoryHit, limit int) []ExpandedHit {
	type acc struct {
		hit      MemoryHit
		bestRank int
		fused    float64
		names    []string
	}
	byKey := map[string]*acc{}
	var order []string
	for vi, list := range lists {
		for rank, h := range list {
			k := h.Note + "\x00" + h.ID
			a := byKey[k]
			if a == nil {
				a = &acc{hit: h, bestRank: rank}
				byKey[k] = a
				order = append(order, k)
			} else if rank < a.bestRank {
				a.hit, a.bestRank = h, rank
			}
			a.fused += 1 / float64(expandRRFK+rank+1)
			a.names = append(a.names, variants[vi].name)
		}
	}

	out := make([]ExpandedHit, 0, len(order))
	scale := float64(expandRRFK+1) / float64(len(variants))
	for _, k := range order {
		a := byKey[k]
		h := a.hit
		h.Score = a.fused * scale
		out = append(out, ExpandedHit{MemoryHit: h, Variants: a.names, Fused: a.fused})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Fused != b.Fused {
			return a.Fused > b.Fused
		}
		if a.Stamp != b.Stamp {
			return a.Stamp > b.Stamp
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Note < b.Note
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// recallUniverse is every entry the caller may see under the query's filters,
// newest first, with each entry's entities. The walk and the alias map are both
// drawn from it, so neither needs its own access check.
type recallUniverse struct {
	hits []MemoryHit
	ents map[entKey][]string
}

// memoryUniverse selects the caller's visible entries with the query's filters
// but without its text: the same MemoryEntries path, so the same rules.
func (ix *Index) memoryUniverse(q MemoryQuery) (*recallUniverse, error) {
	base := q
	base.Query = ""
	base.QueryVector = nil
	base.LexicalOnly = false
	base.Limit = DefaultScanLimit
	hits, err := ix.MemoryEntries(base)
	if err != nil {
		return nil, err
	}
	rows := make([]memoryRow, len(hits))
	for i, h := range hits {
		rows[i] = memoryRow{hit: h}
	}
	return &recallUniverse{hits: hits, ents: ix.candidateEntities(rows, nil)}, nil
}

// entitiesOf returns an entry's entities, from the stored table where it has
// rows and by extraction otherwise.
func (u *recallUniverse) entitiesOf(h MemoryHit) []string {
	if e, ok := u.ents[entKey{h.Note, h.ID}]; ok {
		return e
	}
	return memory.Entities(h.Text)
}

// entityNames is the distinct set of entities across the universe, the input
// to alias resolution.
func (u *recallUniverse) entityNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range u.hits {
		for _, e := range u.entitiesOf(h) {
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	return out
}

// walk reaches out from the top direct hits through shared entities. Hop one
// adds facts that share an entity with a seed; hop two adds facts that share an
// entity with a hop-one fact. Each reached fact keeps the best decayed score
// and the entity that linked it, and at most graphMaxAdded are returned.
//
// Ties are broken by stamp then id, and candidates are read in the universe's
// own order, so the result does not depend on map iteration.
func (u *recallUniverse) walk(direct []ExpandedHit, hops int) []ExpandedHit {
	seeds := direct
	if len(seeds) > expandSeeds {
		seeds = seeds[:expandSeeds]
	}
	taken := make(map[string]bool, len(direct))
	for _, d := range direct {
		taken[d.Note+"\x00"+d.ID] = true
	}

	// frontier maps an entity to the best source score that reached it.
	frontier := map[string]float64{}
	for _, s := range seeds {
		for _, e := range u.entitiesOf(s.MemoryHit) {
			if cur, ok := frontier[e]; !ok || s.Score > cur {
				frontier[e] = s.Score
			}
		}
	}
	visited := map[string]bool{}
	for e := range frontier {
		visited[e] = true
	}

	type cand struct {
		hit     MemoryHit
		score   float64
		hop     int
		connect string
	}
	var cands []cand

	for hop := 1; hop <= hops && len(frontier) > 0; hop++ {
		decay := math.Pow(graphDecay, float64(hop))
		next := map[string]float64{}
		for _, h := range u.hits {
			k := h.Note + "\x00" + h.ID
			if taken[k] {
				continue
			}
			ents := u.entitiesOf(h)
			connect, src, matched := "", 0.0, false
			for _, e := range ents {
				s, ok := frontier[e]
				if !ok {
					continue
				}
				if !matched || e < connect {
					connect = e
				}
				if !matched || s > src {
					src = s
				}
				matched = true
			}
			if !matched {
				continue
			}
			cands = append(cands, cand{hit: h, score: decay * src, hop: hop, connect: connect})
			taken[k] = true
			for _, e := range ents {
				if visited[e] {
					continue
				}
				if cur, ok := next[e]; !ok || src > cur {
					next[e] = src
				}
			}
		}
		for e := range next {
			visited[e] = true
		}
		frontier = next
	}

	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.hit.Stamp != b.hit.Stamp {
			return a.hit.Stamp > b.hit.Stamp
		}
		if a.hit.ID != b.hit.ID {
			return a.hit.ID < b.hit.ID
		}
		return a.hit.Note < b.hit.Note
	})
	if len(cands) > graphMaxAdded {
		cands = cands[:graphMaxAdded]
	}
	out := make([]ExpandedHit, 0, len(cands))
	for _, c := range cands {
		h := c.hit
		h.Score = c.score
		out = append(out, ExpandedHit{MemoryHit: h, Hop: c.hop, Via: "graph", Connect: c.connect})
	}
	return out
}
