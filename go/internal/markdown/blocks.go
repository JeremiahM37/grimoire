package markdown

import (
	"regexp"
	"strings"
)

// Blocks — the parts of a note below the note.
//
// A note is the unit everything here indexes, and for most questions that is
// right. It is wrong for the questions people actually ask of a vault: "every
// open task", "every heading called Decisions", "the third bullet under
// Rollout". Those are about a LINE, and a note-shaped row cannot express one —
// which is why the task list used to be built by reading every note body on
// every request, and why headings were reachable only by scrolling.
//
// So headings, list items and tasks are parsed out here and indexed beside
// notes. They are derived, like everything else: drop the rows and a reindex
// rebuilds them from the markdown.

// Kinds of block.
const (
	KindHeading = "heading"
	KindItem    = "item"
	KindTask    = "task"
)

// Block is one addressable line of a note.
type Block struct {
	Kind string
	Text string
	// Level is the heading depth (1–6), or the list item's indent depth
	// starting at 1. It is what makes "top-level bullets only" expressible.
	Level int
	// Line is 0-based, so a caller can jump to it.
	Line int
	// Checked is meaningful for a task.
	Checked bool
	// Parent is the text of the nearest heading above, so a block can say
	// which section it belongs to without the caller re-reading the note.
	Parent string
}

var (
	blockHeadingRE = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)
	// A task is a list item whose content starts with a checkbox, so the item
	// pattern has to come first and the checkbox is peeled off after.
	blockItemRE  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	blockTaskRE  = regexp.MustCompile(`^\[([ xX])\]\s*(.*)$`)
	blockFenceRE = regexp.MustCompile("^\\s*(```|~~~)")
)

// ParseBlocks reads a note body into its addressable lines.
//
// Code fences are skipped: a shell script full of `# comment` lines would
// otherwise fill the index with headings nobody wrote, and a YAML block would
// contribute a list item per key.
func ParseBlocks(body string) []Block {
	var out []Block
	var heading string
	inFence := false

	for i, raw := range strings.Split(body, "\n") {
		if blockFenceRE.MatchString(raw) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		line := strings.TrimRight(raw, " \t")
		if m := blockHeadingRE.FindStringSubmatch(line); m != nil {
			heading = strings.TrimSpace(m[2])
			out = append(out, Block{
				Kind: KindHeading, Text: heading, Level: len(m[1]), Line: i})
			continue
		}
		m := blockItemRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// Indent depth, counting a tab as one level and every two spaces as
		// one. Editors disagree about which they emit and a note often has
		// both; treating them the same keeps "top-level" meaning what it looks
		// like on screen.
		indent := strings.ReplaceAll(m[1], "\t", "  ")
		level := len(indent)/2 + 1
		content := strings.TrimSpace(m[2])
		if t := blockTaskRE.FindStringSubmatch(content); t != nil {
			out = append(out, Block{
				Kind: KindTask, Text: strings.TrimSpace(t[2]), Level: level,
				Line: i, Checked: t[1] != " ", Parent: heading})
			continue
		}
		if content == "" {
			continue // an empty bullet is punctuation, not a block
		}
		out = append(out, Block{
			Kind: KindItem, Text: content, Level: level, Line: i, Parent: heading})
	}
	return out
}
