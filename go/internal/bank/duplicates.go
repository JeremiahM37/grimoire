package bank

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Near-duplicate review. A bank accumulates facts and observations that say
// nearly the same thing in different words. DuplicateCandidates lists such
// pairs for a person (or an agent) to look at; MergeDuplicates folds one into
// the other on request. Nothing merges by itself.
//
// Candidates are scored by IDF-weighted overlap, so two sentences sharing only
// the bank's common words ("the user said", "project") are not candidates, and
// a veto drops pairs that share fewer than two informative words. Pairs that
// are updates (a changed value) or denials of each other, or that are already
// recorded as a challenge, are contradictions and are left to those paths.
//
// A merge never removes text. The merged-away entry is struck through and kept
// in its file, with the id of the entry it was merged into: a fact as a prose
// line in its facts file, an observation in the file's History section.

// DuplicateSide is one half of a pair.
type DuplicateSide struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Human bool   `json:"human,omitempty"`
}

// DuplicateOut is the JSON form of a candidate.
type DuplicateOut struct {
	Type   string        `json:"type"`
	Score  float64       `json:"score"`
	Keep   DuplicateSide `json:"keep"`
	Merge  DuplicateSide `json:"merge"`
	Shared []string      `json:"shared"`
}

// DuplicateQuery tunes the listing.
type DuplicateQuery struct {
	MinScore float64 // default 0.6
	Limit    int     // default 20, at most 200
	Types    string  // "", fact or observation
}

const (
	dupMaxUnits      = 6000
	dupCommonShare   = 0.30 // a word in more than this share of entries is "common"
	dupMinInformativ = 2
)

func (c *bankCache) dupTokens(u *unit) map[string]bool {
	out := map[string]bool{}
	for _, w := range memory.Tokens(u.Text) {
		out[strings.TrimSuffix(w, "s")] = true
	}
	return out
}

