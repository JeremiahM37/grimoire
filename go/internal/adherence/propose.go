package adherence

import (
	"regexp"
	"strings"
)

var (
	codeSpan = regexp.MustCompile("`([^`\n]{2,60})`")
	negation = regexp.MustCompile(`(?i)\b(?:never|don'?t|do not|must not|should not|shouldn'?t|avoid|stop)\s+(?:ever\s+)?([a-z][a-z\-]*(?:\s+[a-z0-9\-./]+){0,2})`)
	// shellVerbs are first words that name a command or a destructive verb a
	// tool call can contain. A negation like "never use mocks" has none, so
	// no pattern is proposed for it: a regex over a tool target cannot see it.
	shellVerbs = map[string]bool{
		"git": true, "rm": true, "sudo": true, "curl": true, "docker": true, "kill": true,
		"pkill": true, "chmod": true, "chown": true, "dd": true, "mkfs": true, "ssh": true,
		"scp": true, "rsync": true, "systemctl": true, "pip": true, "npm": true, "cargo": true,
		"push": true, "merge": true, "reset": true, "rebase": true, "drop": true,
		"delete": true, "restart": true, "reboot": true, "shutdown": true, "force": true,
		"truncate": true, "wget": true, "apt": true, "mv": true, "cp": true,
	}
	commandWord = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_./\-]*( [A-Za-z0-9_./\-=]+){0,3}$`)
)

// LooksLikeRule reports whether a memory reads as a standing instruction,
// for items stored without an explicit kind.
func LooksLikeRule(category, text string) bool {
	if strings.EqualFold(category, "rule") {
		return true
	}
	return negation.MatchString(text) ||
		regexp.MustCompile(`(?i)\b(always|must|never)\b`).MatchString(text)
}

// Candidates derives candidate forbid patterns from a rule's text: its code
// spans, and the command words after a negation. They are only candidates;
// the decision model is asked whether each one is a faithful detector, and a
// person accepts or rejects what survives.
func Candidates(text string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(words string) {
		words = strings.TrimSpace(words)
		fields := strings.Fields(words)
		if len(fields) == 0 || len(fields) > 4 {
			return
		}
		parts := make([]string, len(fields))
		for i, f := range fields {
			parts[i] = regexp.QuoteMeta(f)
		}
		re := `\b` + strings.Join(parts, `\s+`)
		if !strings.ContainsAny(fields[len(fields)-1][len(fields[len(fields)-1])-1:], "-./") {
			re += `\b`
		}
		if !seen[re] {
			seen[re] = true
			out = append(out, re)
		}
	}
	for _, m := range codeSpan.FindAllStringSubmatch(text, -1) {
		if commandWord.MatchString(strings.TrimSpace(m[1])) {
			add(m[1])
		}
	}
	for _, m := range negation.FindAllStringSubmatch(text, -1) {
		words := strings.Fields(strings.ToLower(m[1]))
		if len(words) > 0 && shellVerbs[words[0]] {
			add(strings.Join(words, " "))
		}
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}
