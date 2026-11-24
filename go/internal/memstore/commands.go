package memstore

import (
	"regexp"
	"strings"
)

// Commands in memory that is injected unasked.
//
// A memory written from a working session often carries the commands that
// worked: a fenced block, numbered steps, a one-liner that reads a key out of
// a secrets file. Injected verbatim they are long, they bury the sentence that
// says what the memory is for, and they hand the agent a ready-made way to
// pull a credential into its context. The memory's point is the prose; the
// commands stay in the note, where an agent that needs them reads them on
// purpose.

var (
	fence      = regexp.MustCompile("(?s)```.*?(?:```|$)")
	inlineCode = regexp.MustCompile("`[^`\n]+`")
	listMarker = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)
	envAssign  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*=\S`)
)

var commandWords = map[string]bool{
	"apt": true, "awk": true, "cat": true, "cd": true, "chmod": true, "chown": true,
	"codex": true, "cp": true, "curl": true, "docker": true, "env": true, "export": true,
	"gh": true, "git": true, "go": true, "grep": true, "grimoire": true, "journalctl": true,
	"kubectl": true, "lectern": true, "make": true, "mkdir": true, "mv": true, "npm": true,
	"pct": true, "pip": true, "pnpm": true, "python": true, "python3": true, "qm": true,
	"rm": true, "scp": true, "sed": true, "ssh": true, "sudo": true, "systemctl": true,
	"tailscale": true, "tar": true, "timeout": true, "wget": true,
}

// LooksLikeCommand reports whether a line is a shell command rather than
// prose that mentions one.
func LooksLikeCommand(s string) bool { return looksLikeCommand(s, false) }

// looksLikeCommand is LooksLikeCommand; span is set for an inline code span,
// where a known command word followed by an argument is already enough.
func looksLikeCommand(s string, span bool) bool {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$ "))
	if s == "" {
		return false
	}
	for _, signal := range []string{"$(", " && ", " | ", "2>&1", " > /", " >> "} {
		if strings.Contains(s, signal) {
			return true
		}
	}
	if strings.HasSuffix(s, " \\") || envAssign.MatchString(s) {
		return true
	}
	fields := strings.Fields(s)
	if len(fields) < 2 || !commandWords[fields[0]] {
		return false
	}
	if span {
		return true
	}
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") || strings.Contains(f, "/") || strings.Contains(f, "=") {
			return true
		}
	}
	return false
}

// CondenseCommands takes the commands out of a memory's text: fenced blocks,
// lines that are commands, and inline code spans that are. It reports how
// many it took out.
func CondenseCommands(text string) (string, int) {
	removed := 0
	text = fence.ReplaceAllStringFunc(text, func(string) string {
		removed++
		return ""
	})
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		// A list item that starts with a command word is a step as run, even
		// without a flag or a path ("2. python3 qeval.py run mmlu").
		item := listMarker.ReplaceAllString(line, "")
		if looksLikeCommand(item, item != line) {
			removed++
			continue
		}
		line = inlineCode.ReplaceAllStringFunc(line, func(span string) string {
			if looksLikeCommand(strings.Trim(span, "`"), true) {
				removed++
				return "`…`"
			}
			return span
		})
		kept = append(kept, commandClauses(line, &removed))
	}
	return strings.Join(kept, "\n"), removed
}

// clauseBreak splits prose into the clauses a command can hide in:
// "Source build: npm ci --prefix frontend; npm run build. Run: …".
var clauseBreak = regexp.MustCompile(`[:;.]\s+`)

// commandClauses replaces the clauses of a prose line that are commands.
func commandClauses(line string, removed *int) string {
	var b strings.Builder
	last := 0
	for _, at := range append(clauseBreak.FindAllStringIndex(line, -1), []int{len(line), len(line)}) {
		clause := line[last:at[0]]
		if looksLikeCommand(clause, false) {
			*removed++
			b.WriteString("…")
		} else {
			b.WriteString(clause)
		}
		b.WriteString(line[at[0]:at[1]])
		last = at[1]
	}
	return b.String()
}
