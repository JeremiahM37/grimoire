package anchor

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The dead-reference scan: concrete references in prose that may point at
// something that no longer exists. Deterministic, with no model involved.
var (
	refPathRE = regexp.MustCompile(`(?:~|/home/[\w.\-]+|/mnt/[\w.\-]+)/[\w./@+\-]*[\w/]`)
	refUnitRE = regexp.MustCompile(`\b([\w@\-]+\.(?:service|timer))\b`)
)

// globChars are the characters that make a path a pattern or a template.
const globChars = "*?[]{}<>$"

// bareRoots are matched by the path pattern but name a whole tree, not a
// reference to something specific, so they are not checked.
var bareRoots = map[string]bool{"~": true, "/opt": true, "/etc": true, "/mnt": true, "/home": true}

// ExtractRefs returns the reference-only anchors in a note's text: absolute
// local paths under ~, /home/<user> and /mnt/<volume> (RefPath), and systemd unit
// names ending .service or .timer (RefUnit). Paths with glob or template
// characters are skipped, and each reference appears once.
func ExtractRefs(text string) []Anchor {
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	var out []Anchor
	add := func(a Anchor) {
		key := a.Type + "\x00" + a.Args[a.argName()]
		if !seen[key] {
			seen[key] = true
			out = append(out, a)
		}
	}
	for _, loc := range refPathRE.FindAllStringIndex(text, -1) {
		// The pattern has no lookbehind, so the character before a match is
		// checked by hand: "x/home/admin/y" is not a reference to /home/admin/y.
		if loc[0] > 0 && isPathByte(text[loc[0]-1]) {
			continue
		}
		raw := text[loc[0]:loc[1]]
		// A glob or template that follows the path ("/mnt/bulk/c/*.json") makes
		// the path a pattern, and the pattern stops at the first wildcard.
		if loc[1] < len(text) && strings.IndexByte(globChars, text[loc[1]]) >= 0 {
			continue
		}
		if strings.ContainsAny(raw, globChars) || bareRoots[strings.TrimRight(raw, "/")] {
			continue
		}
		p := raw
		if strings.HasPrefix(p, "~") {
			if home == "" {
				continue
			}
			p = filepath.Join(home, p[1:])
		}
		if !filepath.IsAbs(p) {
			continue
		}
		add(Anchor{Type: RefPath, Args: map[string]string{"path": p}})
	}
	for _, loc := range refUnitRE.FindAllStringSubmatchIndex(text, -1) {
		u := text[loc[2]:loc[3]]
		if strings.Contains(u, "@") {
			continue // template and instance units cannot be checked by name
		}
		// "-flag.service" and "x.grimoire.service" are tokens, not unit names.
		if loc[2] > 0 && strings.IndexByte("@.-", text[loc[2]-1]) >= 0 {
			continue
		}
		add(Anchor{Type: RefUnit, Args: map[string]string{"unit": u}})
	}
	return out
}

// argName is the one argument a reference anchor is identified by.
func (a Anchor) argName() string {
	if a.Type == RefUnit {
		return "unit"
	}
	return "path"
}

// isPathByte is the character class a path may not directly follow.
func isPathByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		return true
	}
	return c == '.' || c == '~' || c == '/'
}
