package bank

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// RecallRequest is one recall.
type RecallRequest struct {
	Query string
	// Types limits fact types; empty means all.
	Types []string
	// Budget is how widely each arm searches: low, mid (default) or high.
	Budget string
	// MaxTokens bounds the returned facts' text; nil means DefaultMaxTokens
	// and 0 returns no facts (chunks may still be returned).
	MaxTokens *int
	// QueryTimestamp is "now" for the question: the reference for relative
	// dates in the query and for recency.
	QueryTimestamp *time.Time
	Tags           []string
	TagsMatch      string // any (default), all, any_strict, all_strict, exact
	// TagGroups is a boolean tag filter (leaves, and/or/not), AND-ed with
	// Tags and with each other.
	TagGroups []TagGroup
	// Window, when set, is the time window recall ranks against instead of
	// one read from the query text.
	Window *Window
	// MinScores drops results below a reranker or final score.
	MinScores *MinScores
	// PreferObservations drops a fact from the results when an observation
	// built from it already made the cut, so the slot goes to something new.
	PreferObservations bool
	// Entities lists the entities of the returned facts (default on).
	Entities *bool
	// Chunks returns the source chunks of the top facts, within their own
	// budget; 0 means off.
	ChunkTokens int
	// SourceFacts returns the facts an observation was built from, within
	// SourceFactsMaxTokens (0 means 4096, -1 unlimited) and, when
	// SourceFactsPerObs is positive, at most that many tokens per observation.
	SourceFacts          bool
	SourceFactsMaxTokens int
	SourceFactsPerObs    int
	// NoRerank skips the reranker for internal callers that want the fused
	// order (consolidation's related-observation lookup).
	NoRerank bool
	Trace    bool
}

// MinScores are post-ranking floors. A nil field is no floor.
type MinScores struct {
	Reranker *float64 `json:"reranker,omitempty"`
	Final    *float64 `json:"final,omitempty"`
}

// DefaultMaxTokens is the recall budget for fact text.
const DefaultMaxTokens = 4096

// DefaultChunkTokens is the chunk budget when chunks are asked for.
const DefaultChunkTokens = 8192

var allTypes = []string{"world", "experience", "observation"}

// Scores explain one result's rank.
type Scores struct {
	Final    float64  `json:"final"`
	Reranker *float64 `json:"reranker"`
	Semantic *float64 `json:"semantic"`
	Keyword  *float64 `json:"keyword"`
}

