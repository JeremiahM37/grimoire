package vault

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

// The BYO-vault guarantee. Writing a note PATCHES the existing raw frontmatter
// block rather than regenerating it: managed flat keys are replaced, and
// anything the forgiving parser can't fully represent (nested maps, multiline
// block scalars, lists of objects — common in vaults another markdown app also
// manages) passes through byte for byte. Editing a note here must never degrade
// frontmatter written by another tool.

var (
	fmKeyRE        = regexp.MustCompile(`^([A-Za-z0-9_-]+):`)
	fmListDashRE   = regexp.MustCompile(`^-\s`)
	fmNestedListRE = regexp.MustCompile(`^\s+-\s`)
)

// FMEntry renders one flat frontmatter entry in canonical form.
func FMEntry(key string, value markdown.Value) string {
	switch v := value.(type) {
	case []markdown.Value:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = valueToString(item)
		}
		return fmt.Sprintf("%s: [%s]", key, strings.Join(parts, ", "))
	case []string:
		return fmt.Sprintf("%s: [%s]", key, strings.Join(v, ", "))
	case []any:
		// A list that arrived as JSON. Without this case it fell to the default
		// below and was rendered with Go's %v — "[a b]", space separated — which
		// ParseFrontmatter splits on COMMAS, so it read back as the single tag
		// "a b" and every lookup for "a" or "b" found nothing. A note written
		// through the API with two tags silently lost both, and the file was
		// not valid YAML either, so Obsidian could not read it.
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = valueToString(item)
		}
		return fmt.Sprintf("%s: [%s]", key, strings.Join(parts, ", "))
	case bool:
		if v {
			return key + ": true"
		}
		return key + ": false"
	default:
		return fmt.Sprintf("%s: %v", key, value)
	}
}

// PatchFrontmatter merges managed keys into an existing raw frontmatter block.
//
// Block model: a top-level `key:` line owns its continuation lines (indented
// lines and list dashes) until the next top-level key. Three cases:
//
//   - block contains structure we can't represent (an indented non-dash line:
//     nested map, |/> multiline, object list) → preserved VERBATIM, and the
//     caller's necessarily-degraded value for that key is ignored;
//   - flat block whose key is in fm → replaced with our canonical entry;
//   - flat block whose key is absent from fm → removed (deleted in the UI).
//
// Keys new to fm are appended at the end. Unowned stray lines pass through.
func PatchFrontmatter(rawInner string, fm *markdown.Frontmatter) string {
	type block struct {
		key    string
		hasKey bool
		lines  []string
	}
	var blocks []block

	for _, line := range strings.Split(rawInner, "\n") {
		if m := fmKeyRE.FindStringSubmatch(line); m != nil {
			blocks = append(blocks, block{key: m[1], hasKey: true, lines: []string{line}})
			continue
		}
		isContinuation := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") ||
			fmListDashRE.MatchString(line)
		if len(blocks) > 0 && isContinuation {
			blocks[len(blocks)-1].lines = append(blocks[len(blocks)-1].lines, line)
			continue
		}
		blocks = append(blocks, block{lines: []string{line}}) // stray line
	}

	isNested := func(lines []string) bool {
		for _, ln := range lines[1:] {
			indented := strings.HasPrefix(ln, " ") || strings.HasPrefix(ln, "\t")
			if strings.TrimSpace(ln) != "" && indented && !fmNestedListRE.MatchString(ln) {
				return true
			}
		}
		trimmed := strings.TrimRight(lines[0], " \t\r\n")
		return strings.HasSuffix(trimmed, "|") || strings.HasSuffix(trimmed, ">")
	}

	pending := fm.Clone()
	var out []string
	for _, b := range blocks {
		if !b.hasKey {
			if !(len(b.lines) == 1 && b.lines[0] == "") { // drop pure blanks, keep real strays
				out = append(out, b.lines...)
			}
			continue
		}
		if isNested(b.lines) {
			out = append(out, b.lines...) // foreign structure: verbatim, protected
			pending.Delete(b.key)
			continue
		}
		if v, ok := pending.Get(b.key); ok {
			out = append(out, FMEntry(b.key, v))
			pending.Delete(b.key)
		}
		// else: flat key deleted by the editor — omit
	}
	for _, k := range pending.Keys() { // newly added keys
		v, _ := pending.Get(k)
		out = append(out, FMEntry(k, v))
	}
	return "---\n" + strings.Join(out, "\n") + "\n---\n"
}

// Serialize renders frontmatter + body, guaranteeing a trailing newline so
// bodies compare equal modulo that newline everywhere else in the system.
func Serialize(fm *markdown.Frontmatter, body string) string {
	if fm == nil || fm.Len() == 0 {
		if strings.HasSuffix(body, "\n") {
			return body
		}
		return body + "\n"
	}
	lines := []string{"---"}
	for _, k := range fm.Keys() {
		v, _ := fm.Get(k)
		lines = append(lines, FMEntry(k, v))
	}
	lines = append(lines, "---")
	fmBlock := strings.Join(lines, "\n") + "\n"

	if !strings.HasPrefix(body, "\n") {
		body = "\n" + body
	}
	return fmBlock + strings.TrimRight(body, "\n") + "\n"
}
