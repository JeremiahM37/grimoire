package memory

import (
	"fmt"
	"os"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/trust"
)

// Basis is how a fact came to be on file: what the store knows, as opposed to
// what it was told, what it read, or what it guessed.
//
// It is DERIVED at read time from fields the bullet already carries. Nothing is
// stored for it, so there is no file format change and an old vault gets a
// basis the moment it is read. The table is in docs/ARCHITECTURE.md; Basis()
// below is the only implementation of it.
type Basis string

const (
	// BasisStated is asserted by a person (by=human, a bullet with no trailer,
	// or an id that no longer matches its text).
	BasisStated Basis = "stated"
	// BasisObserved is an agent's write that names what it read: an evidence
	// link, or a category that marks an observation.
	BasisObserved Basis = "observed"
	// BasisInferred is an agent's assertion with no evidence link, or one that
	// a consolidation, reflection or dream pass produced.
	BasisInferred Basis = "inferred"
	// BasisImported came from a memory import (origin import:*).
	BasisImported Basis = "imported"
	// BasisPulled came from text other people can write (connector, web, any
	// untrusted origin). It is the same claim trust.Untrusted makes.
	BasisPulled Basis = "pulled"
)

// Bases lists every basis, in the order documentation presents them.
var Bases = []Basis{BasisStated, BasisObserved, BasisInferred, BasisImported, BasisPulled}

// ImportOriginPrefix marks a fact written by a memory import (memport and the
// portability path both stamp it).
const ImportOriginPrefix = "import:"

// observedCategories are the categories that say an agent recorded something
// it observed (a tool result, a document, a transcript) rather than concluded.
// A category is the writer's own word, so this list is deliberately short.
var observedCategories = map[string]bool{
	"observation": true, "observed": true, "tool_result": true,
	"transcript": true, "document": true, "log": true,
}

// synthesisCategories and synthesisPrefixes name writers that compose a fact
// out of other facts rather than observing one. Their output is inferred even
// when evidence is attached, because the evidence is the inputs, not the claim.
var (
	synthesisCategories = map[string]bool{
		"inference": true, "synthesis": true, "reflection": true,
	}
	synthesisPrefixes = []string{"consolidat", "reflect", "dream"}
)

// Basis derives how this fact came to be on file. The order of the cases is
// the rule: the first that applies decides.
//
//  1. origin import:*                       -> imported
//  2. any untrusted origin                  -> pulled   (beats a human claim,
//     exactly as AuthorityOf does: text from outside cannot talk its way up)
//  3. human-authored (by=human, no trailer, id edited) -> stated
//  4. agent is a synthesis writer, or the category says synthesis -> inferred
//  5. evidence present, or an observation category -> observed
//  6. anything else (an agent's bare assertion) -> inferred
func (e Entry) Basis() Basis {
	if strings.HasPrefix(strings.TrimSpace(e.Origin), ImportOriginPrefix) {
		return BasisImported
	}
	if trust.FromOrigin(e.Origin) == trust.Untrusted {
		return BasisPulled
	}
	if e.HumanAuthored() {
		return BasisStated
	}
	if e.synthesised() {
		return BasisInferred
	}
	if e.Evidence != "" || observedCategories[strings.ToLower(strings.TrimSpace(e.Category))] {
		return BasisObserved
	}
	return BasisInferred
}

func (e Entry) synthesised() bool {
	if synthesisCategories[strings.ToLower(strings.TrimSpace(e.Category))] {
		return true
	}
	for _, who := range []string{e.Agent, e.Task} {
		who = strings.ToLower(strings.TrimSpace(who))
		for _, p := range synthesisPrefixes {
			if strings.HasPrefix(who, p) {
				return true
			}
		}
	}
	return false
}

// ParseBases reads a comma-separated basis filter. An unknown name is an error
// rather than being ignored: a filter that silently matches nothing, or
// everything, answers a question nobody asked.
func ParseBases(csv string) ([]Basis, error) {
	var out []Basis
	for _, part := range strings.Split(csv, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		found := false
		for _, b := range Bases {
			if string(b) == name {
				out = append(out, b)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("basis must be a comma list of stated, observed, inferred, imported, pulled")
		}
	}
	return out, nil
}

// EvidenceSep separates evidence items in the trailer. A comma is not allowed
// inside an item, which NormalizeEvidence enforces, so the split is exact.
const EvidenceSep = ","

// MaxEvidence bounds how many evidence links one fact may carry.
const MaxEvidence = 16

// SplitEvidence reads the trailer form of an evidence list.
func SplitEvidence(s string) []string {
	var out []string
	for _, item := range strings.Split(s, EvidenceSep) {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// NormalizeEvidence cleans a caller's evidence list: trimmed, empties dropped,
// bounded, and free of the characters the bullet grammar cannot carry.
func NormalizeEvidence(items []string) ([]string, error) {
	var out []string
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if len(item) > 512 {
			return nil, fmt.Errorf("each evidence item must be at most 512 characters")
		}
		if strings.ContainsAny(item, EvidenceSep+"\r\n") {
			return nil, fmt.Errorf("evidence items may not contain a comma or a line break")
		}
		out = append(out, item)
	}
	if len(out) > MaxEvidence {
		return nil, fmt.Errorf("at most %d evidence items", MaxEvidence)
	}
	return out, nil
}

// RecallMode narrows recall to what a person said about themselves, or to what
// is not that. The default is every fact, which is what recall has always done.
//
// The point is contamination, not a ban. Stored preferences and personas are
// right for a reply about the user and wrong to feed into a factual question,
// where they make the answer agree with the user rather than be correct.
type RecallMode string

const (
	RecallAll      RecallMode = "all"
	RecallFactual  RecallMode = "factual"
	RecallPersonal RecallMode = "personal"
)

// ParseRecallMode reads the mode parameter. Empty is RecallAll. An unknown
// value is an error, for the same reason as ParseBases.
func ParseRecallMode(s string) (RecallMode, error) {
	switch m := RecallMode(strings.ToLower(strings.TrimSpace(s))); m {
	case "", RecallAll:
		return RecallAll, nil
	case RecallFactual, RecallPersonal:
		return m, nil
	}
	return "", fmt.Errorf("mode must be factual, personal or all")
}

// PersonalCategoriesEnv overrides the personal category set. It is a
// comma-separated list; an empty or blank value means the default.
const PersonalCategoriesEnv = "GRIMOIRE_PERSONAL_CATEGORIES"

// DefaultPersonalCategories are the categories that hold what a person prefers
// or how they want to be addressed, rather than facts about the world.
var DefaultPersonalCategories = []string{"preference", "persona", "style", "likes"}

// PersonalCategories returns the personal category set in lower case. It is
// read on each call, so a changed environment takes effect without a restart
// of anything that holds no state of its own.
func PersonalCategories() []string {
	raw := strings.TrimSpace(os.Getenv(PersonalCategoriesEnv))
	if raw == "" {
		return append([]string(nil), DefaultPersonalCategories...)
	}
	var out []string
	for _, c := range strings.Split(raw, ",") {
		if c = strings.ToLower(strings.TrimSpace(c)); c != "" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return append([]string(nil), DefaultPersonalCategories...)
	}
	return out
}

// IsPersonal reports whether a category is in the personal set.
func IsPersonal(category string) bool {
	c := strings.ToLower(strings.TrimSpace(category))
	if c == "" {
		return false
	}
	for _, p := range PersonalCategories() {
		if p == c {
			return true
		}
	}
	return false
}
