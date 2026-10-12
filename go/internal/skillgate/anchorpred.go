package skillgate

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/anchor"
)

// An anchor predicate is `anchor:<encoded>`, where <encoded> is the one-anchor
// token anchor.Encode writes. Encode separates the type from its arguments with
// a literal ';', but the spec's lists are separated by ';' too, so the
// structural separators are written as "%3b" (lowercase; Encode only ever writes
// uppercase escapes, so the two cannot be confused). Decoding reverses that
// before anchor.Decode runs.
const anchorPrefix = "anchor:"

// slotRE is a slot in a postcondition's argument, such as `<version>`.
var slotRE = regexp.MustCompile(`<[a-z0-9_-]+>`)

// AnchorPredicate renders one anchor as a predicate.
func AnchorPredicate(a anchor.Anchor) string {
	return anchorPrefix + strings.ReplaceAll(anchor.Encode([]anchor.Anchor{a}), ";", "%3b")
}

// ParseAnchorPredicate reads the body of an anchor predicate (the text after
// "anchor:"). It is an error unless it holds exactly one anchor.
func ParseAnchorPredicate(arg string) (anchor.Anchor, error) {
	list, err := anchor.Decode(strings.ReplaceAll(arg, "%3b", ";"))
	if err != nil {
		return anchor.Anchor{}, err
	}
	if len(list) != 1 {
		return anchor.Anchor{}, fmt.Errorf("an anchor predicate holds one anchor, not %d", len(list))
	}
	return list[0], nil
}

// HasSlot reports whether any argument of the anchor holds a slot.
func HasSlot(a anchor.Anchor) bool {
	for _, v := range a.Args {
		if slotRE.MatchString(v) {
			return true
		}
	}
	return false
}

// NeedsValues lists the postconditions that are anchor predicates with a slot.
// They cannot be run until the slot is filled in, so `check` never runs them.
func NeedsValues(s Spec) []string {
	var out []string
	for _, p := range s.Postconditions {
		kind, arg, _ := strings.Cut(p, ":")
		if kind != "anchor" {
			continue
		}
		if a, err := ParseAnchorPredicate(arg); err == nil && HasSlot(a) {
			out = append(out, p)
		}
	}
	return out
}

// runAnchorPredicate evaluates one anchor predicate. A slot is never run; a
// skipped probe (not checked from here) does not hold, because nothing vouches
// for it. An anchor the context cannot run is unmet, like any unknown check.
func runAnchorPredicate(arg string, ctx Context) bool {
	a, err := ParseAnchorPredicate(arg)
	if err != nil || HasSlot(a) || ctx.Run == nil {
		return false
	}
	return ctx.Run(a).Status == anchor.Pass
}

// anchorRunner is the real runner: a validated anchor, checked once.
func anchorRunner(a anchor.Anchor) anchor.Result {
	return anchor.Run(context.Background(), a)
}

// ValidateShape checks an anchor the way the store will, with a slot allowed
// only where the caller says so. A slot is replaced by a placeholder that
// satisfies the probe's shape (a port, a status, an absolute path), so the
// check tests the shape and never a value that is not known yet.
func ValidateShape(a anchor.Anchor, allowSlots bool) error {
	if !HasSlot(a) {
		return anchor.Validate(a)
	}
	if !allowSlots {
		return fmt.Errorf("a slot (<name>) is only allowed in a postcondition")
	}
	var last error
	for _, ph := range []string{"placeholder", "1", "200"} {
		b := anchor.Anchor{Type: a.Type, Args: map[string]string{}}
		for k, v := range a.Args {
			b.Args[k] = slotRE.ReplaceAllString(v, ph)
		}
		if err := anchor.Validate(b); err != nil {
			last = err
			continue
		}
		return nil
	}
	return fmt.Errorf("a slot anchor must have a valid shape with the slot filled in: %v", last)
}
