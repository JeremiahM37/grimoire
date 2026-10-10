// Package replay is CI for memory. Every context injection is a situation (a
// request or an about-to-run action) and a set of memories that fired for it,
// some of which turned out useful (the agent cited or followed them). Before
// a change to the store lands, the situations are replayed against the store
// as it is and as it would be, and the two are compared: a memory that used to
// fire and was useful and now does not is a regression; a memory that starts
// firing where nothing useful fired is noise.
//
// The replay re-runs the ranking of GET /api/memory/context (hybrid relevance
// from embedding similarity and term overlap, learned and agent cues, the
// action-stage path triggers, min_rel and the item limit) over an in-memory
// snapshot. It makes no model calls and no writes: situation vectors are
// cached, a proposed store is an overlay on a snapshot, and only the items a
// change touches are rescored. See docs/MEMORY_REPLAY.md.
package replay

import (
	"math"
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/cues"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Part is one embedded span of a memory: a stored fact is one part, a note is
// one part per chunk. Vec is unit length.
type Part struct {
	Text  string
	Vec   []float32
	terms map[string]struct{}
	// size is the bytes this part would take in an injected response.
	size int
}

// Item is one injectable memory. Target is "fact:<id>" or "note:<path>", the
// same identity the injection log and the cue store use.
type Item struct {
	Target string
	Text   string // what injection shows; used to drop duplicates
	Parts  []Part
	// Path is the note the memory lives in (the source shown after the text).
	Path string
}

// CueRec is a cue with its unit vector.
type CueRec struct {
	cues.Cue
	Vec   []float32
	terms map[string]struct{}
	key   string // normalised text, for telling a situation's own cue
}

// textKey collapses case and whitespace.
func textKey(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// sameSituation reports whether a cue is the situation it is being matched
// against: the same words, allowing for the cue having been cut shorter.
func sameSituation(cue, sit string) bool {
	if cue == sit {
		return true
	}
	short, long := cue, sit
	if len(short) > len(long) {
		short, long = long, short
	}
	return len(short) >= 40 && strings.HasPrefix(long, short)
}

// NewItem builds an item from texts and their (not necessarily normalised)
// vectors. A part with no vector can only match on cues.
func NewItem(target, text string, texts []string, vecs [][]float32) *Item {
	return NewItemAt(target, "", text, texts, vecs)
}

// markerRoom mirrors internal/api: the most a "m:<tag> " marker adds.
const markerRoom = 11

// PreambleBytes is the size of the directive preamble, charged to the first
// item of a response. internal/api sets it.
var PreambleBytes = 0

// MaxInjectedRunes is how much of a note chunk injection shows.
const MaxInjectedRunes = 600

// NewItemAt is NewItem with the source path, which injection shows and which
// counts against the byte budget.
func NewItemAt(target, path, text string, texts []string, vecs [][]float32) *Item {
	it := &Item{Target: target, Text: text, Path: path}
	if path == "" {
		if rest, ok := strings.CutPrefix(target, "note:"); ok {
			it.Path = rest
		}
	}
	id := ""
	if rest, ok := strings.CutPrefix(target, "fact:"); ok {
		id = "#" + rest
	}
	for i, t := range texts {
		shown := strings.Join(strings.Fields(t), " ")
		if strings.HasPrefix(target, "note:") {
			if r := []rune(shown); len(r) > MaxInjectedRunes {
				shown = string(r[:MaxInjectedRunes]) + "…"
			}
		}
		p := Part{Text: t, terms: termSet(t), size: len(shown) + len(it.Path) + len(id) + 7 + markerRoom}
		if i < len(vecs) {
			p.Vec = Unit(vecs[i])
		}
		it.Parts = append(it.Parts, p)
	}
	return it
}

// NewCue wraps a cue and its vector.
func NewCue(c cues.Cue, vec []float32) CueRec {
	return CueRec{Cue: c, Vec: Unit(vec), terms: termSet(c.Text), key: textKey(c.Text)}
}

// Unit returns v scaled to length 1 (a copy). An empty or zero vector comes
// back nil.
func Unit(v []float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return nil
	}
	inv := float32(1 / math.Sqrt(n))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

func dot(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += a[i] * b[i]
	}
	return float64(s0 + s1 + s2 + s3)
}

// The word list and the shape of contextTerms in internal/api; a parity test
// there keeps the two identical.
var noise = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields("please can could would should will do does did how what when where why which who me my we our you your it this that these those help want need now just also really anything something tell explain use using work working fix add make get know thanks thank okay ok yes no continue proceed hello hi") {
		m[w] = true
	}
	return m
}()

// Terms are the query terms the context ranking counts overlap against.
func Terms(query string) []string {
	ignored := map[string]bool{}
	for w := range noise {
		ignored[w] = true
	}
	var terms []string
	for _, term := range memory.Tokens(query) {
		if len(term) < 3 || ignored[term] {
			continue
		}
		ignored[term] = true
		terms = append(terms, term)
		if len(terms) == 24 {
			break
		}
	}
	return terms
}

