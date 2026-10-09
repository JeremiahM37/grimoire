// Package memstore is the shared, agent-neutral memory store: the kinds a
// memory can have, the canonical directory every agent's own memory location
// can be linked to, the small always-loaded core plus pointers that replace a
// growing MEMORY.md, the light verification of procedures, and the lint that
// keeps what agents write worth reading.
//
// Everything here is a function over files or text. Nothing deletes a note;
// generated output (the index, a managed block) is replaceable and the notes
// stay the truth.
package memstore

import (
	"regexp"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/dream/filemem"
)

// The five kinds of memory.
const (
	KindRule       = "rule"       // a standing instruction: always applies
	KindProcedure  = "procedure"  // how to do X; steps, verified occasionally
	KindPreference = "preference" // how the user likes things
	KindFact       = "fact"       // state of the world; freshness applies
	KindReference  = "reference"  // pointer to something outside the store
)

// Kinds lists them in the order a core renders them.
var Kinds = []string{KindRule, KindProcedure, KindPreference, KindFact, KindReference}

// Valid reports whether k is one of the five kinds.
func Valid(k string) bool {
	for _, v := range Kinds {
		if k == v {
			return true
		}
	}
	return false
}

// Normalize maps a free-form label ("feedback", "how-to", "pref") to a kind,
// or "" when the label says nothing about kind.
func Normalize(label string) string {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "rule", "rules", "feedback", "instruction", "convention", "guideline", "policy":
		return KindRule
	case "procedure", "howto", "how-to", "how_to", "runbook", "workflow", "steps":
		return KindProcedure
	case "preference", "pref", "preferences", "user", "style":
		return KindPreference
	case "fact", "facts", "project", "state", "gotcha", "decision":
		return KindFact
	case "reference", "ref", "pointer", "link", "resource":
		return KindReference
	}
	return ""
}

var (
	stepLineRE = regexp.MustCompile(`(?m)^\s*(?:\d+[.)]|step\s+\d+[:.)]?)\s+\S`)
	howToRE    = regexp.MustCompile(`(?i)^\s*(?:how to\b|steps? to\b|runbook\b|procedure\b)`)
	imperative = regexp.MustCompile(`(?i)^\s*(?:never|always|do not|don't|dont|must|must not|should never)\b`)
	prefersRE  = regexp.MustCompile(`(?i)\b(?:prefers?|likes? to|wants? (?:me|agents?) to)\b`)
)

// StepShaped reports whether text reads as a procedure: three numbered steps,
// or an opening that says it is a how-to.
func StepShaped(text string) bool {
	return len(stepLineRE.FindAllString(text, -1)) >= 3 || howToRE.MatchString(text)
}

// InferText infers the kind of a one-line fact from its wording alone.
func InferText(text string) string {
	switch {
	case StepShaped(text):
		return KindProcedure
	case imperative.MatchString(text):
		return KindRule
	case prefersRE.MatchString(text):
		return KindPreference
	}
	return KindFact
}

// KindOfFact gives the kind of a stored fact: its category when that names a
// kind, else inferred from the text.
func KindOfFact(category, text string) string {
	if k := Normalize(category); k != "" {
		return k
	}
	return InferText(text)
}

// KindOfNote gives the kind of a file-memory note from its frontmatter, in
// order: metadata.kind, a top-level kind, the Claude type (feedback -> rule,
// user -> preference, reference -> reference, project -> fact), then the
// file-name prefix. A fact-ish or unknown note with step-shaped text is a
// procedure.
func KindOfNote(fields map[string]string, file, body string) string {
	for _, key := range []string{"metadata.kind", "kind"} {
		if k := Normalize(fields[key]); k != "" {
			return k
		}
	}
	kind := ""
	for _, key := range []string{"metadata.type", "type"} {
		if t := strings.ToLower(strings.TrimSpace(fields[key])); t != "" {
			kind = Normalize(t)
			break
		}
	}
	if kind == "" {
		if i := strings.Index(file, "_"); i > 0 {
			kind = Normalize(file[:i])
		}
	}
	if kind == "" || kind == KindFact {
		if StepShaped(body) || howToRE.MatchString(fields["description"]) {
			return KindProcedure
		}
	}
	if kind == "" {
		return KindFact
	}
	return kind
}

// ParseFrontmatter re-exports the file-memory frontmatter reader.
func ParseFrontmatter(raw string) (map[string]string, string, bool) {
	return filemem.ParseFrontmatter(raw)
}
