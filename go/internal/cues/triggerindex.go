package cues

import (
	"sort"
	"strings"
)

// TriggerIndex is Store.Triggered over a fixed list of cues, with the path
// tokens worked out once. Replaying a thousand situations against a store
// would otherwise re-tokenize every action cue a thousand times. It answers
// exactly what Store.Triggered answers for the same cues (tested).
type TriggerIndex struct {
	byTail map[string]map[string]bool // tail -> targets whose action cues name it
	df     map[string]int             // tail -> action cues whose text contains it
	tails  []string
}

// NewTriggerIndex indexes the action cues in all.
func NewTriggerIndex(all []Cue) *TriggerIndex {
	ti := &TriggerIndex{byTail: map[string]map[string]bool{}, df: map[string]int{}}
	var texts []string
	for _, c := range all {
		if c.Kind != Action {
			continue
		}
		low := strings.ToLower(c.Text)
		texts = append(texts, low)
		for _, t := range PathTokens(low) {
			tail := pathTail(t)
			if len(tail) < 6 {
				continue
			}
			if ti.byTail[tail] == nil {
				ti.byTail[tail] = map[string]bool{}
			}
			ti.byTail[tail][c.Target] = true
		}
	}
	for tail := range ti.byTail {
		ti.tails = append(ti.tails, tail)
		for _, low := range texts {
			if strings.Contains(low, tail) {
				ti.df[tail]++
			}
		}
	}
	sort.Strings(ti.tails)
	return ti
}

// Triggered returns the targets whose action cues name a specific path the
// action touches (see Store.Triggered).
func (ti *TriggerIndex) Triggered(action string) []string {
	if ti == nil || len(ti.tails) == 0 {
		return nil
	}
	action = strings.ToLower(action)
	seen := map[string]bool{}
	var out []string
	for _, tail := range ti.tails {
		if !strings.Contains(action, tail) || ti.df[tail] > MaxTriggerTargets {
			continue
		}
		for t := range ti.byTail[tail] {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	sort.Strings(out)
	return out
}

// All returns every cue (a copy), so a caller can build a TriggerIndex or
// carry cues to another target.
func (s *Store) All() []Cue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Cue(nil), s.all...)
}

// Vectors returns the cues with their vectors, aligned.
func (s *Store) Vectors() ([]Cue, [][]float32) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Cue(nil), s.all...), append([][]float32(nil), s.vecs...)
}
