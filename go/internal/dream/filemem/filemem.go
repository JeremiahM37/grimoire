// Package filemem checks file-based agent memory: a directory of markdown
// notes, one fact per file, each with YAML frontmatter, listed from a
// MEMORY.md index. Claude Code and Codex keep this shape.
//
// The index is the part that fails silently. Claude Code loads only the first
// 200 lines or 25 KB of MEMORY.md, whichever comes first, so an entry past
// that cutoff is never seen by any agent, and nothing reports it. The checks
// here make those invisible entries, dead links and orphaned notes visible.
//
// Like the rest of the dream package these are pure functions over documents.
// Fixes are returned only where the repair is mechanical; anything that needs
// a judgment about which of two notes to keep is reported without one.
package filemem

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
)

const (
	indexName = "MEMORY.md"

	// indexMaxLines and indexMaxBytes mirror what Claude Code loads from
	// MEMORY.md. They are the reason truncation is a High finding: past them
	// the entry does not degrade, it disappears.
	indexMaxLines = 200
	indexMaxBytes = 25 * 1024

	// indexLineLongChars is where an index line starts to eat the budget
	// faster than it earns its place.
	indexLineLongChars = 200

	// shingleSize and nearDupJaccard define "the same note written twice".
	// Five-word shingles catch reworded copies without flagging notes that
	// merely share a common phrase.
	shingleSize     = 5
	nearDupJaccard  = 0.6
	minNearDupWords = 20

	maxRelated = 10
)

// linkRE matches a markdown link and captures its target. Targets with a
// space are rejected by the character class, which is what the index format
// produces; a title with spaces is still matched by the bracket part.
var linkRE = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)

// Roots returns the vault-relative directories that contain a file named
// exactly MEMORY.md, sorted. The top of the vault is "". Only the file's
// name matters here, so memory.md or MEMORY.md.bak do not make a root.
func Roots(paths []string) []string {
	seen := make(map[string]bool)
	for _, p := range paths {
		p = path.Clean(p)
		if path.Base(p) != indexName {
			continue
		}
		seen[dirOf(p)] = true
	}
	out := make([]string, 0, len(seen))
	for dir := range seen {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

// Check runs every file-memory check against the notes directly inside root.
// docs may contain documents from elsewhere in the vault; only the .md files
// whose directory is root are considered, so a caller can pass everything it
// collected without filtering first.
//
// Index checks run only when root has a MEMORY.md. Without one there is no
// index for a note to be missing from, so reporting every note as unlinked
// would be noise. Frontmatter and near-duplicate checks run regardless.
//
// Index findings carry Fix.Line numbers from the original document. Apply
// replace-line fixes from the bottom of the file upwards, or each deletion
// shifts the lines below it; Fix.Old makes a stale line skip instead of
// clobbering the wrong one.
func Check(root string, docs []dream.Doc) []dream.Finding {
	root = cleanRoot(root)

	var index *dream.Doc
	var notes []fileNote
	present := make(map[string]bool)
	for _, d := range docs {
		if dirOf(d.Path) != root || !strings.HasSuffix(d.Path, ".md") {
			continue
		}
		name := path.Base(d.Path)
		if name == indexName {
			dd := d
			index = &dd
			present[indexName] = true
			continue
		}
		present[name] = true
		notes = append(notes, parseNote(d))
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].doc.Path < notes[j].doc.Path })

	var out []dream.Finding
	if index != nil {
		out = append(out, checkIndex(*index, notes, present)...)
	}
	out = append(out, checkFrontmatter(notes)...)
	out = append(out, checkNearDuplicates(notes)...)
	return out
}

// Fragmented reports memory directories under projectsDir/*/memory that are
// real directories other than canonical and hold notes. Memory written into
// such a directory is invisible to a session that reads the canonical store,
// and nothing says so: the agent that wrote it sees its own file fine.
//
// Symlinked memory directories are ignored, since they usually point at the
// canonical store already. A MEMORY.md that is empty does not count, because
// an empty index is what a store with no notes looks like. A missing
// projectsDir returns nil.
func Fragmented(projectsDir, canonical string) []dream.Finding {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}
	canonAbs := absClean(canonical)

	var out []dream.Finding
	for _, e := range entries {
		memDir := filepath.Join(projectsDir, e.Name(), "memory")
		abs := absClean(memDir)
		if abs == canonAbs {
			continue
		}
		// Lstat, not Stat: a symlink to the canonical store must read as a
		// link and be skipped, not followed into a second copy of it.
		info, err := os.Lstat(memDir)
		if err != nil || !info.IsDir() {
			continue
		}
		files, err := os.ReadDir(memDir)
		if err != nil {
			continue
		}
		var names []string
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
				continue
			}
			if f.Name() == indexName && isEmptyFile(filepath.Join(memDir, f.Name())) {
				continue
			}
			names = append(names, f.Name())
		}
		if len(names) == 0 {
			continue
		}
		out = append(out, dream.Finding{
			Check:    "fragmented_store",
			Category: dream.Hygiene,
			Severity: dream.Medium,
			Path:     abs,
			Message: fmt.Sprintf("%d memory file(s) live in a store that is not the canonical one (%s); sessions reading the canonical store cannot see them",
				len(names), canonAbs),
			Related: names,
		})
	}
	return out
}