func termSet(text string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, t := range memory.Tokens(text) {
		m[t] = struct{}{}
	}
	return m
}

func overlap(terms []string, have map[string]struct{}) float64 {
	if len(terms) == 0 {
		return 1
	}
	matched := 0
	for _, t := range terms {
		if _, ok := have[t]; ok {
			matched++
		}
	}
	if matched == 0 || (len(terms) > 1 && matched < 2) {
		return 0
	}
	return float64(matched) / float64(len(terms))
}

// Relevance is the hybrid relevance of internal/api (relevance).
func Relevance(cosine, overlap float64) float64 {
	c := math.Min(1, math.Max(0, (cosine-0.45)/0.35))
	return math.Max(c, 0.6*c+0.4*overlap)
}

// CueLow is where a cue's cosine stretch starts (cueRelevance's default).
const CueLow = 0.55

// CueRelevance is internal/api's cueRelevance.
func CueRelevance(cosine, overlap, lo float64) float64 {
	c := math.Min(1, math.Max(0, (cosine-lo)/0.35))
	return math.Max(c, 0.6*c+0.4*overlap)
}

func clamp01(f float64) float64 { return math.Min(1, math.Max(0, f)) }

// Snapshot is the set of injectable memories and cues at one moment. It is
// immutable once built; Overlay returns a new one sharing everything the
// change does not touch.
type Snapshot struct {
	items map[string]*Item
	cues  map[string][]CueRec
	trig  *cues.TriggerIndex
}

// NewSnapshot builds a snapshot.
func NewSnapshot(items []*Item, cs []CueRec) *Snapshot {
	sn := &Snapshot{items: make(map[string]*Item, len(items)), cues: map[string][]CueRec{}}
	for _, it := range items {
		sn.items[it.Target] = it
	}
	for _, c := range cs {
		sn.cues[c.Target] = append(sn.cues[c.Target], c)
	}
	sn.trig = cues.NewTriggerIndex(sn.allCues())
	return sn
}

// Len is the number of memories.
func (sn *Snapshot) Len() int { return len(sn.items) }

// Has reports whether a target is in the snapshot.
func (sn *Snapshot) Has(target string) bool { _, ok := sn.items[target]; return ok }

// Item returns a target's item.
func (sn *Snapshot) Item(target string) *Item { return sn.items[target] }

