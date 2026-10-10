package grounded

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Line is one turn of a conversation or one paragraph of a note.
type Line struct {
	Speaker string `json:"speaker,omitempty"`
	Text    string `json:"text"`
	// Date overrides the document date for this line (per-turn timestamps).
	Date time.Time `json:"date,omitempty"`
}

// Doc is a dated record: a conversation session, a note, a daily log.
type Doc struct {
	ID string `json:"id"`
	// Title is the header shown to a reader ("Session 3 — 9 June 2023").
	Title string    `json:"title"`
	Date  time.Time `json:"date"`
	Lines []Line    `json:"lines"`
}

// PreparedDoc is a Doc plus what ingestion derived from it. The original
// lines are never changed; annotations sit beside them.
type PreparedDoc struct {
	Doc
	Annotations [][]Annotation `json:"annotations"` // one slice per line
}

// Prepared is an ingested corpus: documents with their time annotations and
// the entity/event timeline.
type Prepared struct {
	Version  int           `json:"version"`
	Docs     []PreparedDoc `json:"docs"`
	Timeline *Timeline     `json:"timeline,omitempty"`
}

const preparedVersion = 1

// Prepare runs the deterministic time annotation over every line. res may be
// nil. It does no model work unless res is set.
func Prepare(ctx context.Context, docs []Doc, res Resolver) *Prepared {
	p := &Prepared{Version: preparedVersion}
	for _, d := range docs {
		pd := PreparedDoc{Doc: d, Annotations: make([][]Annotation, len(d.Lines))}
		for i, l := range d.Lines {
			ref := l.Date
			if ref.IsZero() {
				ref = d.Date
			}
			pd.Annotations[i], _ = AnnotateWith(ctx, l.Text, ref, res)
		}
		p.Docs = append(p.Docs, pd)
	}
	return p
}

// RenderOpts choose how a document is shown to a reader.
type RenderOpts struct {
	Annotate bool
}

func renderLine(l Line, anns []Annotation, o RenderOpts) string {
	text := l.Text
	if o.Annotate {
		text = Render(text, anns)
	}
	if l.Speaker != "" {
		text = l.Speaker + ": " + text
	}
	return strings.TrimSpace(text)
}

// RenderDoc renders one document with its header.
func (d PreparedDoc) RenderDoc(o RenderOpts) string {
	var b strings.Builder
	b.WriteString("## " + d.Title + "\n")
	for i, l := range d.Lines {
		var anns []Annotation
		if i < len(d.Annotations) {
			anns = d.Annotations[i]
		}
		b.WriteString(renderLine(l, anns, o))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Render renders every document in order, separated by blank lines.
func (p *Prepared) Render(o RenderOpts) string {
	parts := make([]string, len(p.Docs))
	for i, d := range p.Docs {
		parts[i] = d.RenderDoc(o)
	}
	return strings.Join(parts, "\n\n")
}

// RawText is the document text exactly as ingested, for the extractor.
func (d Doc) RawText() string {
	var b strings.Builder
	for _, l := range d.Lines {
		b.WriteString(renderLine(l, nil, RenderOpts{}))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Save writes the prepared corpus as JSON.
func (p *Prepared) Save(path string) error {
	raw, err := json.MarshalIndent(p, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// LoadPrepared reads a corpus written by Save.
func LoadPrepared(path string) (*Prepared, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Prepared
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Version != preparedVersion {
		return nil, fmt.Errorf("prepared corpus version %d, want %d", p.Version, preparedVersion)
	}
	return &p, nil
}

// LoadDocs reads a JSON array of Doc.
func LoadDocs(path string) ([]Doc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var docs []Doc
	return docs, json.Unmarshal(raw, &docs)
}