// fileNote is a parsed file-memory document.
type fileNote struct {
	doc         dream.Doc
	file        string // base name, e.g. "feedback_x.md"
	title       string // frontmatter name, else title, else the file name sans .md
	description string
	hasFM       bool
	fields      map[string]string // top-level keys, and "metadata.<key>" for nested ones
	body        string            // document text with the frontmatter removed
}

func parseNote(d dream.Doc) fileNote {
	n := fileNote{doc: d, file: path.Base(d.Path)}
	fields, rest, ok := parseFrontmatter(d.Body)
	n.hasFM = ok
	n.fields = fields
	n.body = rest
	n.description = fields["description"]
	n.title = fields["name"]
	if n.title == "" {
		n.title = fields["title"]
	}
	if n.title == "" {
		n.title = strings.TrimSuffix(n.file, ".md")
	}
	return n
}

// indexEntry is one line of MEMORY.md that links a note.
type indexEntry struct {
	line   int    // 1-based
	raw    string // exact line content, for FixReplaceLine.Old
	target string // linked file name
	end    int    // byte offset just past this line's newline
}

// checkIndex covers the four index checks plus long index lines. Lines are
// walked in order, so the findings and the duplicate "first occurrence" are
// both deterministic.
func checkIndex(index dream.Doc, notes []fileNote, present map[string]bool) []dream.Finding {
	idxPath := index.Path
	entries, lines := parseIndex(index.Body)

	var out []dream.Finding
	linked := make(map[string]bool)
	firstLine := make(map[string]int)
	for _, e := range entries {
		linked[e.target] = true
		if !present[e.target] {
			out = append(out, dream.Finding{
				Check:    "index_dangling",
				Category: dream.Hygiene,
				Severity: dream.Medium,
				Path:     idxPath,
				Line:     e.line,
				Message:  fmt.Sprintf("index links %s, which is not in this directory", e.target),
				Related:  []string{joinRoot(dirOf(idxPath), e.target)},
				Fix:      replaceLineFix(idxPath, e),
			})
		}
		if first, ok := firstLine[e.target]; ok {
			out = append(out, dream.Finding{
				Check:    "index_duplicate",
				Category: dream.Hygiene,
				Severity: dream.Low,
				Path:     idxPath,
				Line:     e.line,
				Message:  fmt.Sprintf("index links %s again; it is already linked on line %d", e.target, first),
				Related:  []string{joinRoot(dirOf(idxPath), e.target)},
				Fix:      replaceLineFix(idxPath, e),
			})
			continue
		}
		firstLine[e.target] = e.line
	}

	for _, n := range notes {
		if linked[n.file] {
			continue
		}
		out = append(out, dream.Finding{
			Check:    "index_missing",
			Category: dream.Hygiene,
			Severity: dream.Low,
			Path:     n.doc.Path,
			Message:  "note is not linked from MEMORY.md, so no agent will be pointed at it",
			Fix: &dream.Fix{
				Kind: dream.FixAppend,
				Path: idxPath,
				New:  indexLine(n.title, n.file, n.description),
			},
		})
	}

	// Truncation is a property of the file, not of one line, so it is one
	// finding listing the invisible targets. It needs a line to be a link:
	// an over-long MEMORY.md of prose hurts nothing an agent could act on.
	if over := lines > indexMaxLines || len(index.Body) > indexMaxBytes; over {
		var hidden []indexEntry
		for _, e := range entries {
			if e.line > indexMaxLines || e.end > indexMaxBytes {
				hidden = append(hidden, e)
			}
		}
		if len(hidden) > 0 {
			related := make([]string, 0, min(len(hidden), maxRelated))
			for _, e := range hidden[:min(len(hidden), maxRelated)] {
				related = append(related, e.target)
			}
			out = append(out, dream.Finding{
				Check:    "index_truncated",
				Category: dream.Hygiene,
				Severity: dream.High,
				Path:     idxPath,
				Line:     hidden[0].line,
				Message: fmt.Sprintf("%d of %d index entries are past the %d-line / %d KiB cutoff and are never loaded",
					len(hidden), len(entries), indexMaxLines, indexMaxBytes/1024),
				Related: related,
			})
		}
	}

	// Long lines are Info: they are not wrong, but each one spends the
	// budget that truncation is about to run out of. One finding for all of
	// them — ninety findings saying the same thing is how a report stops
	// being read.
	var long []int
	longest := 0
	for i, raw := range strings.Split(index.Body, "\n") {
		if n := utf8.RuneCountInString(strings.TrimSuffix(raw, "\r")); n > indexLineLongChars {
			long = append(long, i+1)
			if n > longest {
				longest = n
			}
		}
	}
	if len(long) > 0 {
		out = append(out, dream.Finding{
			Check:    "index_line_long",
			Category: dream.Hygiene,
			Severity: dream.Info,
			Path:     idxPath,
			Line:     long[0],
			Message: fmt.Sprintf("%d index line(s) are over %d characters (longest %d); long hooks spend the truncation budget faster",
				len(long), indexLineLongChars, longest),
		})
	}
	return out
}