// RecallFact is one recalled fact.
type RecallFact struct {
	ID            string            `json:"id"`
	Text          string            `json:"text"`
	Type          string            `json:"type"`
	Entities      []string          `json:"entities,omitempty"`
	Context       string            `json:"context,omitempty"`
	OccurredStart string            `json:"occurred_start,omitempty"`
	OccurredEnd   string            `json:"occurred_end,omitempty"`
	MentionedAt   string            `json:"mentioned_at,omitempty"`
	DocumentID    string            `json:"document_id,omitempty"`
	ChunkID       string            `json:"chunk_id,omitempty"`
	Tags          []string          `json:"tags"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	// Authority is who stands behind the fact: "human" for what a person
	// wrote or corrected, "agent" for what a model extracted.
	Authority string `json:"authority"`
	// DisputedBy names a human fact this one contradicts. A disputed fact is
	// still returned — the disagreement is information — but always below
	// the human fact it disagrees with.
	DisputedBy string `json:"disputed_by,omitempty"`
	DocRemoved bool   `json:"doc_removed,omitempty"`
	// SourceFactIDs and ProofCount are set on observations: the facts the
	// observation was built from, and how many there are.
	SourceFactIDs []string `json:"source_fact_ids,omitempty"`
	ProofCount    int      `json:"proof_count,omitempty"`
	Scores        Scores   `json:"scores"`
}

// EntityOut is one entity in a recall response.
type EntityOut struct {
	ID   string `json:"entity_id"`
	Name string `json:"canonical_name"`
}

// ChunkOut is one source chunk in a recall response.
type ChunkOut struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	Index     int    `json:"chunk_index"`
	Document  string `json:"document_id"`
	Truncated bool   `json:"truncated"`
}

// ArmHit is one candidate as an arm ranked it, for the trace.
type ArmHit struct {
	ID    string  `json:"id"`
	Rank  int     `json:"rank"`
	Score float64 `json:"score"`
}

// Trace explains a recall: what each arm found, how they fused, and timings.
type Trace struct {
	Budget       int                       `json:"thinking_budget"`
	Window       *Window                   `json:"temporal_window,omitempty"`
	Arms         map[string][]ArmHit       `json:"arms"`
	Ranks        map[string]map[string]int `json:"ranks"`
	Fused        int                       `json:"fused_candidates"`
	Reranked     bool                      `json:"reranked"`
	TokensUsed   int                       `json:"tokens_used"`
	Disputes     map[string]string         `json:"disputes,omitempty"`
	TimingsMS    map[string]float64        `json:"timings_ms"`
	BankFacts    int                       `json:"bank_facts"`
	KeywordTerms []string                  `json:"keyword_terms,omitempty"`
	// Reranker names the reranker that scored the candidates, RerankError
	// says why none did when one was configured, and Rerank lists each
	// candidate's normalised reranker score in fused order.
	Reranker    string   `json:"reranker,omitempty"`
	RerankError string   `json:"rerank_error,omitempty"`
	Rerank      []ArmHit `json:"rerank,omitempty"`
}

// RecallResponse is the result of a recall.
type RecallResponse struct {
	Results     []RecallFact          `json:"results"`
	Entities    map[string]EntityOut  `json:"entities,omitempty"`
	Chunks      map[string]ChunkOut   `json:"chunks,omitempty"`
	SourceFacts map[string]RecallFact `json:"source_facts,omitempty"`
	// SourceFactsTruncated reports that some observation's sources did not
	// fit the source-facts budget.
	SourceFactsTruncated bool   `json:"source_facts_truncated,omitempty"`
	Trace                *Trace `json:"trace,omitempty"`
}

// ThinkingBudget is how many candidates each arm may return.
func ThinkingBudget(budget string) (int, error) {
	switch budget {
	case "low":
		return 100, nil
	case "", "mid":
		return 300, nil
	case "high":
		return 1000, nil
	}
	return 0, invalid("budget must be low, mid or high")
}

// Arm and scoring constants. They are the defaults that keep each arm's
// contribution comparable; see docs/MEMORY_BANKS.md for what each one does.
const (
	rrfK               = 60
	maxRerankCands     = 300
	semanticFloor      = 0.3
	graphSeedLimit     = 20
	graphSeedFloor     = 0.3
	graphPerEntity     = 200
	temporalPool       = 60
	temporalEntries    = 10
	temporalBuckets    = 8
	temporalSimFloor   = 0.1
	temporalDecay      = 0.7
	temporalBatch      = 20
	temporalPerSource  = 10
	temporalIterations = 5
	temporalContinue   = 0.2
	causalBoost        = 2.0
	recencyAlpha       = 0.2
	temporalAlpha      = 0.2
	proofAlpha         = 0.1
	keywordMaxTerms    = 16
)

// candidate is one fact moving through fusion and scoring.
type candidate struct {
	pos       int32
	rrf       float64
	ranks     map[string]int
	semantic  *float64
	keyword   *float64
	proximity *float64
	reranker  *float64
	weight    float64
	disputed  string
}

func fptr(v float64) *float64 { return &v }

// Recall runs one recall over a bank.
func (e *Engine) Recall(ctx context.Context, bankID string, req RecallRequest) (*RecallResponse, error) {
	t0 := time.Now()
	timings := map[string]float64{}
	mark := func(name string, since time.Time) { timings[name] = float64(time.Since(since).Microseconds()) / 1000 }

	q := strings.TrimSpace(req.Query)
	if q == "" || len(tokenizeQuery(q)) == 0 {
		return nil, invalid("query must contain at least one word")
	}
	if !ValidID(bankID) {
		return nil, invalid("invalid bank id")
	}
	prof, err := e.Profile(bankID)
	if err != nil {
		return nil, err
	}
	tb, err := ThinkingBudget(req.Budget)
	if err != nil {
		return nil, err
	}
	maxTokens := DefaultMaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	types := map[string]bool{}
	if len(req.Types) == 0 {
		for _, t := range allTypes {
			types[t] = true
		}
	}
	for _, t := range req.Types {
		switch t {
		case "world", "experience", "observation":
			types[t] = true
		case "opinion": // retired type; ignored
		default:
			return nil, invalid("unknown fact type %q", t)
		}
	}
	match := req.TagsMatch
	switch match {
	case "":
		match = "any"
	case "any", "all", "any_strict", "all_strict", "exact":
	default:
		return nil, invalid("tags_match must be any, all, any_strict, all_strict or exact")
	}
	for i := range req.TagGroups {
		if err := req.TagGroups[i].validate(0); err != nil {
			return nil, invalid("tag_groups[%d]: %v", i, err)
		}
	}
	if req.Window != nil && (req.Window.Start.IsZero() || req.Window.End.IsZero() || req.Window.End.Before(req.Window.Start)) {
		return nil, invalid("temporal_window needs a start before its end")
	}
	now := time.Now().UTC()
	if req.QueryTimestamp != nil {
		now = req.QueryTimestamp.UTC()
	}

	tl := time.Now()
	c, err := e.cache(bankID)
	if err != nil {
		return nil, err
	}
	mark("load", tl)
	resp := &RecallResponse{Results: []RecallFact{}}
	var tr *Trace
	if req.Trace {
		tr = &Trace{Budget: tb, Arms: map[string][]ArmHit{}, Ranks: map[string]map[string]int{},
			TimingsMS: timings, BankFacts: len(c.units)}
		resp.Trace = tr
	}
	if len(types) == 0 || len(c.units) == 0 {
		return resp, nil
	}
	visible := func(i int32) bool {
		u := &c.units[i]
		return types[u.Type] && tagsAllow(u.Tags, req.Tags, match) && groupsAllow(req.TagGroups, u.Tags)
	}

	// --- semantic arm (and the graph arm's seeds)
	ts := time.Now()
	sims := c.similarities(e.queryVector(q))
	var semList, seeds []int32
	if sims != nil {
		semList = topBy(len(c.units), max(tb, graphSeedLimit), func(i int32) (float64, bool) {
			return float64(sims[i]), float64(sims[i]) >= semanticFloor && visible(i)
		})
		for _, p := range semList {
			if len(seeds) < graphSeedLimit && float64(sims[p]) >= graphSeedFloor {
				seeds = append(seeds, p)
			}
		}
		if len(semList) > tb {
			semList = semList[:tb]
		}
	}
	mark("semantic", ts)

	// --- keyword arm
	tk := time.Now()
	var kwList []int32
	kwScore := map[int32]float64{}
	if prof.Setting("enable_text_search", "true") == "true" {
		if terms := keywordTerms(q); len(terms) > 0 {
			var used []string
			kwList, kwScore, used, err = e.keywordArm(c, bankID, terms, tb, visible)
			if err != nil {
				return nil, err
			}
			if tr != nil {
				tr.KeywordTerms = used
			}
		}
	}
	mark("keyword", tk)

	// --- graph arm
	tg := time.Now()
	var graphList []int32
	graphScore := map[int32]float64{}
	if prof.Setting("enable_graph", "true") == "true" && len(seeds) > 0 {
		graphList, graphScore = c.graphArm(seeds, tb, visible)
	}
	mark("graph", tg)

	// --- temporal arm
	tt := time.Now()
	var tempList []int32
	tempScore, proximity := map[int32]float64{}, map[int32]float64{}
	if prof.Setting("enable_temporal", "true") == "true" {
		w, ok := ParseWindow(q, now)
		if req.Window != nil {
			w, ok = Window{Start: req.Window.Start.UTC(), End: req.Window.End.UTC()}, true
		}
		if ok {
			if tr != nil {
				tr.Window = &w
			}
			tempList, tempScore, proximity = c.temporalArm(w, sims, tb, visible)
		}
	}
	mark("temporal", tt)

	// --- fusion
	tf := time.Now()
	arms := []struct {
		name  string
		list  []int32
		score func(int32) float64
	}{
		{"semantic", semList, func(p int32) float64 { return float64(sims[p]) }},
		{"keyword", kwList, func(p int32) float64 { return kwScore[p] }},
		{"graph", graphList, func(p int32) float64 { return graphScore[p] }},
		{"temporal", tempList, func(p int32) float64 { return tempScore[p] }},
	}
	lists := make([][]int32, len(arms))
	names := make([]string, len(arms))
	for i, arm := range arms {
		lists[i], names[i] = arm.list, arm.name
		if tr != nil {
			for r, p := range arm.list {
				tr.Arms[arm.name] = append(tr.Arms[arm.name], ArmHit{ID: c.units[p].ID, Rank: r + 1, Score: arm.score(p)})
			}
		}
	}
	cands := fuseRRF(names, lists)
	for _, cd := range cands {
		if _, ok := cd.ranks["semantic"]; ok {
			cd.semantic = fptr(float64(sims[cd.pos]))
		}
		if v, ok := kwScore[cd.pos]; ok {
			cd.keyword = fptr(v)
		}
		if v, ok := proximity[cd.pos]; ok {
			cd.proximity = fptr(v)
		}
	}
	if len(cands) > maxRerankCands {
		cands = cands[:maxRerankCands]
	}
	if tr != nil {
		tr.Fused = len(cands)
	}
	mark("fusion", tf)

	// --- rerank
	tr2 := time.Now()
	reranked := false
	if e.Reranker != nil && !req.NoRerank && prof.Setting("enable_reranking", "true") == "true" && len(cands) > 0 {
		docs := make([]string, len(cands))
		for i, cd := range cands {
			docs[i] = rerankDoc(&c.units[cd.pos])
		}
		scores, err := e.Reranker.Score(ctx, q, docs)
		switch {
		case err != nil:
			// A reranker that fails (model missing, service down) costs the
			// query its reordering, never its answer.
			if tr != nil {
				tr.RerankError = err.Error()
			}
		case scores == nil:
			// No reranker behind the interface: nothing to report.
		case len(scores) != len(cands):
			if tr != nil {
				tr.RerankError = fmt.Sprintf("reranker returned %d scores for %d candidates", len(scores), len(cands))
			}
		default:
			norm := normaliseScores(scores)
			for i, cd := range cands {
				cd.reranker = fptr(norm[i])
			}
			reranked = true
			if tr != nil {
				tr.Reranker = rerankerName(e.Reranker)
				for i, cd := range cands {
					tr.Rerank = append(tr.Rerank, ArmHit{ID: c.units[cd.pos].ID, Rank: i + 1, Score: norm[i]})
				}
			}
		}
	}
	if !reranked {
		// Keep the fused order, mapped onto a score the boosts can scale.
		n := len(cands)
		for i, cd := range cands {
			v := 1.0
			if n > 1 {
				v = 1 - 0.9*float64(i)/float64(n-1)
			}
			cd.reranker = fptr(v)
		}
	}
	if tr != nil {
		tr.Reranked = reranked
	}
	mark("rerank", tr2)

	// --- boosts
	for _, cd := range cands {
		u := &c.units[cd.pos]
		rec := recency(u, now)
		temp := 0.5
		if cd.proximity != nil {
			temp = *cd.proximity
		}
		proof := 0.5
		if u.Proof >= 1 {
			proof = math.Max(0, math.Min(1, 0.5+math.Log(float64(u.Proof))/10))
		}
		cd.weight = *cd.reranker * (1 + recencyAlpha*(rec-0.5)) * (1 + temporalAlpha*(temp-0.5)) * (1 + proofAlpha*(proof-0.5))
	}
	sortCands(cands)

	// --- what a person wrote outranks what a model inferred
	cands = c.applyAuthority(cands, visible, tr)

	if req.MinScores != nil {
		kept := cands[:0]
		for _, cd := range cands {
			if f := req.MinScores.Reranker; f != nil && reranked && *cd.reranker < *f {
				continue
			}
			if f := req.MinScores.Final; f != nil && cd.weight < *f {
				continue
			}
			kept = append(kept, cd)
		}
		cands = kept
	}

	if len(cands) > 2*tb {
		cands = cands[:2*tb]
	}

	// --- an observation that made the cut stands in for the facts it was
	// built from, so their slots go to something it does not already say.
	if req.PreferObservations && types["observation"] && (types["world"] || types["experience"]) {
		covered := map[string]bool{}
		for _, cd := range cands {
			if u := &c.units[cd.pos]; u.Type == "observation" {
				for _, s := range u.Sources {
					covered[s] = true
				}
			}
		}
		if len(covered) > 0 {
			kept := cands[:0]
			for _, cd := range cands {
				if u := &c.units[cd.pos]; u.Type != "observation" && covered[u.ID] {
					continue
				}
				kept = append(kept, cd)
			}
			cands = kept
		}
	}

	// --- chunks, from the ranked list before packing
	if req.ChunkTokens > 0 {
		chunks, err := e.recallChunks(c, bankID, cands, req.ChunkTokens)
		if err != nil {
			return nil, err
		}
		resp.Chunks = chunks
	}

	// --- pack facts into the token budget
	costs := make([]int, len(cands))
	for i, cd := range cands {
		costs[i] = CountTokens(c.units[cd.pos].Text)
	}
	keepIdx, used := packByTokens(costs, maxTokens)
	picked := make([]*candidate, len(keepIdx))
	for i, k := range keepIdx {
		picked[i] = cands[k]
	}
	if tr != nil {
		tr.TokensUsed = used
		for _, cd := range picked {
			tr.Ranks[c.units[cd.pos].ID] = cd.ranks
		}
	}

	wantEnts := req.Entities == nil || *req.Entities
	if wantEnts {
		resp.Entities = map[string]EntityOut{}
	}
	for _, cd := range picked {
		u := &c.units[cd.pos]
		rf := c.factOut(bankID, cd.pos)
		rf.Scores = Scores{Final: cd.weight, Reranker: nilIfPassthrough(cd.reranker, reranked), Semantic: cd.semantic, Keyword: cd.keyword}
		rf.DisputedBy = cd.disputed
		if !wantEnts {
			rf.Entities = nil
		}
		resp.Results = append(resp.Results, rf)
		if wantEnts {
			for _, ei := range u.ents {
				en := c.entities[ei]
				if _, ok := resp.Entities[en.Name]; !ok {
					resp.Entities[en.Name] = EntityOut{ID: en.ID, Name: en.Name}
				}
			}
		}
	}
	if req.SourceFacts {
		resp.SourceFacts, resp.SourceFactsTruncated = c.sourceFacts(bankID, picked, req.SourceFactsMaxTokens, req.SourceFactsPerObs)
	}
	mark("total", t0)
	return resp, nil
}

// sourceFacts collects the facts behind the observations in a result list,
// in observation-rank order, skipping (not stopping at) one that does not fit
// the budget.
func (c *bankCache) sourceFacts(bankID string, picked []*candidate, budget, perObs int) (map[string]RecallFact, bool) {
	if budget == 0 {
		budget = 4096
	}
	out := map[string]RecallFact{}
	used, truncated := 0, false
	for _, cd := range picked {
		u := &c.units[cd.pos]
		if u.Type != "observation" {
			continue
		}
		mine := 0
		for _, sid := range u.Sources {
			if _, done := out[sid]; done {
				continue
			}
			p, ok := c.byID[sid]
			if !ok {
				continue
			}
			t := CountTokens(c.units[p].Text)
			if (budget > 0 && used+t > budget) || (perObs > 0 && mine+t > perObs) {
				truncated = true
				continue
			}
			used += t
			mine += t
			out[sid] = c.factOut(bankID, p)
		}
	}
	return out, truncated
}

// rerankerName is a reranker's self-reported name, when it has one.
func rerankerName(r Reranker) string {
	if n, ok := r.(interface{ Name() string }); ok {
		return n.Name()
	}
	return fmt.Sprintf("%T", r)
}

// fuseRRF merges ranked lists by reciprocal rank: each list adds 1/(k+rank)
// for every item it holds, so an item several arms agree on rises above one
// that a single arm ranked first. Only ranks are used — the arms' raw scores
// live on incomparable scales. Ties keep the earlier position.
func fuseRRF(names []string, lists [][]int32) []*candidate {
	byPos := map[int32]*candidate{}
	var cands []*candidate
	for li, list := range lists {
		for r, p := range list {
			cd, ok := byPos[p]
			if !ok {
				cd = &candidate{pos: p, ranks: map[string]int{}}
				byPos[p] = cd
				cands = append(cands, cd)
			}
			cd.rrf += 1 / float64(rrfK+r+1)
			cd.ranks[names[li]] = r + 1
		}
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].rrf != cands[b].rrf {
			return cands[a].rrf > cands[b].rrf
		}
		return cands[a].pos < cands[b].pos
	})
	return cands
}

// packByTokens keeps items in rank order while they fit the budget, skipping
// (not stopping at) one that does not, so a long fact does not block shorter
// ones ranked after it. If nothing fits but something was offered, the first
// item is returned alone: one fact over budget is more useful than none. A
// budget of zero or less returns nothing.
func packByTokens(costs []int, budget int) (keep []int, used int) {
	if budget <= 0 {
		return nil, 0
	}
	skipped := false
	for i, t := range costs {
		if used+t > budget {
			skipped = true
			continue
		}
		used += t
		keep = append(keep, i)
	}
	if len(keep) == 0 && skipped {
		return []int{0}, costs[0]
	}
	return keep, used
}

func nilIfPassthrough(v *float64, reranked bool) *float64 {
	if !reranked {
		return nil
	}
	return v
}

func sortCands(cands []*candidate) {
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].weight != cands[b].weight {
			return cands[a].weight > cands[b].weight
		}
		return cands[a].pos < cands[b].pos
	})
}

// factOut renders a cached fact for a response.
func (c *bankCache) factOut(bankID string, pos int32) RecallFact {
	u := &c.units[pos]
	rf := RecallFact{ID: u.ID, Text: u.Text, Type: u.Type, Context: u.Context,
		DocumentID: u.Doc, Tags: u.Tags, Authority: "agent", DocRemoved: u.DocRemoved}
	if rf.Tags == nil {
		rf.Tags = []string{}
	}
	if u.Human {
		rf.Authority = "human"
	}
	if u.Type == "observation" {
		rf.SourceFactIDs = u.Sources
		rf.ProofCount = u.Proof
	}
	if u.Chunk >= 0 && u.Doc != "" {
		rf.ChunkID = ChunkID(bankID, u.Doc, u.Chunk)
	}
	if u.OccStart != 0 {
		rf.OccurredStart = fromMS(u.OccStart).Format(time.RFC3339)
		end := u.OccEnd
		if end == 0 {
			end = u.OccStart
		}
		rf.OccurredEnd = fromMS(end).Format(time.RFC3339)
	}
	if u.Mentioned != 0 {
		rf.MentionedAt = fromMS(u.Mentioned).Format(time.RFC3339)
	}
	if u.Metadata != "" {
		_ = json.Unmarshal([]byte(u.Metadata), &rf.Metadata)
	}
	for _, ei := range u.ents {
		rf.Entities = append(rf.Entities, c.entities[ei].Name)
	}
	return rf
}

// tagsAllow applies a tag filter. "any" and "all" also admit untagged facts,
// so a tag filter narrows a bank without hiding the facts nobody tagged; the
// strict variants do not.
func tagsAllow(have, want []string, mode string) bool {
	if mode == "exact" {
		return sameTagSet(have, want)
	}
	if len(want) == 0 {
		return true
	}
	if len(have) == 0 {
		return mode == "any" || mode == "all"
	}
	set := map[string]bool{}
	for _, t := range have {
		set[t] = true
	}
	switch mode {
	case "all", "all_strict":
		for _, t := range want {
			if !set[t] {
				return false
			}
		}
		return true
	}
	for _, t := range want {
		if set[t] {
			return true
		}
	}
	return false
}

// queryVector embeds the query; nil when no embedder is configured.
func (e *Engine) queryVector(q string) []float32 {
	if e.Index.Emb == nil {
		return nil
	}
	vs := e.Index.Emb.Embed([]string{q})
	if len(vs) == 0 || len(vs[0]) == 0 {
		return nil
	}
	v := append([]float32(nil), vs[0]...)
	normalize(v)
	return v
}

// similarities is the cosine of every fact to qv, computed in parallel over
// the contiguous vector matrix. Facts without a vector score 0.
func (c *bankCache) similarities(qv []float32) []float32 {
	if qv == nil || c.dim == 0 || len(qv) != c.dim {
		return nil
	}
	n := len(c.units)
	out := make([]float32, n)
	workers := min(runtime.GOMAXPROCS(0), max(1, n/4096))
	per := (n + workers - 1) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo, hi := w*per, min((w+1)*per, n)
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			for i := lo; i < hi; i++ {
				out[i] = dot(c.vecs[i*c.dim:(i+1)*c.dim], qv)
			}
		}(lo, hi)
	}
	wg.Wait()
	return out
}

// topBy returns up to k positions with the highest scores among those
// accepted, highest first, ties by position.
func topBy(n, k int, score func(int32) (float64, bool)) []int32 {
	type sc struct {
		p int32
		s float64
	}
	var all []sc
	for i := 0; i < n; i++ {
		if s, ok := score(int32(i)); ok {
			all = append(all, sc{int32(i), s})
		}
	}
	sort.Slice(all, func(a, b int) bool {
		if all[a].s != all[b].s {
			return all[a].s > all[b].s
		}
		return all[a].p < all[b].p
	})
	if len(all) > k {
		all = all[:k]
	}
	out := make([]int32, len(all))
	for i, x := range all {
		out[i] = x.p
	}
	return out
}

// ------------------------------------------------------------ keyword arm

var queryTokenRE = regexp.MustCompile(`[^\p{L}\p{N}\s]+`)

func tokenizeQuery(q string) []string {
	return strings.Fields(queryTokenRE.ReplaceAllString(strings.ToLower(q), " "))
}

// stopwords carry no evidence of which fact is meant; an OR query over them
// matches most of a bank.
var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the and or but if of at by for with about against between into through
	during before after above below to from up down in out on off over under again further then once here there
	when where why how all any both each few more most other some such no nor not only own same so than too very
	s t can will just don should now i me my myself we our ours you your yours he him his she her hers it its they
	them their what which who whom this that these those am is are was were be been being have has had having do
	does did doing would could ought i'm you're he's she's it's we're they're i've you've we've they've i'd you'd
	he'd she'd we'd they'd i'll you'll he'll she'll we'll they'll isn't aren't wasn't weren't hasn't haven't hadn't
	doesn't didn't won't wouldn't shan't shouldn't can't cannot couldn't mustn't let's that's who's what's here's
	there's when's where's why's how's did does tell know`) {
		stopwords[w] = true
	}
}

