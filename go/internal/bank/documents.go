package bank

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

// Document is a retained source, as stored in banks/<bank>/documents/<doc>.md:
// the content exactly as it was handed over, plus what retain needs to process
// it again — its timestamp, context, tags and the hash of every chunk, which
// is what lets a re-retain re-extract only the chunks that changed.
type Document struct {
	ID string `json:"document_id"`
	// Timestamp is when the content was written or said. Zero with Unset
	// false means "now at retain time" was recorded; Unset means the caller
	// said the content has no time at all.
	Timestamp   time.Time         `json:"timestamp"`
	Unset       bool              `json:"-"`
	Context     string            `json:"context,omitempty"`
	Tags        []string          `json:"tags"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	ChunkSize   int               `json:"chunk_size"`
	ChunkHashes []string          `json:"chunk_hashes,omitempty"`
	Content     string            `json:"content,omitempty"`
	Created     string            `json:"created,omitempty"`
	Updated     string            `json:"updated,omitempty"`
}

// Frontmatter renders the document's frontmatter.
func (d *Document) Frontmatter() *markdown.Frontmatter {
	fm := markdown.NewFrontmatter()
	fm.Set("title", "Document: "+oneLine(d.ID))
	fm.Set("document_id", d.ID)
	switch {
	case d.Unset:
		fm.Set("timestamp", "unset")
	case !d.Timestamp.IsZero():
		fm.Set("timestamp", d.Timestamp.UTC().Format(time.RFC3339))
	}
	if d.Context != "" {
		fm.Set("context", oneLine(d.Context))
	}
	fm.Set("doc_tags", listValue(d.Tags))
	if len(d.Metadata) > 0 {
		raw, _ := json.Marshal(d.Metadata)
		fm.Set("metadata", string(raw))
	}
	fm.Set("chunk_size", strconv.Itoa(d.ChunkSize))
	fm.Set("chunk_hashes", listValue(d.ChunkHashes))
	return fm
}

func listValue(xs []string) []markdown.Value {
	out := make([]markdown.Value, 0, len(xs))
	for _, x := range xs {
		// The frontmatter list syntax is comma-separated.
		if x = strings.TrimSpace(strings.ReplaceAll(x, ",", " ")); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// ParseDocument reads a document file.
func ParseDocument(fm *markdown.Frontmatter, body, fallbackID string) *Document {
	d := &Document{ID: strings.TrimSpace(fm.StringVal("document_id")), Tags: []string{}}
	if d.ID == "" {
		d.ID = fallbackID
	}
	switch ts := strings.TrimSpace(fm.StringVal("timestamp")); ts {
	case "unset":
		d.Unset = true
	default:
		d.Timestamp = parseTime(ts)
	}
	d.Context = fm.StringVal("context")
	if v, ok := fm.Get("doc_tags"); ok {
		d.Tags = valueList(v)
	}
	if raw := fm.StringVal("metadata"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &d.Metadata)
	}
	d.ChunkSize, _ = strconv.Atoi(fm.StringVal("chunk_size"))
	if v, ok := fm.Get("chunk_hashes"); ok {
		d.ChunkHashes = valueList(v)
	}
	d.Content = strings.TrimSpace(body)
	d.Created = fm.StringVal("created")
	d.Updated = fm.StringVal("updated")
	return d
}

// factsFrontmatter is the frontmatter of a facts file.
func factsFrontmatter(bankID, doc string) *markdown.Frontmatter {
	fm := markdown.NewFrontmatter()
	fm.Set("title", "Facts: "+oneLine(doc))
	fm.Set("bank", bankID)
	fm.Set("document_id", doc)
	return fm
}