// parseIndex returns the linking lines of MEMORY.md and the number of lines
// in the document. A trailing newline does not start an extra line, which is
// how the loader counts.
func parseIndex(body string) ([]indexEntry, int) {
	raws := strings.Split(body, "\n")
	lines := len(raws)
	if strings.HasSuffix(body, "\n") {
		lines--
	}
	var entries []indexEntry
	offset := 0
	for i, raw := range raws {
		end := offset + len(raw)
		if i < len(raws)-1 {
			end++ // the newline that split consumed
		}
		if target := linkTarget(raw); target != "" {
			entries = append(entries, indexEntry{line: i + 1, raw: raw, target: target, end: end})
		}
		offset = end
	}
	return entries, lines
}

// linkTarget returns the first markdown link on a line that points at a
// .md file in the same directory. External URLs and subdirectory paths are
// not this directory's files, so they never count as links here.
func linkTarget(raw string) string {
	for _, m := range linkRE.FindAllStringSubmatch(raw, -1) {
		t := m[1]
		if strings.Contains(t, "://") || strings.HasPrefix(t, "#") {
			continue
		}
		t = strings.TrimPrefix(t, "./")
		if !strings.HasSuffix(t, ".md") || strings.Contains(t, "/") {
			continue
		}
		return t
	}
	return ""
}

// checkFrontmatter requires the three fields an agent uses to decide whether
// a note is relevant: a name, a description to scan, and a type. Type may be
// top-level or under metadata, because both shapes exist in the wild.
func checkFrontmatter(notes []fileNote) []dream.Finding {
	var out []dream.Finding
	for _, n := range notes {
		if !n.hasFM {
			out = append(out, dream.Finding{
				Check:    "frontmatter",
				Category: dream.Hygiene,
				Severity: dream.Low,
				Path:     n.doc.Path,
				Message:  "note has no YAML frontmatter, so agents cannot see its name, description or type",
			})
			continue
		}
		var missing []string
		if n.fields["name"] == "" {
			missing = append(missing, "name")
		}
		if n.fields["description"] == "" {
			missing = append(missing, "description")
		}
		if n.fields["type"] == "" && n.fields["metadata.type"] == "" {
			missing = append(missing, "type")
		}
		if len(missing) > 0 {
			out = append(out, dream.Finding{
				Check:    "frontmatter",
				Category: dream.Hygiene,
				Severity: dream.Low,
				Path:     n.doc.Path,
				Message:  "frontmatter is missing: " + strings.Join(missing, ", "),
			})
		}
	}
	return out
}

