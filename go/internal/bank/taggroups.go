package bank

import (
	"fmt"
	"strings"
)

// TagGroup is one node of a boolean tag filter. A leaf names tags and how
// they must match; an inner node combines children with and, or or not.
// Exactly one of the four shapes is set:
//
//	{"tags": ["team:a", "q3"], "match": "all_strict"}
//	{"and": [ … ]}   {"or": [ … ]}   {"not": { … }}
//
// A leaf's match defaults to any_strict — a group exists to narrow, so an
// untagged fact does not slip through a leaf the way it does through the
// lenient top-level tags filter.
type TagGroup struct {
	Tags  []string   `json:"tags,omitempty"`
	Match string     `json:"match,omitempty"`
	And   []TagGroup `json:"and,omitempty"`
	Or    []TagGroup `json:"or,omitempty"`
	Not   *TagGroup  `json:"not,omitempty"`
	// Resolve is accepted for wire compatibility; only "exact" (the
	// default) is supported.
	Resolve string `json:"resolve,omitempty"`
}

const maxTagGroupDepth = 16

func (g *TagGroup) validate(depth int) error {
	if depth > maxTagGroupDepth {
		return fmt.Errorf("nested deeper than %d", maxTagGroupDepth)
	}
	shapes := 0
	if g.Tags != nil || g.Match != "" {
		shapes++
	}
	if g.And != nil {
		shapes++
	}
	if g.Or != nil {
		shapes++
	}
	if g.Not != nil {
		shapes++
	}
	if shapes != 1 {
		return fmt.Errorf("a group is exactly one of {tags, match}, {and}, {or} or {not}")
	}
	switch g.Resolve {
	case "", "exact":
	default:
		return fmt.Errorf("resolve %q is not supported (only exact)", g.Resolve)
	}
	switch {
	case g.Not != nil:
		return g.Not.validate(depth + 1)
	case g.And != nil || g.Or != nil:
		kids := g.And
		if g.Or != nil {
			kids = g.Or
		}
		if len(kids) == 0 {
			return fmt.Errorf("and/or needs at least one child")
		}
		for i := range kids {
			if err := kids[i].validate(depth + 1); err != nil {
				return err
			}
		}
		return nil
	}
	switch g.Match {
	case "", "any", "all", "any_strict", "all_strict", "exact":
	default:
		return fmt.Errorf("match must be any, all, any_strict, all_strict or exact")
	}
	for _, t := range g.Tags {
		if strings.TrimSpace(t) == "" {
			return fmt.Errorf("empty tag")
		}
	}
	return nil
}

// allows evaluates the group against one fact's tags.
func (g *TagGroup) allows(have []string) bool {
	switch {
	case g.Not != nil:
		return !g.Not.allows(have)
	case g.And != nil:
		for i := range g.And {
			if !g.And[i].allows(have) {
				return false
			}
		}
		return true
	case g.Or != nil:
		for i := range g.Or {
			if g.Or[i].allows(have) {
				return true
			}
		}
		return false
	}
	mode := g.Match
	if mode == "" {
		mode = "any_strict"
	}
	return tagsAllow(have, g.Tags, mode)
}

// groupsAllow is the AND of every top-level group.
func groupsAllow(groups []TagGroup, have []string) bool {
	for i := range groups {
		if !groups[i].allows(have) {
			return false
		}
	}
	return true
}

// sameTagSet is exact tag matching: the fact's tags are exactly the wanted
// set. An empty wanted set selects the untagged facts.
func sameTagSet(have, want []string) bool {
	h, w := map[string]bool{}, map[string]bool{}
	for _, t := range have {
		h[t] = true
	}
	for _, t := range want {
		w[t] = true
	}
	if len(h) != len(w) {
		return false
	}
	for t := range w {
		if !h[t] {
			return false
		}
	}
	return true
}
