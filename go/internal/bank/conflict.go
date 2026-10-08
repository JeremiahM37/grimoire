package bank

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Cross-document conflicts between a person's fact and a model's.
//
// ValueUpdate (in memory) already catches a changed number. This catches the
// rest: "Alice lives in Lyon" (a person wrote it) against "Alice lives in
// Paris" (a model extracted it from another document), or a fact against its
// own negation. A model fact found to contradict a person's is recorded as a
// challenge (chal=<human id>) in its file, so the disagreement survives a
// rebuild and recall and reflect show it as disputed with the person's fact
// first. Nothing is deleted and the person's fact is never touched.
//
// The model is optional: with one configured it judges the candidate pairs;
// without one (or when it fails) the rules below decide.

// singleValued are the predicates that take one value at a time.
var singleValued = map[string]bool{
	"lives in": true, "works at": true, "is called": true, "is named": true,
	"prefers": true, "prefer": true, "drives": true,
}

func lowerSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[strings.ToLower(strings.TrimSpace(x))] = true
	}
	return m
}

func sharedEntities(a, b []string) []string {
	bs := lowerSet(b)
	var out []string
	for _, x := range a {
		if bs[strings.ToLower(strings.TrimSpace(x))] {
			out = append(out, strings.ToLower(strings.TrimSpace(x)))
		}
	}
	return out
}

func stemWords(s string, drop map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, w := range memory.Tokens(s) {
		w = strings.TrimSuffix(w, "s")
		if w == "" || drop[w] {
			continue
		}
		out[w] = true
	}
	return out
}

// stripNegation removes negation phrases so a statement can be compared with
// its affirmative form.
func stripNegation(text string) string {
	n := " " + memory.Normalize(text) + " "
	for _, neg := range []string{"no longer", "not anymore", "no more", "does not", "doesn t", "doesnt", "is not",
		"isn t", "isnt", "never again", "never", "not", "don t", "dont", "didn t", "didnt", "stopped", "quit", "gave up"} {
		n = strings.ReplaceAll(n, " "+neg+" ", " ")
	}
	return strings.TrimSpace(n)
}

// ruleConflict reports whether two statements about a shared entity disagree:
// the same single-valued attribute with different values, a statement against
// its negation, or the same slot with a different value.
func ruleConflict(hText string, hEnts []string, mText string, mEnts []string) bool {
	shared := sharedEntities(hEnts, mEnts)
	if len(shared) == 0 || memory.Normalize(hText) == memory.Normalize(mText) {
		return false
	}
	if hs, hp, hv, ok := memory.Attribute(hText); ok {
		if ms, mp, mv, ok := memory.Attribute(mText); ok && hs == ms && hp == mp && singleValued[hp] &&
			hv != mv && !strings.Contains(hv, mv) && !strings.Contains(mv, hv) {
			return true
		}
	}
	if memory.IsNegation(hText) != memory.IsNegation(mText) {
		a, b := hText, mText
		if memory.IsNegation(a) {
			a = stripNegation(a)
		} else {
			b = stripNegation(b)
		}
		drop := lowerSet(shared)
		sa, sb := stemWords(a, drop), stemWords(b, drop)
		inter := 0
		for w := range sa {
			if sb[w] {
				inter++
			}
		}
		if inter > 0 && float64(inter)/float64(len(sa)+len(sb)-inter) >= 0.5 {
			return true
		}
		return false
	}
	if _, same := memory.SameSlot(hText, mText); same {
		drop := lowerSet(shared)
		sa, sb := stemWords(hText, drop), stemWords(mText, drop)
		common, ua, ub := 0, 0, 0
		for w := range sa {
			if sb[w] {
				common++
			} else {
				ua++
			}
		}
		for w := range sb {
			if !sa[w] {
				ub++
			}
		}
		if ua >= 1 && ub >= 1 && ua <= 2 && ub <= 2 && common >= 2 && float64(common)/float64(common+max(ua, ub)) >= 0.6 {
			return true
		}
	}
	return false
}

// conflictCandidate is a model fact and the person's fact it may contradict.
type conflictCandidate struct {
	fact int // index into the added facts
	h    *unit
}

// llmConflicts asks the model which candidate pairs contradict. ok is false
// when no answer could be had, so the caller falls back to the rules.
func (e *Engine) llmConflicts(ctx context.Context, added []Fact, cands []conflictCandidate) (map[int]bool, bool) {
	if !e.AI.Available() || len(cands) == 0 {
		return nil, false
	}
	var b strings.Builder
	for i, c := range cands {
		fmt.Fprintf(&b, "%d. A: %s\n   B: %s\n", i, clip(c.h.Text, 300), clip(added[c.fact].Text, 300))
	}
	client := e.AI.WithSurface("bank.conflicts", "")
	comp, err := client.CompleteWith(ctx, b.String(), ai.CompleteOpts{
		System: "For each numbered pair, decide whether statements A and B cannot both be true of the same person " +
			"or thing at the same time (a different value for one attribute, or one denies the other). Related " +
			"or additive facts do not contradict. Reply with JSON {\"contradict\":[<numbers>]}.",
		Temperature: ai.Temp(0), MaxTokens: 300, JSON: true})
	if err != nil {
		return nil, false
	}
	var out struct {
		Contradict []int `json:"contradict"`
	}
	if json.Unmarshal([]byte(extractJSONObject(comp.Text)), &out) != nil {
		return nil, false
	}
	res := map[int]bool{}
	for _, n := range out.Contradict {
		res[n] = true
	}
	return res, true
}

// challengeAcrossDocuments marks each new model fact that contradicts a
// person's fact from another document with chal=<that fact's id>.
func (e *Engine) challengeAcrossDocuments(ctx context.Context, c *bankCache, docID string, added []Fact) {
	if len(c.humans) == 0 {
		return
	}
	var cands []conflictCandidate
	for k := range added {
		if added[k].Challenges != "" || len(added[k].Entities) == 0 {
			continue
		}
		for _, hp := range c.humans {
			h := &c.units[hp]
			if h.Doc == docID || len(sharedEntities(h.Entities, added[k].Entities)) == 0 {
				continue
			}
			cands = append(cands, conflictCandidate{k, h})
		}
	}
	if len(cands) == 0 {
		return
	}
	if len(cands) > 24 {
		cands = cands[:24]
	}
	verdict, ok := e.llmConflicts(ctx, added, cands)
	for i, cd := range cands {
		if added[cd.fact].Challenges != "" {
			continue
		}
		hit := false
		if ok {
			hit = verdict[i]
		} else {
			hit = ruleConflict(cd.h.Text, cd.h.Entities, added[cd.fact].Text, added[cd.fact].Entities)
		}
		if hit {
			added[cd.fact].Challenges = cd.h.ID
		}
	}
}
