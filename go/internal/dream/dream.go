// Package dream is the periodic offline pass over everything an agent has
// remembered: the memory notes written through remember, the memory banks,
// and file-based agent memory (a directory of markdown notes with a MEMORY.md
// index, the shape Claude Code and Codex keep).
//
// Memory written one fact at a time decays in ways no single write can see:
// two agents record the same lesson twice, an index stops pointing at the
// files it lists, a fact passes its own expiry and keeps being recalled, a
// pasted token sits in a note that syncs to a phone, a sentence copied from a
// web page starts reading like an instruction. A dream is the pass that looks
// at the store as a whole and reports — and, for the mechanical cases only,
// repairs — what it finds.
//
// The checks here are pure functions over documents: they read text and
// return findings. Collecting the documents, writing the report and applying
// fixes belong to the caller, so every check is testable without a vault.
package dream

import "time"

// Kind says which memory system a document belongs to. Checks use it to pick
// the documents they understand.
type Kind string

const (
	// KindMemoryNote is a note under memory/ written by remember.
	KindMemoryNote Kind = "memory_note"
	// KindFileMemory is a markdown note in a file-memory directory: one
	// fact per file, YAML frontmatter, listed from a MEMORY.md index.
	KindFileMemory Kind = "file_memory"
	// KindBank is a fact or observation file inside a memory bank.
	KindBank Kind = "bank"
)

// Doc is one document the dream reads. Path is vault-relative and uses
// forward slashes.
type Doc struct {
	Path  string
	Body  string
	Kind  Kind
	Mtime time.Time
}

// Category groups findings for the report.
type Category string

const (
	Security Category = "security"
	Hygiene  Category = "hygiene"
)

// Severity orders findings. High means act now (a live credential, an
// injected instruction); info means worth knowing, nothing to do.
type Severity string

const (
	High   Severity = "high"
	Medium Severity = "medium"
	Low    Severity = "low"
	Info   Severity = "info"
)

// Finding is one thing a dream noticed.
type Finding struct {
	// Check is a stable machine name, e.g. "secret", "injection",
	// "index_missing". Tests and the report group by it.
	Check    string   `json:"check"`
	Category Category `json:"category"`
	Severity Severity `json:"severity"`
	Path     string   `json:"path"`
	// Line is 1-based; 0 means the whole document.
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
	// Excerpt is a short, SAFE quote of the offending text. A credential is
	// always masked here: the report is itself a note, and notes sync.
	Excerpt string `json:"excerpt,omitempty"`
	// Related names the other documents or entry ids involved, e.g. the
	// twin of a duplicate.
	Related []string `json:"related,omitempty"`
	// Fix is set only when the repair is mechanical and reversible. Nothing
	// that needs judgment carries one.
	Fix *Fix `json:"fix,omitempty"`
}

// Fix is a mechanical edit to one document.
//
//   - FixReplaceLine: replace line Line (1-based), whose current content must
//     equal Old exactly, with New. New == "" deletes the line.
//   - FixAppend: append New as a new last line.
//
// Old is checked at apply time, so a fix computed against a document that
// has since changed is skipped rather than applied to the wrong line.
type Fix struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	Old  string `json:"old,omitempty"`
	New  string `json:"new,omitempty"`
}

const (
	FixReplaceLine = "replace_line"
	FixAppend      = "append"
)

// Report is the result of one dream.
type Report struct {
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Docs     int       `json:"docs"`
	Findings []Finding `json:"findings"`
	// Applied lists the fixes that were made, in order.
	Applied []Fix `json:"applied,omitempty"`
	// Actions records anything else the dream did, e.g. queuing a bank
	// consolidation, in a sentence each.
	Actions []string `json:"actions,omitempty"`
}
