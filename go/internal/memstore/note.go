package memstore

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IndexName is the file every agent loads first.
const IndexName = "MEMORY.md"

// Note is one file in a memory directory.
type Note struct {
	File        string
	Title       string
	Description string
	Body        string
	Kind        string
	Fields      map[string]string
	MTime       time.Time
	// Helpful and Unhelpful are the feedback counters kept in the note's
	// frontmatter metadata, when present.
	Helpful, Unhelpful int
}

// Score orders notes of the same kind: net feedback.
func (n Note) Score() int { return n.Helpful - n.Unhelpful }

// ParseNote parses one note's text.
func ParseNote(file, raw string, mtime time.Time) Note {
	fields, body, _ := ParseFrontmatter(raw)
	n := Note{File: file, Body: strings.TrimSpace(body), Fields: fields, MTime: mtime}
	n.Title = fields["name"]
	if n.Title == "" {
		n.Title = fields["title"]
	}
	if n.Title == "" {
		n.Title = strings.TrimSuffix(file, ".md")
	}
	n.Description = strings.TrimSpace(fields["description"])
	n.Kind = KindOfNote(fields, file, n.Body)
	n.Helpful, _ = strconv.Atoi(fields["metadata.helpful"])
	n.Unhelpful, _ = strconv.Atoi(fields["metadata.unhelpful"])
	return n
}

// LoadDir reads the notes directly inside dir (not MEMORY.md, not
// subdirectories), sorted by file name. A symlinked dir is followed.
func LoadDir(dir string) ([]Note, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Note
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || name == IndexName {
			continue
		}
		p := filepath.Join(dir, name)
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var mt time.Time
		if st, err := os.Stat(p); err == nil {
			mt = st.ModTime()
		}
		out = append(out, ParseNote(name, string(raw), mt))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

// Summary is the one line that stands for a note: its description, else the
// first sentence-like line of the body.
func (n Note) Summary(max int) string {
	s := n.Description
	if s == "" {
		for _, line := range strings.Split(n.Body, "\n") {
			line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#-*> "))
			if line != "" {
				s = line
				break
			}
		}
	}
	return clip(strings.Join(strings.Fields(strings.ReplaceAll(s, `\"`, `"`)), " "), max)
}

func clip(s string, max int) string {
	r := []rune(s)
	if max > 0 && len(r) > max {
		return strings.TrimSpace(string(r[:max-1])) + "…"
	}
	return s
}