// keywordTerms is the query's content words, deduplicated, in order.
func keywordTerms(q string) []string {
	var terms []string
	seen := map[string]bool{}
	for _, t := range tokenizeQuery(q) {
		if stopwords[t] || seen[t] {
			continue
		}
		seen[t] = true
		terms = append(terms, t)
	}
	return terms
}

// BM25 parameters.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// keywordArm ranks facts by BM25 over the query's content words.
//
// Matching — tokenising, Porter stemming, finding the facts that contain a
// term — is FTS5's, one MATCH per term confined to the bank by its token.
// Scoring is done here. FTS5's own bm25() costs about five microseconds per
// matching row, because it re-reads every row's position lists, and a common
// word in a 20,000-fact bank matches thousands: measured, that was 10–45 ms of
// a 15–50 ms recall. A per-term MATCH that returns only row ids costs a few
// hundred microseconds, and gives each term's document frequency for free —
// which is both BM25's idf and how the rarest terms are chosen when a long
// question has more than keywordMaxTerms of them.
//
// Facts are one or two sentences, so a term almost never repeats within one;
// term frequency is taken as 1, which makes the score exact for nearly every
// fact and leaves length normalisation doing the rest.
func (e *Engine) keywordArm(c *bankCache, bankID string, terms []string, tb int, visible func(int32) bool) ([]int32, map[int32]float64, []string, error) {
	bk := bkToken(bankID)
	type posting struct {
		term string
		pos  []int32
	}
	var lists []posting
	for _, t := range terms {
		// Postings are memoised for the life of the cache, which every write
		// to the bank replaces, so a memo is never stale.
		c.postMu.Lock()
		ps, hit := c.postings[t]
		c.postMu.Unlock()
		if hit {
			if len(ps) > 0 {
				lists = append(lists, posting{t, ps})
			}
			continue
		}
		expr := `bk:"` + bk + `" AND body:"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		rows, err := e.Index.DB.Query("SELECT rowid FROM bank_units_fts WHERE bank_units_fts MATCH ?", expr)
		if err != nil {
			return nil, nil, nil, err
		}
		ps = nil
		for rows.Next() {
			var rid int64
			if err := rows.Scan(&rid); err != nil {
				rows.Close()
				return nil, nil, nil, err
			}
			if p, ok := c.byRID[rid]; ok {
				ps = append(ps, p)
			}
		}
		rows.Close()
		c.postMu.Lock()
		if len(c.postings) < 50000 {
			c.postings[t] = ps
		}
		c.postMu.Unlock()
		if len(ps) > 0 {
			lists = append(lists, posting{t, ps})
		}
	}
	// The rarest terms carry the question; keep at most keywordMaxTerms.
	if len(lists) > keywordMaxTerms {
		sort.SliceStable(lists, func(a, b int) bool { return len(lists[a].pos) < len(lists[b].pos) })
		lists = lists[:keywordMaxTerms]
	}
	used := make([]string, len(lists))
	n := float64(len(c.units))
	avg := c.avgBodyLen()
	scores := make([]float64, len(c.units))
	var touched []int32
	for i, pl := range lists {
		used[i] = pl.term
		df := float64(len(pl.pos))
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		for _, p := range pl.pos {
			if scores[p] == 0 {
				touched = append(touched, p)
			}
			norm := 1 - bm25B + bm25B*float64(c.bodyLen[p])/avg
			scores[p] += idf * (bm25K1 + 1) / (1 + bm25K1*norm)
		}
	}
	list := touched[:0]
	for _, p := range touched {
		if visible(p) {
			list = append(list, p)
		}
	}
	sort.Slice(list, func(a, b int) bool {
		if scores[list[a]] != scores[list[b]] {
			return scores[list[a]] > scores[list[b]]
		}
		return list[a] < list[b]
	})
	if len(list) > tb {
		list = list[:tb]
	}
	out := make(map[int32]float64, len(list))
	for _, p := range list {
		out[p] = scores[p]
	}
	return list, out, used, nil
}

// ------------------------------------------------------------ graph arm

// graphArm expands one hop from the semantic seeds: facts sharing their
// entities, their nearest neighbours in meaning, and what they were caused
// by. A fact reached several ways adds the evidence up.
func (c *bankCache) graphArm(seeds []int32, tb int, visible func(int32) bool) ([]int32, map[int32]float64) {
	isSeed := map[int32]bool{}
	for _, s := range seeds {
		isSeed[s] = true
	}
	shared := map[int32]map[int32]bool{} // unit → entities shared with seeds
	seedEnts := map[int32]bool{}
	for _, s := range seeds {
		for _, ei := range c.units[s].ents {
			seedEnts[ei] = true
		}
	}
	for ei := range seedEnts {
		us := c.entities[ei].units
		// The most recently written facts of a very common entity.
		if len(us) > graphPerEntity {
			us = us[len(us)-graphPerEntity:]
		}
		for _, u := range us {
			if isSeed[u] {
				continue
			}
			if shared[u] == nil {
				shared[u] = map[int32]bool{}
			}
			shared[u][ei] = true
		}
	}
	score := map[int32]float64{}
	for u, ents := range shared {
		score[u] += math.Tanh(0.5 * float64(len(ents)))
	}
	semBest := map[int32]float64{}
	for _, ns := range c.semanticNeighbors(seeds) {
		for _, n := range ns {
			if !isSeed[n.pos] && n.weight > semBest[n.pos] {
				semBest[n.pos] = n.weight
			}
		}
	}
	for u, w := range semBest {
		score[u] += w
	}
	causal := map[int32]bool{}
	for _, s := range seeds {
		for _, t := range c.units[s].Causes {
			if !isSeed[t] {
				causal[t] = true
			}
		}
	}
	for u := range causal {
		score[u] += 1
	}
	var list []int32
	for u := range score {
		if visible(u) {
			list = append(list, u)
		}
	}
	sort.Slice(list, func(a, b int) bool {
		if score[list[a]] != score[list[b]] {
			return score[list[a]] > score[list[b]]
		}
		return list[a] < list[b]
	})
	if len(list) > tb {
		list = list[:tb]
	}
	return list, score
}

// ------------------------------------------------------------ temporal arm

// unitSpan is the time a fact covers: what happened when, or when it was said.
func unitSpan(u *unit) (start, end int64) {
	if u.OccStart != 0 {
		end := u.OccEnd
		if end == 0 {
			end = u.OccStart
		}
		return u.OccStart, endOfDayMS(end)
	}
	return u.Mentioned, u.Mentioned
}

func endOfDayMS(v int64) int64 {
	t := fromMS(v)
	if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 {
		return ms(endOfDay(t))
	}
	return v
}

func bestDate(u *unit) int64 {
	switch {
	case u.OccStart != 0 && u.OccEnd != 0:
		return u.OccStart + (endOfDayMS(u.OccEnd)-u.OccStart)/2
	case u.OccStart != 0:
		return u.OccStart
	}
	return u.Mentioned
}

// proximityTo is how close a fact sits to the middle of the window, 1 at the
// middle falling to 0 at the edges.
func proximityTo(u *unit, w Window, missing float64) float64 {
	d := bestDate(u)
	if d == 0 {
		return missing
	}
	total := w.End.Sub(w.Start).Hours() / 24
	if total <= 0 {
		return 1
	}
	mid := w.Start.Add(w.End.Sub(w.Start) / 2)
	dist := math.Abs(fromMS(d).Sub(mid).Hours() / 24)
	return 1 - math.Min(dist/(total/2), 1)
}

// temporalArm finds facts in the time window the query names. Entry points
// are the facts inside the window most similar to the query, spread across
// the window so one busy day cannot take every slot; from them activation
// spreads along time and causal links, decaying with each hop, to facts the
// window itself did not catch.
func (c *bankCache) temporalArm(w Window, sims []float32, tb int, visible func(int32) bool) ([]int32, map[int32]float64, map[int32]float64) {
	ws, we := ms(w.Start), ms(w.End)
	type entry struct {
		p   int32
		sim float64
	}
	var pool []entry
	for i := range c.units {
		u := &c.units[i]
		s, e := unitSpan(u)
		if s == 0 || !visible(int32(i)) {
			continue
		}
		in := (s <= we && e >= ws) || (u.Mentioned >= ws && u.Mentioned <= we)
		if !in {
			continue
		}
		sim := 1.0
		if sims != nil {
			sim = float64(sims[i])
			if sim < temporalSimFloor {
				continue
			}
		}
		pool = append(pool, entry{int32(i), sim})
	}
	sort.Slice(pool, func(a, b int) bool {
		if pool[a].sim != pool[b].sim {
			return pool[a].sim > pool[b].sim
		}
		return pool[a].p < pool[b].p
	})
	if len(pool) > temporalPool {
		pool = pool[:temporalPool]
	}
	// Coverage: round-robin over 8 buckets of the window.
	var entries []entry
	if len(pool) <= temporalEntries {
		entries = pool
	} else {
		buckets := make([][]entry, temporalBuckets)
		span := float64(we - ws)
		for _, en := range pool {
			u := &c.units[en.p]
			d := u.OccStart
			if d == 0 {
				d = u.Mentioned
			}
			b := 0
			if span > 0 {
				b = int(float64(d-ws) / span * temporalBuckets)
			}
			b = max(0, min(temporalBuckets-1, b))
			buckets[b] = append(buckets[b], en)
		}
		for tier := 0; len(entries) < temporalEntries; tier++ {
			var row []entry
			for _, bk := range buckets {
				if tier < len(bk) {
					row = append(row, bk[tier])
				}
			}
			if len(row) == 0 {
				break
			}
			sort.SliceStable(row, func(a, b int) bool { return row[a].sim > row[b].sim })
			for _, en := range row {
				if len(entries) < temporalEntries {
					entries = append(entries, en)
				}
			}
		}
	}
	score := map[int32]float64{}
	prox := map[int32]float64{}
	var frontier []int32
	for _, en := range entries {
		p := proximityTo(&c.units[en.p], w, 0.5)
		score[en.p], prox[en.p] = p, p
		frontier = append(frontier, en.p)
	}
	budget := tb - len(entries)
	for it := 0; it < temporalIterations && budget > 0 && len(frontier) > 0; it++ {
		batch := frontier
		if len(batch) > temporalBatch {
			batch, frontier = frontier[:temporalBatch], frontier[temporalBatch:]
		} else {
			frontier = nil
		}
		type hop struct {
			p          int32
			propagated float64
		}
		best := map[int32]float64{}
		for _, src := range batch {
			links := c.temporalNeighbors(src)
			if len(links) > temporalPerSource {
				links = links[:temporalPerSource]
			}
			for _, ln := range links {
				if prop := score[src] * ln.weight * temporalDecay; prop > best[ln.pos] {
					best[ln.pos] = prop
				}
			}
			for _, t := range c.units[src].Causes {
				if prop := score[src] * 1.0 * causalBoost * temporalDecay; prop > best[t] {
					best[t] = prop
				}
			}
		}
		var hops []hop
		for p, v := range best {
			if _, done := score[p]; done || !visible(p) {
				continue
			}
			if sims != nil && float64(sims[p]) < temporalSimFloor {
				continue
			}
			hops = append(hops, hop{p, v})
		}
		sort.Slice(hops, func(a, b int) bool {
			if hops[a].propagated != hops[b].propagated {
				return hops[a].propagated > hops[b].propagated
			}
			return hops[a].p < hops[b].p
		})
		for _, h := range hops {
			if budget <= 0 {
				break
			}
			np := proximityTo(&c.units[h.p], w, 0.3)
			combined := math.Max(np, h.propagated)
			score[h.p], prox[h.p] = combined, np
			budget--
			if combined > temporalContinue {
				frontier = append(frontier, h.p)
			}
		}
	}
	list := make([]int32, 0, len(score))
	for p := range score {
		list = append(list, p)
	}
	sort.Slice(list, func(a, b int) bool {
		if score[list[a]] != score[list[b]] {
			return score[list[a]] > score[list[b]]
		}
		return list[a] < list[b]
	})
	return list, score, prox
}

// ------------------------------------------------------------ scoring

// rerankDoc is the text a reranker scores for a fact: its date and context
// in front of the fact, because "when" and "said where" change what a fact
// answers.
func rerankDoc(u *unit) string {
	doc := u.Text
	if u.Context != "" {
		doc = u.Context + ": " + doc
	}
	if u.OccStart != 0 {
		t := fromMS(u.OccStart)
		doc = fmt.Sprintf("[Date: %s (%s)] %s", t.Format("January 02, 2006"), t.Format("2006-01-02"), doc)
	}
	return doc
}

// normaliseScores maps reranker output onto [0,1]: calibrated scores pass
// through, logits go through a sigmoid.
func normaliseScores(raw []float32) []float64 {
	out := make([]float64, len(raw))
	inUnit := true
	for _, s := range raw {
		if s < 0 || s > 1 {
			inUnit = false
		}
	}
	for i, s := range raw {
		v := float64(s)
		if math.IsNaN(v) {
			out[i] = 0
			continue
		}
		if !inUnit {
			v = 1 / (1 + math.Exp(-v))
		}
		out[i] = v
	}
	return out
}

func linearDecay(days float64) float64 { return math.Max(0.1, math.Min(1, 1-days/365)) }

// coarse reports whether a fact's dates are exactly a calendar month or year
// — a vague date, which should not read as recent just because its period
// ended recently.
func coarse(u *unit) bool {
	if u.OccStart == 0 || u.OccEnd == 0 {
		return false
	}
	s, e := fromMS(u.OccStart), fromMS(u.OccEnd)
	if s.Day() != 1 || s.Hour() != 0 {
		return false
	}
	if s.Month() == 1 && e.Year() == s.Year() && e.Month() == 12 && e.Day() == 31 {
		return true
	}
	last := s.AddDate(0, 1, -1)
	return e.Year() == last.Year() && e.Month() == last.Month() && e.Day() == last.Day()
}

func recency(u *unit, now time.Time) float64 {
	if coarse(u) {
		return math.Min(0.5, linearDecay(now.Sub(fromMS(u.OccEnd)).Hours()/24))
	}
	eff := u.OccStart
	if eff == 0 {
		eff = u.Mentioned
	}
	if eff == 0 {
		eff = u.OccEnd
	}
	if eff == 0 {
		return 0.5
	}
	return linearDecay(now.Sub(fromMS(eff)).Hours() / 24)
}

// ------------------------------------------------------------ authority

// conflicts reports whether a model fact contradicts a person's: it carries a
// challenge naming the human fact (retain records one when it re-extracts
// over a person's correction), or it states a different value for the same
// slot ("the deploy takes 10 minutes" against "... 25 minutes"), or it is the
// model re-deriving, from the same chunk, a fact the person rewrote.
func (c *bankCache) conflicts(h, m *unit) bool {
	if m.Challenges != "" && m.Challenges == h.ID {
		return true
	}
	if _, ok := memory.ValueUpdate(h.Text, m.Text); ok {
		return true
	}
	if sameSourceRewrite(h.ID, h.Doc, h.Chunk, h.Text, m.ID, m.Doc, m.Chunk, m.Text) {
		return true
	}
	// A different value or a denial about the same entity in another document.
	return h.Doc != m.Doc && ruleConflict(h.Text, h.Entities, m.Text, m.Entities)
}

// sameSourceRewrite is the signature of a person's correction meeting a
// model's re-extraction: both from the same chunk of the same document, and
// either the model's fact carries the very id the person's edit kept (the
// model wrote the original text again) or the two are about the same thing in
// different words.
func sameSourceRewrite(hID, hDoc string, hChunk int, hText, mID, mDoc string, mChunk int, mText string) bool {
	if hDoc == "" || hDoc != mDoc || hChunk < 0 || hChunk != mChunk {
		return false
	}
	if memory.Normalize(hText) == memory.Normalize(mText) {
		return false // the same fact, not a contradiction
	}
	if baseID(hID) == baseID(mID) {
		return true
	}
	if _, same := memory.SameSlot(hText, mText); same {
		return true
	}
	return memory.Similarity(hText, mText) >= 0.5
}

// baseID drops the "-N" a duplicate id is suffixed with.
func baseID(id string) string {
	if i := strings.LastIndex(id, "-"); i > 0 {
		if _, err := atoi(id[i+1:]); err == nil {
			return id[:i]
		}
	}
	return id
}

// applyAuthority enforces that a person's fact ranks above any model fact it
// contradicts. The model fact is kept and marked disputed — the disagreement
// is worth seeing — but it can never be the first thing a reader meets on
// that question, and the human fact is pulled into the results if retrieval
// had not found it.
//
// This is the recall half of "what a person writes wins". The write half is
// in retain (human facts survive re-extraction) and in the file format
// (authorship is read from the facts file, so a reindex keeps it).
func (c *bankCache) applyAuthority(cands []*candidate, visible func(int32) bool, tr *Trace) []*candidate {
	if len(c.humans) == 0 || len(cands) == 0 {
		return cands
	}
	in := map[int32]*candidate{}
	for _, cd := range cands {
		in[cd.pos] = cd
	}
	limit := min(len(cands), 600)
	changed := false
	for _, cd := range cands[:limit] {
		m := &c.units[cd.pos]
		if m.Human {
			continue
		}
		for _, hp := range c.humans {
			h := &c.units[hp]
			if hp == cd.pos || !visible(hp) || !c.conflicts(h, m) {
				continue
			}
			cd.disputed = h.ID
			if tr != nil {
				if tr.Disputes == nil {
					tr.Disputes = map[string]string{}
				}
				tr.Disputes[m.ID] = h.ID
			}
			hc, ok := in[hp]
			if !ok {
				hc = &candidate{pos: hp, ranks: map[string]int{"authority": 1}, reranker: fptr(cd.weight)}
				in[hp] = hc
				cands = append(cands, hc)
			}
			if hc.weight <= cd.weight {
				hc.weight = cd.weight * 1.0001
				if hc.weight == 0 {
					hc.weight = 1e-9
				}
			}
			changed = true
			break
		}
	}
	if !changed {
		return cands
	}
	// A disputed fact sits just below every human fact that disputes it.
	for _, cd := range cands {
		if cd.disputed == "" {
			continue
		}
		if hp, ok := c.byID[cd.disputed]; ok {
			if hc := in[hp]; hc != nil && cd.weight >= hc.weight {
				cd.weight = hc.weight * 0.9999
			}
		}
	}
	sortCands(cands)
	return cands
}

// ------------------------------------------------------------ chunks

func (e *Engine) recallChunks(c *bankCache, bankID string, cands []*candidate, budget int) (map[string]ChunkOut, error) {
	type key struct {
		doc string
		idx int
	}
	var order []key
	seen := map[key]bool{}
	for _, cd := range cands {
		u := &c.units[cd.pos]
		if u.Chunk < 0 || u.Doc == "" {
			continue
		}
		k := key{u.Doc, u.Chunk}
		if !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
	}
	out := map[string]ChunkOut{}
	used := 0
	for _, k := range order {
		var text string
		err := e.Index.DB.QueryRow("SELECT text FROM bank_chunks WHERE bank=? AND doc=? AND idx=?",
			bankID, k.doc, k.idx).Scan(&text)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		id := ChunkID(bankID, k.doc, k.idx)
		t := CountTokens(text)
		if used+t <= budget {
			out[id] = ChunkOut{ID: id, Text: text, Index: k.idx, Document: k.doc}
			used += t
			continue
		}
		// The first chunk that does not fit is cut to what remains, and
		// packing stops.
		if room := budget - used; room > 0 {
			out[id] = ChunkOut{ID: id, Text: truncateTokens(text, room), Index: k.idx, Document: k.doc, Truncated: true}
		}
		break
	}
	return out, nil
}

func truncateTokens(s string, n int) string {
	words := strings.Fields(s)
	var b strings.Builder
	used := 0
	for _, w := range words {
		t := CountTokens(w)
		if used+t > n {
			break
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(w)
		used += t
	}
	return b.String()
}
