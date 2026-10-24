package filemem

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
)

// Tiering keeps MEMORY.md under the load limit without dropping anything.
//
// Agents append one line per memory and nothing ever takes one out, so an
// index grows past the 200 lines / 25 KiB that Claude Code loads and the
// newest half of memory silently stops being seen. Reporting that every day
// fixed nothing. A tiered index keeps the lines agents most need — standing
// rules first, then the memories that are actually used — in MEMORY.md, and
// moves the rest, line for line, into MEMORY-<type>.md files that MEMORY.md
// points at. Every entry is still one read away and still searchable; only
// what is loaded unasked changes.

const (
	// partPrefix names the overflow files. They are part of the index, not
	// memories, so the note checks skip them.
	partPrefix = "MEMORY-"

	// A rewrite fills the core to the target, not to the limit, so the next
	// dozen appends do not push it straight back over and churn the files.
	tierTargetLines = 170
	tierTargetBytes = 21 * 1024

	// partMarker ends a pointer line so a later tiering recognises it as its
	// own and does not carry it as an entry.
	partMarker = "<!-- grimoire:index-part -->"
)

// IsIndexPart reports whether a file name is an overflow part of MEMORY.md.
func IsIndexPart(name string) bool {
	return strings.HasPrefix(name, partPrefix) && strings.HasSuffix(name, ".md")
}

// OverLimit reports whether an index body is past what Claude Code loads.
func OverLimit(body string) bool {
	_, lines := parseIndex(body)
	return lines > indexMaxLines || len(body) > indexMaxBytes
}

// ruleTypes are the memory types that say how to behave rather than what is
// true. They are kept in the core ahead of everything else.
var ruleTypes = map[string]bool{"feedback": true, "user": true}

type tierEntry struct {
	raw    string
	target string
	kind   string
	use    int
	mtime  time.Time
	order  int
}

