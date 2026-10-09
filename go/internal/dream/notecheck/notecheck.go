// Package notecheck holds the dream checks that read memory notes: a fact
// stored twice, a fact still recalled after its expiry, a contradiction nobody
// resolved, a fact the feedback has turned against, and a value that tends to
// change and has not been asserted for a while.
//
// Like the rest of the dream package these are pure functions over documents.
// None of them proposes a fix. Removing a duplicate has to go through
// supersession, not a line deletion, so the audit trail survives.
package notecheck

import (
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

const (
	// nearDuplicateThreshold is the token-set Jaccard overlap at or above
	// which two facts count as restating each other.
	nearDuplicateThreshold = 0.8
	// nearDuplicateMinTokens is how many distinct content words each fact
	// needs before an overlap means anything. Two short fragments can share
	// most of their words by accident.
	nearDuplicateMinTokens = 6
	// openChallengeAfter is how long a challenge can sit unresolved before the
	// finding says how long it has been open.
	openChallengeAfter = 14 * 24 * time.Hour
	// staleAfter is how old an assertion must be before a volatile value in
	// it is worth a second look.
	staleAfter = 90 * 24 * time.Hour
)

// volatileRE matches the kinds of value that go out of date: an IPv4 address,
// a :port, a semantic version, and the words that say a claim is a snapshot.
var volatileRE = regexp.MustCompile(`(?i)\b(?:\d{1,3}\.){3}\d{1,3}\b|:[0-9]{2,5}\b|\bv?\d+\.\d+\.\d+\b|\b(?:currently|now|latest|as of)\b`)

// active is one live entry together with where it lives and what it reads as.
type active struct {
	path string
	memory.Entry
	norm    string    // memory.Normalize of the text, for exact comparison
	written time.Time // zero when the stamp does not parse
}

// ref names an entry the way a finding's Related does: path#id.
func (a active) ref() string { return a.path + "#" + a.ID }

func (a active) finding(check string, sev dream.Severity, msg string, related ...string) dream.Finding {
	return dream.Finding{
		Check:    check,
		Category: dream.Hygiene,
		Severity: sev,
		Path:     a.path,
		Line:     a.Line + 1,
		Message:  msg,
		Related:  related,
	}
}

// Check runs the note checks over the memory notes in docs. Documents of any
// other kind are ignored, and superseded entries are never considered: a
// replaced fact is history, not something recall will return.
func Check(docs []dream.Doc, now time.Time) []dream.Finding {
	entries := collect(docs)

	var out []dream.Finding
	out = append(out, duplicates(entries)...)
	out = append(out, nearDuplicates(entries)...)
	for _, a := range entries {
		if a.ExpiredAt(now) {
			out = append(out, a.finding("expired", dream.Low,
				fmt.Sprintf("still recalled after its own expiry (%s)", a.Expires)))
		}
		if a.Challenges != "" {
			msg := fmt.Sprintf("contradicts %s, a higher-authority fact, and nobody resolved it", a.Challenges)
			if !a.written.IsZero() && now.Sub(a.written) > openChallengeAfter {
				msg += fmt.Sprintf("; open %d days", int(now.Sub(a.written)/(24*time.Hour)))
			}
			out = append(out, a.finding("open_challenge", dream.Medium, msg, a.Challenges))
		}
		if a.Unhelpful-a.Helpful >= 2 {
			out = append(out, a.finding("unhelpful", dream.Low,
				fmt.Sprintf("marked unhelpful %d times against %d helpful, and still recalled", a.Unhelpful, a.Helpful)))
		}
		if !a.written.IsZero() && now.Sub(a.written) > staleAfter && volatileRE.MatchString(a.Text) {
			out = append(out, a.finding("stale_volatile", dream.Info,
				fmt.Sprintf("a value that tends to change, last asserted %d days ago — verify before relying on it",
					int(now.Sub(a.written)/(24*time.Hour)))))
		}
	}

	rank := map[dream.Severity]int{dream.High: 0, dream.Medium: 1, dream.Low: 2, dream.Info: 3}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if rank[a.Severity] != rank[b.Severity] {
			return rank[a.Severity] < rank[b.Severity]
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Check < b.Check
	})
	return out
}

// collect parses the active entries out of the memory notes, oldest first.
// The order is what makes "the later one" of a pair well defined whatever
// order the documents arrived in.
func collect(docs []dream.Doc) []active {
	var out []active
	for _, d := range docs {
		if d.Kind != dream.KindMemoryNote {
			continue
		}
		for _, e := range memory.Parse(d.Body) {
			if e.Superseded() {
				continue
			}
			a := active{path: d.Path, Entry: e, norm: memory.Normalize(e.Text)}
			if t, err := time.ParseInLocation(memory.StampFormat, e.Stamp, time.Local); err == nil {
				a.written = t
			}
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Stamp != b.Stamp {
			return a.Stamp < b.Stamp
		}
		if a.path != b.path {
			return a.path < b.path
		}
		return a.Line < b.Line
	})
	return out
}

// duplicates reports every active entry whose normalized text equals an
// earlier active entry's. Related lists all the earlier copies.
func duplicates(entries []active) []dream.Finding {
	var out []dream.Finding
	seen := map[string][]active{}
	for _, a := range entries {
		if a.norm == "" {
			continue
		}
		if earlier := seen[a.norm]; len(earlier) > 0 {
			refs := make([]string, 0, len(earlier))
			for _, e := range earlier {
				refs = append(refs, e.ref())
			}
			out = append(out, a.finding("duplicate", dream.Low,
				"restates a fact already active on file", refs...))
		}
		seen[a.norm] = append(seen[a.norm], a)
	}
	return out
}

// nearDuplicates reports entries that overlap an earlier active entry at or
// above the Jaccard threshold, unless the two are exact duplicates, which
// duplicates already covers. Related lists every earlier entry it overlaps.
func nearDuplicates(entries []active) []dream.Finding {
	var out []dream.Finding
	for j, b := range entries {
		if b.norm == "" || distinctTokens(b.Text) < nearDuplicateMinTokens {
			continue
		}
		var refs []string
		for _, a := range entries[:j] {
			if a.norm == "" || a.norm == b.norm || distinctTokens(a.Text) < nearDuplicateMinTokens {
				continue
			}
			if memory.Similarity(a.Text, b.Text) >= nearDuplicateThreshold {
				refs = append(refs, a.ref())
			}
		}
		if len(refs) > 0 {
			out = append(out, b.finding("near_duplicate", dream.Info,
				"nearly restates an earlier active fact", refs...))
		}
	}
	return out
}

// distinctTokens counts the distinct content words in a fact.
func distinctTokens(s string) int {
	seen := map[string]bool{}
	for _, w := range memory.Tokens(s) {
		seen[w] = true
	}
	return len(seen)
}
