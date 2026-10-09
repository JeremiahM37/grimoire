package skillmine

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Step is one simple command of a shell line.
type Step struct {
	Unit    string // e.g. "git push", "go test", "python3 build.py"
	Command string // the concrete command (redacted by the transcript reader)
}

// subcommandHeads are programs whose first non-flag argument names what they do.
var subcommandHeads = map[string]bool{
	"git": true, "docker": true, "docker-compose": true, "podman": true, "go": true, "npm": true, "pnpm": true, "yarn": true,
	"cargo": true, "pip": true, "pip3": true, "uv": true, "systemctl": true, "journalctl": false, "kubectl": true,
	"helm": true, "terraform": true, "gh": true, "grimoire": true, "pct": true, "qm": true, "pvesh": true, "apt": true,
	"apt-get": true, "brew": true, "tmux": true, "make": true, "restic": true, "npx": true, "pytest": false, "tailscale": true,
	"cmake": false, "rustup": true, "bun": true, "deno": true, "lectern": true,
}

// scriptHeads run a script; the script's file name is the unit.
var scriptHeads = map[string]bool{"python": true, "python3": true, "node": true, "bash": true, "sh": true, "zsh": true, "ruby": true, "perl": true}

// leadKeywords introduce a command rather than being one: `do gofmt -l x`.
var leadKeywords = map[string]bool{"do": true, "then": true, "else": true, "{": true, "!": true, "if": true, "while": true, "elif": true, "until": true}

// trivialHeads say nothing about a workflow.
var trivialHeads = map[string]bool{
	"cd": true, "ls": true, "pwd": true, "echo": true, "cat": true, "head": true, "tail": true, "wc": true, "grep": true,
	"rg": true, "find": true, "which": true, "true": true, "false": true, "clear": true, "export": true, "sleep": true,
	"tree": true, "stat": true, "file": true, "date": true, "whoami": true, "env": true, "printf": true, "test": true,
	"[": true, "read": true, "sed": true, "awk": true, "jq": true, "sort": true, "uniq": true, "xargs": true, "set": true,
	"source": true, ".": true, "type": true, "command": true, "less": true, "more": true, "tee": true, "cut": true,
	"tr": true, "diff": true, "du": true, "df": true, "ps": true, "wait": true, "exit": true, "alias": true, "unset": true,
	"mkdir": true, "touch": true, "rm": true, "cp": true, "mv": true, "ln": true, "chmod": true, "id": true, "uname": true,
	"hostname": true, "fd": true, "realpath": true, "basename": true, "dirname": true, "nl": true, "column": true,
	"bat": true, "ll": true, "la": true,
	// shell syntax, not programs
	"for": true, "do": true, "done": true, "if": true, "then": true, "fi": true, "else": true, "elif": true, "while": true,
	"until": true, "case": true, "esac": true, "in": true, "function": true, "return": true, "break": true, "continue": true,
	"{": true, "}": true, "(": true, ")": true, "!": true,
}

var (
	heredocRE = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)
	assignRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	wrapperRE = map[string]bool{"sudo": true, "time": true, "nohup": true, "env": true, "command": true, "exec": true, "nice": true, "timeout": false}
)

// Normalize splits a shell line into its simple commands and names each one.
// A pipeline is one step, named by its first program. Trivial programs (ls,
// cat, cd, grep ...) are dropped: a run of them is not a procedure.
func Normalize(line string) []Step {
	var out []Step
	for _, part := range splitShell(stripHeredocs(line)) {
		part = strings.TrimSpace(part)
		if part == "" || strings.HasPrefix(part, "#") {
			continue
		}
		toks := strings.Fields(part)
		for len(toks) > 0 && (assignRE.MatchString(toks[0]) || wrapperRE[toks[0]] || leadKeywords[toks[0]]) {
			toks = toks[1:]
		}
		if len(toks) == 0 {
			continue
		}
		head := filepath.Base(strings.Trim(toks[0], `"'`))
		if trivialHeads[head] || strings.ContainsAny(head, "$(`<>{}") {
			continue
		}
		unit := head
		switch {
		case subcommandHeads[head]:
			for _, t := range toks[1:] {
				if !strings.HasPrefix(t, "-") && !strings.ContainsAny(t, "$(`<>|=/") {
					unit = head + " " + t
					break
				}
			}
		case scriptHeads[head]:
			for _, t := range toks[1:] {
				if t == "-m" || t == "-c" || t == "-e" {
					break
				}
				if !strings.HasPrefix(t, "-") && !strings.ContainsAny(t, "$(`<>|") {
					unit = head + " " + filepath.Base(strings.Trim(t, `"'`))
					break
				}
			}
		}
		out = append(out, Step{Unit: unit, Command: part})
	}
	return out
}

// stripHeredocs removes the bodies of here-documents: `python3 - <<'PY' ... PY`
// is one command, and the script inside is not a sequence of commands.
func stripHeredocs(s string) string {
	for guard := 0; guard < 20; guard++ {
		loc := heredocRE.FindStringSubmatchIndex(s)
		if loc == nil {
			return s
		}
		delim := s[loc[2]:loc[3]]
		eol := strings.IndexByte(s[loc[1]:], '\n')
		if eol < 0 {
			return s
		}
		bodyStart := loc[1] + eol + 1
		end := len(s)
		for i := bodyStart; i <= len(s); {
			j := strings.IndexByte(s[i:], '\n')
			lineEnd := len(s)
			if j >= 0 {
				lineEnd = i + j
			}
			if strings.TrimSpace(s[i:lineEnd]) == delim {
				end = min(lineEnd+1, len(s))
				break
			}
			if j < 0 {
				break
			}
			i = lineEnd + 1
		}
		s = s[:bodyStart] + s[end:]
		// Mark this heredoc handled so the loop moves on.
		s = s[:loc[0]] + "<" + s[loc[0]+2:]
	}
	return s
}

// splitShell splits a command line into simple commands at &&, ||, ; and
// newlines that are outside quotes.
func splitShell(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, t)
		}
		cur.Reset()
	}
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case quote != 0:
			cur.WriteRune(c)
			if c == '\\' && quote == '"' && i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			} else if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
			cur.WriteRune(c)
		case c == '\\' && i+1 < len(rs):
			cur.WriteRune(c)
			i++
			cur.WriteRune(rs[i])
		case c == '\n' || c == ';':
			flush()
		case (c == '&' || c == '|') && i+1 < len(rs) && rs[i+1] == c:
			flush()
			i++
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return out
}