// Tier plans the rewrite of an over-limit index. docs are the documents of
// one memory directory (as Check takes them); use counts how often each note
// was shown to an agent recently, by file name. It returns fixes that write
// MEMORY.md and its parts, or nil when the index is within the limit.
//
// Each fix carries the file's current text as Old, so a plan made before an
// agent's append is skipped at apply time rather than losing the append.
func Tier(root string, docs []dream.Doc, use map[string]int) []dream.Fix {
	root = cleanRoot(root)
	var index *dream.Doc
	parts := map[string]dream.Doc{}
	notes := map[string]fileNote{}
	for _, d := range docs {
		if dirOf(d.Path) != root || !strings.HasSuffix(d.Path, ".md") {
			continue
		}
		name := path.Base(d.Path)
		switch {
		case name == indexName:
			dd := d
			index = &dd
		case IsIndexPart(name):
			parts[name] = d
		default:
			notes[name] = parseNote(d)
		}
	}
	if index == nil || !OverLimit(index.Body) {
		return nil
	}

	// Gather every entry: the core's own first, then each part's, so a note
	// listed twice keeps its core line.
	var header []string
	var entries []tierEntry
	seen := map[string]bool{}
	add := func(raw string) {
		target := linkTarget(raw)
		if target == "" || IsIndexPart(target) || strings.Contains(raw, partMarker) || seen[target] {
			return
		}
		seen[target] = true
		e := tierEntry{raw: raw, target: target, kind: kindOf(target, notes[target]),
			use: use[target], order: len(entries)}
		if n, ok := notes[target]; ok {
			e.mtime = n.doc.Mtime
		}
		entries = append(entries, e)
	}
	inEntries := false
	for _, raw := range strings.Split(strings.TrimSuffix(index.Body, "\n"), "\n") {
		if linkTarget(raw) != "" || strings.Contains(raw, partMarker) {
			inEntries = true
			add(raw)
			continue
		}
		// Prose before the first entry is the index's own header and stays on
		// top. Blank lines and stray prose between entries are layout, which
		// the rewrite regenerates.
		if !inEntries && strings.TrimSpace(raw) != "" && raw != GeneratedMarker {
			header = append(header, raw)
		}
	}
	names := make([]string, 0, len(parts))
	for n := range parts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, raw := range strings.Split(parts[n].Body, "\n") {
			add(raw)
		}
	}

	// Rank: rules first, then by recent use, then most recently changed.
	ranked := make([]tierEntry, len(entries))
	copy(ranked, entries)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if ruleTypes[a.kind] != ruleTypes[b.kind] {
			return ruleTypes[a.kind]
		}
		if a.use != b.use {
			return a.use > b.use
		}
		if !a.mtime.Equal(b.mtime) {
			return a.mtime.After(b.mtime)
		}
		return a.order < b.order
	})

	// Pointer lines are reserved before entries are chosen, one per type that
	// could overflow, so the way to the rest is never what gets cut.
	kinds := map[string]bool{}
	for _, e := range entries {
		kinds[e.kind] = true
	}
	lines := len(header) + 2 + len(kinds) + 2
	bytes := 0
	for _, h := range header {
		bytes += len(h) + 1
	}
	bytes += len(kinds)*170 + 4
	keep := map[string]bool{}
	for _, e := range ranked {
		if lines+1 > tierTargetLines || bytes+len(e.raw)+1 > tierTargetBytes {
			continue // a shorter line further down may still fit
		}
		keep[e.target] = true
		lines++
		bytes += len(e.raw) + 1
	}

	overflow := map[string][]string{}
	var core []string
	core = append(core, header...)
	if len(header) > 0 {
		core = append(core, "")
	}
	for _, e := range entries { // original order: appends stay where they were
		if keep[e.target] {
			core = append(core, e.raw)
		} else {
			overflow[e.kind] = append(overflow[e.kind], e.raw)
		}
	}
	partFor := func(kind string) string { return partPrefix + kind + ".md" }
	var kindsOut []string
	for k := range overflow {
		kindsOut = append(kindsOut, k)
	}
	sort.Strings(kindsOut)
	if len(kindsOut) > 0 {
		core = append(core, "")
		for _, k := range kindsOut {
			core = append(core, fmt.Sprintf("- [More %s memories (%d)](%s) — not loaded at start; read it, or search_notes, when the topic is not above %s",
				k, len(overflow[k]), partFor(k), partMarker))
		}
	}

	var fixes []dream.Fix
	write := func(name, text string) {
		rel := joinRoot(root, name)
		old := ""
		if name == indexName {
			old = index.Body
		} else if d, ok := parts[name]; ok {
			old = d.Body
		}
		if old == text {
			return
		}
		fixes = append(fixes, dream.Fix{Kind: dream.FixWriteFile, Path: rel, Old: old, New: text})
	}
	write(indexName, strings.Join(core, "\n")+"\n")
	for _, k := range kindsOut {
		body := []string{
			fmt.Sprintf("# More %s memories", k),
			"",
			"Moved out of MEMORY.md by Grimoire's dream so the index stays under what agents load at start. Nothing here was changed or dropped; an entry moves back when it is used.",
			"",
		}
		body = append(body, overflow[k]...)
		write(partFor(k), strings.Join(body, "\n")+"\n")
	}
	// A part whose entries all moved back to the core is removed.
	for _, n := range names {
		k := strings.TrimSuffix(strings.TrimPrefix(n, partPrefix), ".md")
		if _, ok := overflow[k]; !ok {
			fixes = append(fixes, dream.Fix{Kind: dream.FixRemoveFile, Path: joinRoot(root, n), Old: parts[n].Body})
		}
	}
	return fixes
}

// kindOf is a note's memory type: its frontmatter type, else the file name's
// prefix (feedback_x.md), else "other".
func kindOf(file string, n fileNote) string {
	for _, k := range []string{"metadata.type", "type", "metadata.kind"} {
		if v := strings.ToLower(strings.TrimSpace(n.fields[k])); v != "" && !strings.ContainsAny(v, " /\\") {
			return v
		}
	}
	if i := strings.IndexAny(file, "_-"); i > 0 {
		p := strings.ToLower(file[:i])
		if p != "memory" && !strings.ContainsAny(p, " ./\\") {
			return p
		}
	}
	return "other"
}