// Targets lists every target, sorted.
func (sn *Snapshot) Targets() []string {
	out := make([]string, 0, len(sn.items))
	for t := range sn.items {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func (sn *Snapshot) allCues() []cues.Cue {
	var out []cues.Cue
	for _, cs := range sn.cues {
		for _, c := range cs {
			out = append(out, c.Cue)
		}
	}
	return out
}

// Change is a proposed edit to the store. It is data only: the caller embeds
// new texts. Nothing here writes anything.
type Change struct {
	// Upsert adds or replaces memories by target.
	Upsert []*Item
	// Remove drops memories (and their cues, unless Remap carries them).
	Remove []string
	// Remap says memory Old was replaced by New (a merge, a supersession, a
	// rewrite that re-minted an id). Old's cues are carried to New, and a
	// situation that expected Old counts New firing as Old kept.
	Remap map[string]string
}

// Empty reports whether the change does nothing.
func (c Change) Empty() bool { return len(c.Upsert) == 0 && len(c.Remove) == 0 && len(c.Remap) == 0 }

// resolve follows Remap chains to the final target (cycle-safe).
func (c Change) resolve(t string) string {
	for i := 0; i < 8; i++ {
		n, ok := c.Remap[t]
		if !ok || n == t {
			return t
		}
		t = n
	}
	return t
}

// Touched is every target whose score can differ after the change.
func (c Change) Touched() map[string]bool {
	m := map[string]bool{}
	for _, it := range c.Upsert {
		m[it.Target] = true
	}
	for _, t := range c.Remove {
		m[t] = true
	}
	for o, n := range c.Remap {
		m[o] = true
		m[n] = true
	}
	return m
}

// Overlay returns the snapshot as it would be after the change.
func (sn *Snapshot) Overlay(c Change) *Snapshot {
	out := &Snapshot{items: make(map[string]*Item, len(sn.items)+len(c.Upsert)),
		cues: make(map[string][]CueRec, len(sn.cues))}
	for k, v := range sn.items {
		out.items[k] = v
	}
	for k, v := range sn.cues {
		out.cues[k] = v
	}
	actionTouched := false
	hasAction := func(cs []CueRec) bool {
		for _, x := range cs {
			if x.Kind == cues.Action {
				return true
			}
		}
		return false
	}
	// Carry cues first: the old target is about to disappear.
	for old, nw := range c.Remap {
		if old == nw {
			continue
		}
		carried := out.cues[old]
		if len(carried) == 0 {
			continue
		}
		have := map[string]bool{}
		for _, x := range out.cues[nw] {
			have[strings.ToLower(strings.TrimSpace(x.Text))] = true
		}
		merged := append([]CueRec(nil), out.cues[nw]...)
		for _, x := range carried {
			if have[strings.ToLower(strings.TrimSpace(x.Text))] {
				continue
			}
			x.Target = nw
			merged = append(merged, x)
		}
		out.cues[nw] = merged
		actionTouched = actionTouched || hasAction(carried)
	}
	for _, t := range c.Remove {
		delete(out.items, t)
		actionTouched = actionTouched || hasAction(out.cues[t])
		delete(out.cues, t)
	}
	for old, nw := range c.Remap {
		if old != nw {
			delete(out.items, old)
			delete(out.cues, old)
		}
	}
	for _, it := range c.Upsert {
		out.items[it.Target] = it
	}
	if actionTouched {
		out.trig = cues.NewTriggerIndex(out.allCues())
	} else {
		out.trig = sn.trig
	}
	return out
}

// Prepared is a situation ready to score: its vector is unit length and its
// terms are counted once.
type Prepared struct {
	Situation
	terms []string
	vec   []float32
	key   string
}

// Prepare readies a situation. A situation without a vector can only match on
// terms, which scores nothing under Relevance; callers embed first.
func Prepare(s Situation) *Prepared {
	if s.MinRel <= 0 {
		s.MinRel = DefaultMinRel(s.Stage)
	}
	return &Prepared{Situation: s, terms: Terms(s.Text), vec: Unit(s.Vec), key: textKey(s.Text)}
}

// score is the context relevance of one memory for one situation. The text
// and cue scores are kept apart: the endpoint only considers the strongest
// text matches as candidates, but every cue match.
func (sn *Snapshot) score(it *Item, p *Prepared, triggered map[string]bool) Scored {
	sc := Scored{Target: it.Target}
	isNote := strings.HasPrefix(it.Target, "note:") || strings.HasPrefix(it.Target, "ghost:")
	for i := range it.Parts {
		pt := &it.Parts[i]
		cos := clamp01(dot(p.vec, pt.Vec))
		r := Relevance(cos, overlap(p.terms, pt.terms))
		if r > sc.Text || i == 0 {
			sc.Text, sc.Size = r, pt.size
		}
		if isNote && r >= floorFor(p) {
			sc.Chunks = append(sc.Chunks, chunkScore{r, pt.size})
		}
	}
	if len(sc.Chunks) > 1 {
		sort.Slice(sc.Chunks, func(a, b int) bool { return sc.Chunks[a].S > sc.Chunks[b].S })
	}
	if len(sc.Chunks) > maxChunks {
		sc.Chunks = sc.Chunks[:maxChunks]
	}
	for _, c := range sn.cues[it.Target] {
		// A cue learned from this very situation would make it pass whatever
		// happens to the memory's text. Leave it out, as in leave-one-out
		// validation: the replay asks whether the memory still fires through
		// its text and the cues taught by other situations.
		if sameSituation(c.key, p.key) {
			continue
		}
		if r := CueRelevance(dot(p.vec, c.Vec), overlap(p.terms, c.terms), CueLow); r > sc.Cue {
			sc.Cue = r
		}
	}
	if triggered[it.Target] {
		sc.Cue = 1
	}
	sc.Score = math.Max(sc.Text, sc.Cue)
	return sc
}

func (sn *Snapshot) triggered(p *Prepared) map[string]bool {
	if p.Stage != "action" {
		return nil
	}
	ts := sn.trig.Triggered(p.Text)
	if len(ts) == 0 {
		return nil
	}
	m := make(map[string]bool, len(ts))
	for _, t := range ts {
		m[t] = true
	}
	return m
}

// Scored is one memory's relevance for a situation.
type Scored struct {
	Target string
	Score  float64 // the larger of Text and Cue
	Text   float64 // relevance of the memory's own text
	Cue    float64 // relevance of its best cue (1 when an action cue names the path)
	Size   int     // bytes it takes when injected
	// Chunks are a note's strongest chunk scores, best first: retrieval takes
	// the top chunks of the whole corpus, so a note's text score depends on
	// which of its chunks made that cut.
	Chunks []chunkScore
}

type chunkScore struct {
	S    float64
	Size int
}

// maxChunks is how many chunk scores are kept per note.
const maxChunks = 8

// floorFor is the lowest score worth keeping for a situation: anything under
// it cannot fire at the situation's min_rel.
func floorFor(p *Prepared) float64 {
	f := p.MinRel * 0.5
	if f > 0.25 {
		f = 0.25
	}
	return f
}

// Score ranks every memory for a situation, strongest first, keeping those
// that could still fire. only, when not nil, restricts the work to those
// targets.
func (sn *Snapshot) Score(p *Prepared, only map[string]bool) []Scored {
	trig := sn.triggered(p)
	floor := floorFor(p)
	var out []Scored
	if only != nil {
		for t := range only {
			if it, ok := sn.items[t]; ok {
				if sc := sn.score(it, p, trig); sc.Score >= floor {
					out = append(out, sc)
				}
			}
		}
	} else {
		for _, it := range sn.items {
			if sc := sn.score(it, p, trig); sc.Score >= floor {
				out = append(out, sc)
			}
		}
	}
	sortScored(out)
	return out
}

func sortScored(s []Scored) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Score != s[j].Score {
			return s[i].Score > s[j].Score
		}
		return s[i].Target < s[j].Target
	})
}

