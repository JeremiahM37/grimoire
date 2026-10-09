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
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
	"github.com/JeremiahM37/grimoire/go/internal/dream/secscan"
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
	raw     string    // the bullet exactly as it is in the file, for a fix's Old
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
	return CheckWith(docs, now, memory.DefaultPriors())
}

// CheckWith is Check with the store's learned change rates, which the
// freshness checks use to decide whether a fact's declared tier still fits.
func CheckWith(docs []dream.Doc, now time.Time, priors memory.Priors) []dream.Finding {
	entries := collect(docs)
	chains := history(docs)

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
		out = append(out, backfill(a, chains[a.ID])...)
		out = append(out, freshness(a, now, priors)...)
		tier, _, _ := memory.ParseFresh(a.Fresh)
		lastKnown := a.written
		if t, err := time.ParseInLocation(memory.StampFormat, a.LastVerified(), time.Local); err == nil {
			lastKnown = t
		}
		// A fact declared stable says it does not go out of date, and a
		// volatile one is re-checked on every use already; neither needs this.
		if tier != memory.TierStable && tier != memory.TierVolatile &&
			!lastKnown.IsZero() && now.Sub(lastKnown) > staleAfter && volatileRE.MatchString(a.Text) {
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
			a := active{path: d.Path, Entry: e, norm: memory.Normalize(e.Text), raw: lineAt(d.Body, e.Line)}
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

func lineAt(body string, i int) string {
	lines := strings.Split(body, "\n")
	if i < 0 || i >= len(lines) {
		return ""
	}
	return lines[i]
}

// writeLikeRE matches commands that change state rather than read it. A check
// is run by an agent that has been told it is a lookup, so one of these in a
// check is either a mistake or an attempt to get an agent to do something.
var writeLikeRE = regexp.MustCompile(`(?i)\b(?:restart|stop|start|kill|pkill|reboot|shutdown|rm|rmdir|mv|delete|drop|truncate|push|apply|install|uninstall|upgrade|chmod|chown|dd|mkfs|systemctl\s+(?:enable|disable|mask))\b|(?:^|[^>])>\s*[^\s&]|-X\s*(?:POST|PUT|PATCH|DELETE)\b|\|\s*(?:sudo\s+)?(?:ba|z)?sh\b`)

// discardRE matches output thrown away rather than written: "2>/dev/null",
// ">/dev/null", "2>&1". Removed before writeLikeRE looks for redirects.
var discardRE = regexp.MustCompile(`[0-9]*>{1,2}\s*(?:/dev/null|&[0-9])`)

// freshness checks one fact's tier and check against its history.
//
//   - retier: the history says the declared tier is wrong — confirmed many
//     times and never changed, or changed repeatedly. The fix rewrites the
//     tier; the fact's text, id and counts are untouched, and history keeps the
//     old line. Facts a person wrote are reported, never rewritten.
//   - no_check: a fact marked for re-checking that does not say how, so every
//     re-check starts with an agent working out where the truth lives.
//   - check_command: a stored check that would be dangerous or that changes
//     state. Checks are commands agents run, so this is a security finding.
func freshness(a active, now time.Time, priors memory.Priors) []dream.Finding {
	var out []dream.Finding
	tier, ttl, _ := memory.ParseFresh(a.Fresh)
	if (tier == memory.TierVolatile || ttl > 0) && a.Check == "" {
		out = append(out, a.finding("no_check", dream.Low,
			"re-checked "+describeTier(a.Fresh)+" but records no way to check it; add check= so a re-check is one lookup"))
	}
	if a.Check != "" {
		if reason, bad := secscan.DangerousCommand(a.Check); bad {
			f := a.finding("check_command", dream.High, "its check "+reason+": "+a.Check)
			f.Category = dream.Security
			out = append(out, f)
		} else if writeLikeRE.MatchString(discardRE.ReplaceAllString(a.Check, "")) {
			f := a.finding("check_command", dream.Medium,
				"its check changes state rather than reading it, and agents run checks without asking: "+a.Check)
			f.Category = dream.Security
			out = append(out, f)
		}
		if a.Untrusted() {
			f := a.finding("check_command", dream.Medium,
				"carries a check but came from an untrusted origin ("+a.Origin+"); recall does not offer it")
			f.Category = dream.Security
			out = append(out, f)
		}
	}
	if next, why := a.SuggestTier(now, priors); next != "" {
		f := a.finding("retier", dream.Info,
			fmt.Sprintf("tier %s → %s: %s", tierName(a.Fresh), next, why))
		if !a.Human && !a.HandWritten && a.raw != "" {
			e := a.Entry
			e.Fresh = memory.NormFresh(next)
			f.Fix = &dream.Fix{Kind: dream.FixReplaceLine, Path: a.path, Line: a.Line + 1,
				Old: a.raw, New: e.Format()}
		}
		out = append(out, f)
	}
	return out
}

func tierName(s string) string {
	if s == "" {
		return "auto"
	}
	return s
}

func describeTier(s string) string {
	switch s {
	case "":
		return "auto"
	case memory.TierVolatile:
		return "on every use"
	case memory.TierStable:
		return "stable"
	}
	return "every " + s
}

// chain is what a fact's struck-through predecessors say about it.
type chain struct {
	changes  int    // how many versions came before this one
	earliest string // the first version's stamp
}

// history walks the supersession links in every memory note. A replaced fact
// names its replacement (sup=), so following the links backward from a live
// fact counts the times it changed — evidence that predates the fact itself
// carrying a count, and that would otherwise be ignored by its change rate.
func history(docs []dream.Doc) map[string]chain {
	prev := map[string][]memory.Entry{} // replacement id -> what it replaced
	stamp := map[string]string{}
	for _, d := range docs {
		if d.Kind != dream.KindMemoryNote {
			continue
		}
		for _, e := range memory.Parse(d.Body) {
			stamp[e.ID] = e.Stamp
			if e.Superseded() && !strings.HasPrefix(e.SupersededBy, "retracted:") {
				prev[e.SupersededBy] = append(prev[e.SupersededBy], e)
			}
		}
	}
	out := map[string]chain{}
	var walk func(id string, seen map[string]bool) chain
	walk = func(id string, seen map[string]bool) chain {
		var c chain
		for _, p := range prev[id] {
			if seen[p.ID] {
				continue // a cycle is a hand edit gone wrong; count each once
			}
			seen[p.ID] = true
			if memory.CountsAsChange(p.Stamp, stamp[id]) {
				c.changes++
			}
			if c.earliest == "" || (p.Stamp != "" && p.Stamp < c.earliest) {
				c.earliest = p.Stamp
			}
			sub := walk(p.ID, seen)
			c.changes += sub.changes
			if sub.earliest != "" && sub.earliest < c.earliest {
				c.earliest = sub.earliest
			}
		}
		return c
	}
	for id := range prev {
		out[id] = walk(id, map[string]bool{id: true})
	}
	return out
}

// backfill records a fact's change history in its trailer when the history
// says more than the trailer does: facts replaced before freshness existed
// carry no count, and their predecessors are still in the file to count.
func backfill(a active, c chain) []dream.Finding {
	if c.changes <= a.Changes || a.Human || a.HandWritten || a.raw == "" {
		return nil
	}
	e := a.Entry
	e.Changes = c.changes
	if e.Since == "" || (c.earliest != "" && c.earliest < e.Since) {
		e.Since = c.earliest
	}
	f := a.finding("history", dream.Info,
		fmt.Sprintf("replaced %d times since %s; recording it so its change rate uses it", c.changes, e.Since))
	f.Fix = &dream.Fix{Kind: dream.FixReplaceLine, Path: a.path, Line: a.Line + 1, Old: a.raw, New: e.Format()}
	return []dream.Finding{f}
}
