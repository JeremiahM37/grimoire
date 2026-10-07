package bank

import (
	"path"
	"sort"
	"strings"
)

// File-keyed memory: what a bank remembers about one file in a repository,
// for a coding agent about to read it. Facts and observations that mention the
// path, and session digests that name it. Matching is by the path as written
// in the text, so it needs no model and no index beyond the files.

// FileMemoryItem is one remembered thing about a file.
type FileMemoryItem struct {
	Kind  string `json:"kind"` // fact | observation | digest
	Ref   string `json:"ref,omitempty"`
	Text  string `json:"text"`
	Date  string `json:"date,omitempty"`
	Human bool   `json:"human,omitempty"`
}

func isPathChar(b byte) bool {
	return b == '/' || b == '.' || b == '_' || b == '-' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// mentionsPath reports whether text names the file: its full path, or its
// last two segments, on path boundaries. A lone filename is only matched when
// that is all the path there is.
func mentionsPath(text, rel string) bool {
	rel = strings.TrimPrefix(path.Clean(strings.ReplaceAll(rel, "\\", "/")), "./")
	if rel == "" || rel == "." {
		return false
	}
	needles := []string{rel}
	if segs := strings.Split(rel, "/"); len(segs) > 2 {
		needles = append(needles, strings.Join(segs[len(segs)-2:], "/"))
	}
	for _, n := range needles {
		for from := 0; ; {
			i := strings.Index(text[from:], n)
			if i < 0 {
				break
			}
			i += from
			end := i + len(n)
			before := i == 0 || !isPathChar(text[i-1]) || (text[i-1] == '/' && n != rel)
			after := end == len(text) || !isPathChar(text[end]) ||
				(text[end] == '.' && (end+1 == len(text) || !isPathChar(text[end+1])))
			if before && after {
				return true
			}
			from = i + 1
		}
	}
	return false
}

// FileMemory returns what the bank holds about a file, a person's entries
// first and then the newest, at most limit of them.
func (e *Engine) FileMemory(bankID, file string, limit int) ([]FileMemoryItem, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 12
	}
	all, err := e.entries(bankID)
	if err != nil {
		return nil, err
	}
	var out []FileMemoryItem
	for _, d := range all {
		if mentionsPath(d.text, file) {
			out = append(out, FileMemoryItem{Kind: d.entry.Type, Ref: d.entry.Ref, Text: d.text,
				Date: d.entry.Date, Human: d.entry.Human})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Human != out[j].Human {
			return out[i].Human
		}
		return out[i].Date > out[j].Date
	})
	ds, err := e.ListDigests(bankID, 0)
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		if !mentionsPath(d.Body, file) {
			continue
		}
		var lines []string
		for _, ln := range strings.Split(d.Body, "\n") {
			if mentionsPath(ln, file) {
				lines = append(lines, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "- ")))
			}
		}
		out = append(out, FileMemoryItem{Kind: "digest", Ref: d.SessionID, Date: isoDay(d.Ended),
			Text: "session " + clip(d.SessionID, 24) + ": " + clip(strings.Join(lines, "; "), 300)})
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