// checkNearDuplicates reports each pair of notes once, against the earlier
// path. Pairs where either body is short are skipped entirely, including the
// identical-description test: a two-line note can share a description with
// another without being a duplicate, and short bodies make Jaccard noisy.
func checkNearDuplicates(notes []fileNote) []dream.Finding {
	type shingled struct {
		fileNote
		words    int
		grams    map[string]struct{}
		descNorm string
	}
	items := make([]shingled, 0, len(notes))
	for _, n := range notes {
		words := tokenize(n.body)
		s := shingled{fileNote: n, words: len(words), grams: shingles(words)}
		s.descNorm = strings.ToLower(strings.TrimSpace(n.description))
		items = append(items, s)
	}

	var out []dream.Finding
	for i := range items {
		for j := i + 1; j < len(items); j++ {
			a, b := items[i], items[j]
			if a.words < minNearDupWords || b.words < minNearDupWords {
				continue
			}
			if sim := jaccard(a.grams, b.grams); sim >= nearDupJaccard {
				out = append(out, dream.Finding{
					Check:    "near_duplicate",
					Category: dream.Hygiene,
					Severity: dream.Low,
					Path:     a.doc.Path,
					Message:  fmt.Sprintf("body is %.0f%% similar to %s (word %d-gram overlap)", sim*100, b.doc.Path, shingleSize),
					Related:  []string{b.doc.Path},
				})
				continue
			}
			if a.descNorm != "" && a.descNorm == b.descNorm {
				out = append(out, dream.Finding{
					Check:    "near_duplicate",
					Category: dream.Hygiene,
					Severity: dream.Low,
					Path:     a.doc.Path,
					Message:  "same description as " + b.doc.Path,
					Related:  []string{b.doc.Path},
				})
			}
		}
	}
	return out
}

// parseFrontmatter reads a leading --- block of simple YAML. It understands
// top-level scalars, folded or literal block scalars, and one level of
// nesting under metadata (stored as "metadata.<key>"). That covers the note
// shape in use; anything more elaborate is left unparsed, which at worst
// produces a frontmatter finding the author can read.
func parseFrontmatter(body string) (map[string]string, string, bool) {
	text := strings.TrimPrefix(body, "\uFEFF")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return map[string]string{}, text, false
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		// An unterminated block is not frontmatter; treating it as body keeps
		// the near-duplicate check honest about what the note actually says.
		return map[string]string{}, text, false
	}

	fields := make(map[string]string)
	var top string // current top-level key
	block := false // current top-level value is a block scalar
	for _, raw := range lines[1:end] {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		indented := line[0] == ' ' || line[0] == '\t'
		if indented {
			key, val, isKV := splitKV(strings.TrimSpace(line))
			switch {
			case top == "metadata" && isKV:
				fields["metadata."+key] = unquote(val)
			case block:
				fields[top] = joinText(fields[top], strings.TrimSpace(line))
			}
			continue
		}
		key, val, isKV := splitKV(line)
		if !isKV {
			continue
		}
		top = key
		block = val == "" || val == ">" || val == "|" || val == ">-" || val == "|-"
		if block {
			fields[key] = ""
		} else {
			fields[key] = unquote(val)
		}
	}
	rest := strings.Join(lines[end+1:], "\n")
	return fields, rest, true
}

func splitKV(s string) (key, val string, ok bool) {
	i := strings.Index(s, ":")
	if i <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(s[:i])
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", "", false
	}
	return key, strings.TrimSpace(s[i+1:]), true
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// joinText folds block-scalar lines into one value. Descriptions are read
// as prose, so a folded value and a single line compare the same way.
func joinText(a, b string) string {
	if a == "" {
		return b
	}
	return a + " " + b
}

func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func shingles(words []string) map[string]struct{} {
	set := make(map[string]struct{})
	for i := 0; i+shingleSize <= len(words); i++ {
		set[strings.Join(words[i:i+shingleSize], " ")] = struct{}{}
	}
	return set
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	small, big := a, b
	if len(b) < len(a) {
		small, big = b, a
	}
	inter := 0
	for g := range small {
		if _, ok := big[g]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	return float64(inter) / float64(union)
}

// indexLine is the entry index_missing appends. The shape matches the
// existing MEMORY.md index: "- [Title](file.md) — hook". With no description
// the separator is dropped rather than leaving a dangling dash.
func indexLine(title, file, description string) string {
	line := fmt.Sprintf("- [%s](%s)", title, file)
	if description != "" {
		line += " — " + description
	}
	return line
}

func replaceLineFix(idxPath string, e indexEntry) *dream.Fix {
	return &dream.Fix{
		Kind: dream.FixReplaceLine,
		Path: idxPath,
		Line: e.line,
		Old:  e.raw,
		New:  "",
	}
}

func isEmptyFile(p string) bool {
	b, err := os.ReadFile(p)
	return err == nil && strings.TrimSpace(string(b)) == ""
}

func absClean(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// dirOf returns the vault-relative directory of a slash path, with "" for
// the top of the vault (path.Dir reports ".").
func dirOf(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

func cleanRoot(root string) string {
	if root == "" || root == "." {
		return ""
	}
	return strings.Trim(path.Clean(root), "/")
}

func joinRoot(root, name string) string {
	if root == "" {
		return name
	}
	return root + "/" + name
}