// DefaultLimit is how many memories one response carries: the prompt-stage
// default of the context endpoint, and the action-stage hook's.
func (p *Prepared) limit() int {
	if p.Limit > 0 {
		return p.Limit
	}
	if p.Stage == "action" {
		return 2
	}
	return 5
}

// How many text matches the endpoint considers: stored facts by their own
// ranking (Limit 40), notes by fused retrieval (30 chunks, one per note).
const (
	factCandidates = 40
	noteCandidates = 30
)

// DefaultBudget is the byte budget of one response.
func DefaultBudget(stage string) int {
	if stage == "action" {
		return 1200
	}
	return 2400
}

// Fire picks what the context endpoint would inject: only the strongest text
// matches are candidates (plus every cue match), strongest first, at or above
// min_rel, no two with the same text, within the byte budget, at most the
// limit.
func (sn *Snapshot) Fire(p *Prepared, ranked []Scored) []Scored {
	// Facts: the strongest factCandidates by their own text.
	var facts []int
	type chunkRef struct {
		i int
		c chunkScore
	}
	var chunks []chunkRef
	for i := range ranked {
		switch {
		case strings.HasPrefix(ranked[i].Target, "fact:"):
			facts = append(facts, i)
		default:
			for _, c := range ranked[i].Chunks {
				chunks = append(chunks, chunkRef{i, c})
			}
		}
	}
	sort.SliceStable(facts, func(a, b int) bool { return ranked[facts[a]].Text > ranked[facts[b]].Text })
	candidate := make(map[int]bool, len(ranked))
	for k, i := range facts {
		if k < factCandidates {
			candidate[i] = true
		}
	}
	// Notes: retrieval returns the best chunks of the whole corpus, then keeps
	// each note's strongest. A note is a candidate only through a chunk that
	// made the cut, and its text score is that chunk's.
	sort.SliceStable(chunks, func(a, b int) bool { return chunks[a].c.S > chunks[b].c.S })
	if len(chunks) > noteCandidates {
		chunks = chunks[:noteCandidates]
	}
	noteText := map[int]chunkScore{}
	for _, ch := range chunks {
		if _, ok := noteText[ch.i]; !ok {
			noteText[ch.i] = ch.c
		}
	}
	ranked = append([]Scored(nil), ranked...)
	for i := range ranked {
		if strings.HasPrefix(ranked[i].Target, "fact:") {
			continue
		}
		if ch, ok := noteText[i]; ok && !strings.HasPrefix(ranked[i].Target, "ghost:") {
			candidate[i] = true
			ranked[i].Text, ranked[i].Size = ch.S, ch.Size
		}
	}
	eff := make([]Scored, 0, len(ranked))
	for i, sc := range ranked {
		if !candidate[i] {
			sc.Score = sc.Cue
		} else if sc.Cue > sc.Text {
			sc.Score = sc.Cue
		} else {
			sc.Score = sc.Text
		}
		if sc.Score >= p.MinRel {
			eff = append(eff, sc)
		}
	}
	sortScored(eff)
	budget := p.Budget
	if budget <= 0 {
		budget = DefaultBudget(p.Stage)
	}
	var out []Scored
	seen := map[string]bool{}
	used := 0
	for _, sc := range eff {
		it := sn.items[sc.Target]
		if it == nil {
			continue
		}
		norm := memory.Normalize(it.Text)
		if seen[norm] {
			continue
		}
		prefix := 0
		if len(out) == 0 {
			prefix = PreambleBytes
		}
		if used+prefix+sc.Size > budget {
			continue
		}
		used += prefix + sc.Size
		seen[norm] = true
		out = append(out, sc)
		if len(out) == p.limit() {
			break
		}
	}
	return out
}

// DefaultMinRel is the relevance floor of the hook at a stage: 0.5 on a
// prompt, 0.7 on a tool call.
func DefaultMinRel(stage string) float64 {
	if stage == "action" {
		return 0.7
	}
	return 0.5
}
