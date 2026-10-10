package rulecheck

import (
	"regexp"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/adherence"
)

// Candidate is one proposed check for a rule, before it has been measured.
type Candidate struct {
	Spec
	Source string `json:"source"` // deterministic | llm
	Why    string `json:"why,omitempty"`
}

// MaxCandidates is the most checks kept per rule.
const MaxCandidates = 4

var (
	spanRE     = regexp.MustCompile("`([^`\n]{1,120})`")
	negRE      = regexp.MustCompile(`(?i)\b(?:never|don'?t|do not|must not|mustn'?t|should not|shouldn'?t|avoid|stop)\b`)
	alwaysRE   = regexp.MustCompile(`(?i)\b(?:always|must|make sure|ensure|need to|has to|have to)\b`)
	beforeRE   = regexp.MustCompile(`(?i)\bbefore\b`)
	withoutRE  = regexp.MustCompile(`(?i)\bwithout\b`)
	flagRE     = regexp.MustCompile(`^-{1,2}[A-Za-z][A-Za-z0-9\-]*(?:[= ]\S+)?$`)
	pathRE     = regexp.MustCompile(`^(?:~|\.{0,2})?/[\w.\-/~*]+$|^[\w.\-]+(?:/[\w.\-*]+)+/?$`)
	repoRE     = regexp.MustCompile(`(?i)\b(?:in|inside|on|for|within)\s+(?:the\s+)?([A-Za-z][A-Za-z0-9_.\-]{2,40})\s+(?:repo|repository|project|codebase)\b`)
	agentRE    = regexp.MustCompile(`(?i)\b(?:in|for|when using|under)\s+(claude code|claude-code|codex)\b`)
	editVerbRE = regexp.MustCompile(`(?i)\b(?:edit|write to|modify|touch|overwrite|change|delete)\b`)
	sentenceRE = regexp.MustCompile(`[.!?\n]+\s`)
	cmdSpanRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_./\-]*(?: [A-Za-z0-9_./\-=:@]+){0,4}$`)
)

type span struct {
	text       string
	start, end int
}

func spansOf(s string) []span {
	var out []span
	for _, m := range spanRE.FindAllStringSubmatchIndex(s, -1) {
		out = append(out, span{strings.TrimSpace(s[m[2]:m[3]]), m[0], m[1]})
	}
	return out
}

func wordsRE(text string) string {
	f := strings.Fields(text)
	for i := range f {
		f[i] = regexp.QuoteMeta(f[i])
	}
	re := `\b` + strings.Join(f, `\s+`)
	if last := f[len(f)-1]; last != "" && !strings.ContainsAny(last[len(last)-1:], "-./*") {
		re += `\b`
	}
	return re
}

func flagPattern(flag string) string {
	f := strings.Fields(flag)
	return `(?:^|\s)` + regexp.QuoteMeta(f[0]) + `(?:[=\s]|$)`
}

func pathPattern(p string) string {
	p = strings.TrimPrefix(strings.TrimPrefix(p, "~"), "./")
	return regexp.QuoteMeta(strings.TrimSuffix(p, "/")) + `(?:/|$)`
}

// scopeOf reads an explicit where-clause out of the rule text: a named repo or
// project, an agent. Paths used as scope are handled by the caller.
func scopeOf(text string) Scope {
	var sc Scope
	if m := repoRE.FindStringSubmatch(text); m != nil {
		sc.Cwd = `/` + regexp.QuoteMeta(m[1]) + `(?:/|$)`
	}
	if m := agentRE.FindStringSubmatch(text); m != nil {
		sc.Agent = strings.ReplaceAll(strings.ToLower(m[1]), " ", "-")
	}
	return sc
}

// Deterministic derives candidate checks from the rule's own words: backticked
// commands and flags, the command after a negation, "before" clauses and
// "without" clauses. It is the floor the model-written candidates are added to.
func Deterministic(text string) []Candidate {
	var out []Candidate
	seen := map[string]bool{}
	add := func(c Candidate) {
		if _, err := c.Spec.Compile(); err != nil {
			return
		}
		k := c.Spec.Describe() + "|" + strings.Join(c.Tools, ",")
		if !seen[k] && len(out) < MaxCandidates {
			seen[k] = true
			c.Source = "deterministic"
			out = append(out, c)
		}
	}
	rawScope := scopeOf(text)
	for _, sent := range sentenceRE.Split(text, -1) {
		sent = strings.TrimSpace(sent)
		if len(sent) < 8 {
			continue
		}
		sp := spansOf(sent)
		sc := rawScope
		// A path span next to a command is where the rule applies, not what it forbids.
		var cmds, flags, paths []span
		for _, s := range sp {
			switch {
			case flagRE.MatchString(s.text):
				flags = append(flags, s)
			case pathRE.MatchString(s.text) && !strings.Contains(s.text, " "):
				paths = append(paths, s)
			case cmdSpanRE.MatchString(s.text):
				cmds = append(cmds, s)
			}
		}
		neg := negRE.FindStringIndex(sent)
		bef := beforeRE.FindStringIndex(sent)

		// require-before: "run B before A" / "before A, run B".
		if bef != nil {
			var pre, post []span
			for _, s := range cmds {
				if s.start < bef[0] {
					pre = append(pre, s)
				} else {
					post = append(post, s)
				}
			}
			var a, b string
			switch {
			case len(pre) > 0 && len(post) > 0:
				b, a = pre[len(pre)-1].text, post[0].text
			case len(pre) == 0 && len(post) > 1:
				a, b = post[0].text, post[1].text
			}
			if a != "" && b != "" && a != b {
				add(Candidate{Spec: Spec{Shape: ShapeRequireBefore, Action: wordsRE(a), Before: wordsRE(b), Scope: sc},
					Why: "\"" + b + "\" before \"" + a + "\""})
				continue
			}
		}
		// require-with: "never A without --flag" / "always pass --flag to A".
		if len(flags) > 0 && (withoutRE.MatchString(sent) || alwaysRE.MatchString(sent)) && !(neg != nil && !withoutRE.MatchString(sent)) {
			action := ""
			if len(cmds) > 0 {
				action = wordsRE(cmds[0].text)
			} else {
				for _, w := range strings.Fields(strings.ToLower(sent)) {
					w = strings.Trim(w, ",.;:()")
					if shellWords[w] {
						action = `\b` + regexp.QuoteMeta(w) + `\b`
						break
					}
				}
			}
			if action != "" {
				add(Candidate{Spec: Spec{Shape: ShapeRequireWith, Action: action, With: flagPattern(flags[0].text), Scope: sc},
					Why: "calls matching the action must carry " + flags[0].text})
				continue
			}
		}
		// forbid: "never A".
		if neg != nil {
			// A path after "never edit/write/touch" is a file the agent must not change.
			if len(paths) > 0 && editVerbRE.MatchString(sent) && len(cmds) == 0 {
				add(Candidate{Spec: Spec{Shape: ShapeForbid, Tools: []string{"Edit", "Write", "MultiEdit"}, Action: pathPattern(paths[0].text), Scope: sc},
					Why: "writes to " + paths[0].text})
				continue
			}
			if len(paths) > 0 && sc.Cwd == "" && (len(cmds) > 0) {
				sc.Cwd = pathPattern(paths[0].text)
			}
			for _, pat := range adherence.Candidates(sent) {
				add(Candidate{Spec: Spec{Shape: ShapeForbid, Action: pat, Scope: sc}, Why: "command named after a negation or in code"})
			}
		}
	}
	return out
}

// shellWords are command words recognised without backticks.
var shellWords = map[string]bool{"git": true, "rm": true, "sudo": true, "curl": true, "docker": true, "kill": true,
	"pkill": true, "chmod": true, "chown": true, "ssh": true, "scp": true, "rsync": true, "systemctl": true,
	"pip": true, "npm": true, "cargo": true, "go": true, "make": true, "apt": true, "wget": true, "mv": true, "cp": true, "pytest": true}

// IsRuleText reports whether text reads as a standing instruction worth
// compiling: never/always/must/don't/avoid or a before clause.
func IsRuleText(text string) bool {
	return negRE.MatchString(text) || alwaysRE.MatchString(text) || beforeRE.MatchString(text)
}

// Prohibition reports whether the rule's own words are a hard never/always
// ("never", "always", "must not", "do not", "must"), the condition for enforce:
// ask to be considered automatically. "Avoid" and "prefer" are softer.
var hardRE = regexp.MustCompile(`(?i)\b(?:never|always|must not|mustn'?t|do not|don'?t|must)\b`)

func HardRule(text string) bool { return hardRE.MatchString(text) }