// DuplicateCandidates lists near-duplicate pairs, most similar first.
func (e *Engine) DuplicateCandidates(bankID string, q DuplicateQuery) ([]DuplicateOut, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, err
	}
	c, err := e.cache(bankID)
	if err != nil {
		return nil, err
	}
	if q.MinScore <= 0 {
		q.MinScore = 0.6
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 20
	}
	var pos []int
	for i := range c.units {
		u := &c.units[i]
		isObs := u.Type == "observation"
		if q.Types == "fact" && isObs || q.Types == "observation" && !isObs {
			continue
		}
		pos = append(pos, i)
	}
	if len(pos) > dupMaxUnits {
		pos = pos[:dupMaxUnits]
	}
	toks := make([]map[string]bool, len(pos))
	df := map[string]int{}
	for k, p := range pos {
		toks[k] = c.dupTokens(&c.units[p])
		for w := range toks[k] {
			df[w]++
		}
	}
	n := float64(len(pos))
	idf := func(w string) float64 { return math.Log(1 + n/float64(df[w])) }
	common := func(w string) bool { return n >= 10 && float64(df[w])/n > dupCommonShare }
	post := map[string][]int{}
	for k := range pos {
		for w := range toks[k] {
			if !common(w) && df[w] <= 40 {
				post[w] = append(post[w], k)
			}
		}
	}
	type pair struct{ a, b int }
	seen := map[pair]bool{}
	var out []DuplicateOut
	for _, list := range post {
		for x := 0; x < len(list); x++ {
			for y := x + 1; y < len(list); y++ {
				p := pair{list[x], list[y]}
				if seen[p] {
					continue
				}
				seen[p] = true
				ua, ub := &c.units[pos[p.a]], &c.units[pos[p.b]]
				if (ua.Type == "observation") != (ub.Type == "observation") {
					continue
				}
				if c.dupExcluded(ua, ub) {
					continue
				}
				var inter, union float64
				var shared []string
				informative := 0
				for w := range toks[p.a] {
					if toks[p.b][w] {
						inter += idf(w)
						if !common(w) {
							informative++
							shared = append(shared, w)
						}
					}
					union += idf(w)
				}
				for w := range toks[p.b] {
					if !toks[p.a][w] {
						union += idf(w)
					}
				}
				if union == 0 {
					continue
				}
				score := inter / union
				// The veto: shared common words alone never make a candidate.
				need := dupMinInformativ
				if len(toks[p.a]) <= 3 || len(toks[p.b]) <= 3 {
					need = 1
				}
				if score < q.MinScore || informative < need {
					continue
				}
				keep, drop := dupOrder(ua, ub)
				sort.Strings(shared)
				kind := "fact"
				if ua.Type == "observation" {
					kind = "observation"
				}
				out = append(out, DuplicateOut{Type: kind, Score: math.Round(score*1000) / 1000,
					Keep:  DuplicateSide{keep.ID, keep.Text, keep.Human},
					Merge: DuplicateSide{drop.ID, drop.Text, drop.Human}, Shared: shared})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Keep.ID+out[i].Merge.ID < out[j].Keep.ID+out[j].Merge.ID
	})
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// dupExcluded: pairs that are contradictions, not repeats.
func (c *bankCache) dupExcluded(a, b *unit) bool {
	if a.DocRemoved || b.DocRemoved {
		return true
	}
	if a.Challenges == b.ID || b.Challenges == a.ID {
		return true
	}
	if memory.IsNegation(a.Text) != memory.IsNegation(b.Text) {
		return true
	}
	if _, ok := memory.ValueUpdate(a.Text, b.Text); ok {
		return true
	}
	return len(a.Entities) > 0 && len(b.Entities) > 0 && ruleConflict(a.Text, a.Entities, b.Text, b.Entities)
}

// dupOrder picks the entry to keep: a person's, then the better supported,
// then the older (earlier id order stands in for age when undated).
func dupOrder(a, b *unit) (keep, drop *unit) {
	switch {
	case a.Human != b.Human:
		if a.Human {
			return a, b
		}
		return b, a
	case a.Proof != b.Proof:
		if a.Proof > b.Proof {
			return a, b
		}
		return b, a
	}
	if a.Mentioned != 0 && b.Mentioned != 0 && a.Mentioned != b.Mentioned {
		if a.Mentioned < b.Mentioned {
			return a, b
		}
		return b, a
	}
	if a.ID < b.ID {
		return a, b
	}
	return b, a
}

// MergeResult is what a merge did.
type MergeResult struct {
	Type   string `json:"type"`
	Kept   string `json:"kept"`
	Merged string `json:"merged"`
	// Struck is where the merged-away text now lives, struck through.
	Struck string `json:"struck"`
}

// MergeDuplicates folds the entry `merge` into `keep`. The two must be of the
// same kind. A person's entry cannot be merged into a model's, and the merged
// text is struck through and kept, never deleted.
func (e *Engine) MergeDuplicates(bankID, keepID, mergeID string) (*MergeResult, error) {
	if keepID == "" || mergeID == "" || keepID == mergeID {
		return nil, invalid("keep and merge must be two different ids")
	}
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()
	if _, err := e.Profile(bankID); err != nil {
		return nil, err
	}
	c, err := e.cache(bankID)
	if err != nil {
		return nil, err
	}
	kp, ok1 := c.byID[keepID]
	mp, ok2 := c.byID[mergeID]
	if !ok1 || !ok2 {
		return nil, ErrNotFound
	}
	k, m := c.units[kp], c.units[mp]
	if (k.Type == "observation") != (m.Type == "observation") {
		return nil, invalid("a fact cannot be merged with an observation")
	}
	if m.Human && !k.Human {
		return nil, fmt.Errorf("%w: keep the person's version and merge the model's into it", ErrHumanProtected)
	}
	now := e.now()
	if k.Type == "observation" {
		return e.mergeObservations(bankID, keepID, mergeID, now)
	}
	return e.mergeFacts(bankID, &k, &m, now)
}

func (e *Engine) mergeObservations(bankID, keepID, mergeID string, now time.Time) (*MergeResult, error) {
	of, old, err := e.readObservations(bankID)
	if err != nil {
		return nil, err
	}
	ki, mi := -1, -1
	for i, o := range of.Current {
		switch o.ID {
		case keepID:
			ki = i
		case mergeID:
			mi = i
		}
	}
	if ki < 0 || mi < 0 {
		return nil, ErrNotFound
	}
	loser := of.Current[mi]
	have := map[string]bool{}
	for _, s := range of.Current[ki].Sources {
		have[s] = true
	}
	for _, s := range loser.Sources {
		if !have[s] {
			of.Current[ki].Sources = append(of.Current[ki].Sources, s)
		}
	}
	of.Current[ki].Evidence = append(of.Current[ki].Evidence, loser.Evidence...)
	of.Current = append(of.Current[:mi:mi], of.Current[mi+1:]...)
	h := loser
	h.Of, h.At, h.Deleted = keepID, now, false
	of.History = append(of.History, h)
	if err := e.writeObservations(bankID, of, old); err != nil {
		return nil, err
	}
	return &MergeResult{Type: "observation", Kept: keepID, Merged: mergeID, Struck: ObservationsPath(bankID)}, nil
}

// mergedMarker starts the prose line a merged-away fact leaves behind. It is
// not a bullet, so a facts file keeps it verbatim and no index reads it as a
// fact, and it is struck through so a reader sees it was folded elsewhere.
func mergedLine(text, into string, at time.Time) string {
	return "~~" + normFactText(text) + "~~ (merged into " + into + " on " + at.Format("2006-01-02") + ")"
}

func (e *Engine) mergeFacts(bankID string, k, m *unit, now time.Time) (*MergeResult, error) {
	n, err := e.Vault.Read(m.Path)
	if err != nil {
		return nil, ErrNotFound
	}
	ff := ParseFacts(n.Body, bankID, m.Doc)
	var keep []Fact
	found := false
	for _, f := range ff.Facts {
		if f.ID == m.ID {
			found = true
			continue
		}
		keep = append(keep, f)
	}
	if !found {
		return nil, ErrNotFound
	}
	ff.Facts = keep
	ff.Prose = append(ff.Prose, mergedLine(m.Text, k.ID, now))
	if err := e.rewriteFacts(bankID, m.Doc, m.Path, n.Body, ff); err != nil {
		return nil, err
	}
	return &MergeResult{Type: "fact", Kept: k.ID, Merged: m.ID, Struck: m.Path}, nil
}
