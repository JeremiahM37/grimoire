// Package skillgate gives mined procedure skills a structured spec and an
// offline test gate.
//
// A spec is a trigger (when the skill applies), preconditions and
// postconditions (checkable predicates), the ordered steps (the note body) and
// provenance (the sessions it was mined from). It is stored in the note's
// frontmatter under metadata.skill_*, a block older readers ignore, so a note
// without it loads exactly as before.
//
// The gate runs a candidate against the recorded sessions before it can be
// accepted: support, trigger false-fire rate, and failure contamination. It is
// a statistical sanity check over recorded transcripts. It is not proof that
// the skill works, and it uses no model.
package skillgate

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/memstore"
	"github.com/JeremiahM37/grimoire/go/internal/skillmine"
)

// Spec is the structured form of a procedure skill.
type Spec struct {
	Trigger        string   // when to apply, as the text the trigger is matched with
	Preconditions  []string // predicates that must hold before the steps run
	Steps          []string // ordered steps, from the note body
	Postconditions []string // predicates that should hold once the steps have run
	Sessions       []string // provenance: ids of the sessions it was mined from
}

// Empty reports whether the spec carries nothing, so the note is an ordinary
// procedure with no structure.
func (s Spec) Empty() bool {
	return s.Trigger == "" && len(s.Preconditions) == 0 && len(s.Postconditions) == 0
}

// frontmatter keys, under the metadata block.
const (
	keyTrigger = "metadata.skill_trigger"
	keyPre     = "metadata.skill_pre"
	keyPost    = "metadata.skill_post"
	keySess    = "metadata.skill_sessions"
	keyGate    = "metadata.skill_gate"
	keyOver    = "metadata.skill_gate_override"
)

var stepRE = regexp.MustCompile(`(?m)^\s*\d+\.\s+(.+?)\s*$`)

// SpecOf reads the spec a note carries. ok is false when it has none.
func SpecOf(n memstore.Note) (Spec, bool) {
	s := Spec{
		Trigger:        strings.TrimSpace(n.Fields[keyTrigger]),
		Preconditions:  splitList(n.Fields[keyPre]),
		Postconditions: splitList(n.Fields[keyPost]),
		Sessions:       splitList(n.Fields[keySess]),
	}
	for _, m := range stepRE.FindAllStringSubmatch(n.Body, -1) {
		s.Steps = append(s.Steps, m[1])
	}
	return s, !s.Empty()
}

// Lines renders the spec's frontmatter lines (under metadata:), two spaces
// indented. A value that cannot be stored on one line, or that would be split
// by the list separator, is an error rather than a silently changed spec.
func Lines(s Spec, g *skillmine.Gate) ([]string, error) {
	var out []string
	add := func(key string, v string) error {
		if strings.ContainsAny(v, "\n\r") {
			return fmt.Errorf("%s: value spans lines", key)
		}
		out = append(out, "  "+strings.TrimPrefix(key, "metadata.")+": "+yamlQuote(v))
		return nil
	}
	if err := add(keyTrigger, s.Trigger); err != nil {
		return nil, err
	}
	for _, list := range []struct {
		key   string
		items []string
	}{{keyPre, s.Preconditions}, {keyPost, s.Postconditions}, {keySess, s.Sessions}} {
		for _, it := range list.items {
			if strings.Contains(it, sep) {
				return nil, fmt.Errorf("%s: %q contains %q", list.key, it, sep)
			}
		}
		if err := add(list.key, strings.Join(list.items, sep)); err != nil {
			return nil, err
		}
	}
	if g != nil {
		state := "fail"
		if g.Pass {
			state = "pass"
		}
		if g.Override != "" {
			state = "override"
		}
		out = append(out, "  skill_gate: "+state)
		if g.Override != "" {
			if strings.ContainsAny(g.Override, "\n\r") {
				return nil, fmt.Errorf("override reason spans lines")
			}
			out = append(out, "  skill_gate_override: "+yamlQuote(g.Override))
		}
	}
	return out, nil
}

// Stamp writes a spec (and the gate state) into the frontmatter of a draft
// rendered by skillmine.Draft, before its closing `---`.
func Stamp(draft string, s Spec, g *skillmine.Gate) (string, error) {
	lines, err := Lines(s, g)
	if err != nil {
		return "", err
	}
	const open = "---\n"
	if !strings.HasPrefix(draft, open) {
		return "", fmt.Errorf("draft has no frontmatter")
	}
	i := strings.Index(draft[len(open):], "\n---\n")
	if i < 0 {
		return "", fmt.Errorf("draft has no frontmatter")
	}
	at := len(open) + i + 1 // just after the newline that ends the last field
	return draft[:at] + strings.Join(lines, "\n") + "\n" + draft[at:], nil
}

// FromCandidate is the spec a mined candidate starts from: its trigger is the
// text the gate matched, the preconditions say which programs and files the
// steps need, the postconditions that each step ran.
func FromCandidate(c skillmine.Candidate) Spec {
	s := Spec{Steps: c.Examples}
	if c.Gate != nil {
		s.Trigger = c.Gate.Trigger
		s.Sessions = c.Gate.Sources
	}
	if s.Trigger == "" {
		s.Trigger = strings.Join(c.Units, " ")
	}
	seen := map[string]bool{}
	for _, u := range c.Units {
		head := strings.Fields(u)[0]
		p := "command:" + head
		if strings.Contains(head, ".") {
			p = "file:" + head // a script the steps run, relative to the working directory
		}
		if !seen[p] {
			seen[p] = true
			s.Preconditions = append(s.Preconditions, p)
		}
		s.Postconditions = append(s.Postconditions, "ran:"+u)
	}
	return s
}

const sep = ";"

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// yamlQuote quotes a value when a YAML reader would misread it; the same rule
// skillmine uses for its drafts.
func yamlQuote(s string) string {
	if s == "" || strings.ContainsAny(s, ":#{}[],&*!|>'\"%@`") || strings.HasPrefix(s, "-") || strings.TrimSpace(s) != s {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return s
}
