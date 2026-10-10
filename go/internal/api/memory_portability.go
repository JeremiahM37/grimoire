package api

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memimage"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/memport"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Memory portability: the whole-memory export and import (docs/PORTABILITY.md).
//
// Neither direction is a new way in. The export reads through the same query
// as the recall list and the existing export, so the caller's spaces and reader
// lists apply exactly as they do everywhere else. The import writes through
// rememberOne, the path every agent write takes, and the only additions are the
// trust rules: an imported fact carries an import origin, so it is untrusted and
// not human, and a file cannot ask to be recorded as the operator.

const (
	// maxImportBytes bounds one import body. A memory store is text; 16 MiB is
	// far more facts than anyone has, and it keeps one request from buffering
	// an unbounded upload.
	maxImportBytes = 16 << 20
	// maxImportItems bounds the number of facts one import may write, for the
	// same reason: each becomes a note append and an index update.
	maxImportItems = 5000
	// importSampleSize is how many new facts a dry run shows.
	importSampleSize = 5
)

// restoredFact is what a grimoire export carries that a plain write cannot
// express. rememberOne consumes it through memoryIn.restore.
type restoredFact struct {
	Entry memory.Entry
	Path  string
}

// exportPortable writes the portable export in the requested format. The
// filters are the ones the existing export takes; the access rule is the same.
func (s *Server) exportPortable(w http.ResponseWriter, r *http.Request, format string) {
	if format != "jsonl" && format != "grimoire" && format != "markdown" && format != "md" {
		writeErr(w, http.StatusBadRequest, "format must be jsonl or markdown")
		return
	}
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter:            filterFor(r, true),
		Agent:             strings.TrimSpace(r.URL.Query().Get("agent")),
		Session:           strings.TrimSpace(r.URL.Query().Get("session")),
		Category:          strings.TrimSpace(r.URL.Query().Get("category")),
		IncludeSuperseded: true,
		IncludeExpired:    true,
		Now:               vault.Now(),
		Limit:             1 << 30,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	records := make([]memport.Record, 0, len(hits))
	sentImage := map[string]bool{}
	for _, h := range hits {
		rec := memport.Record{
			ID: h.ID, Text: h.Text, Path: h.Note, Agent: h.Agent, Task: h.Task,
			Session: h.Session, Category: h.Category, Stamp: h.Stamp,
			Expires: h.Expires, Immutable: h.Immutable, Authority: h.Authority().String(),
			Origin: h.Origin, SupersededBy: h.SupersededBy, Challenges: h.Challenges,
			Image: h.Image, CaptionBasis: h.CaptionBasis,
		}
		// The bytes travel once per picture, on the first fact that uses it.
		// Nothing is attached if the bytes are missing: the fact is exported
		// with its reference, and the importer will refuse it rather than
		// write a dangling picture.
		if h.Image != "" && !sentImage[h.Image] {
			if f, p, ok := memimage.Find(s.Vault.Root, h.Image); ok {
				if data, err := os.ReadFile(p); err == nil {
					rec.Attachment = &memport.Attachment{SHA: h.Image, MIME: f.MIME, Data: data}
					sentImage[h.Image] = true
				}
			}
		}
		records = append(records, rec)
	}
	now := vault.Now()
	if format == "markdown" || format == "md" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="grimoire-memory.md"`)
		_, _ = w.Write(memport.WriteMarkdown(records, now))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="grimoire-memory.jsonl"`)
	_, _ = w.Write(memport.WriteJSONL(records, now))
}

// importReport is the answer to one import. Written is zero on a dry run, and
// New is what a real run would write, so a dry run and a real run read alike.
type importReport struct {
	Format     string         `json:"format"`
	DryRun     bool           `json:"dry_run"`
	Total      int            `json:"total"`
	New        int            `json:"new"`
	Written    int            `json:"written"`
	Duplicates int            `json:"duplicates"`
	Failed     int            `json:"failed"`
	Skipped    []memport.Skip `json:"skipped"`
	Errors     []memport.Skip `json:"errors,omitempty"`
	Sample     []string       `json:"sample"`
}

// importMemory reads a memory file in any supported format and writes the facts
// it holds. POST /api/memory/import?from=auto|grimoire|mem0|letta|zep|jsonl-generic
// &dry_run=1, with the file as the raw body.
//
// Idempotency comes from content: a fact whose text is already on file, or that
// appears twice in the same file, is counted as a duplicate and not written, so
// running the same import again writes nothing.
func (s *Server) importMemory(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImportBytes))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "a memory import is limited to 16 MiB")
		return
	}
	dry := boolParam(r, "dry_run")
	res, err := memport.Parse(body, r.URL.Query().Get("from"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(res.Items) > maxImportItems {
		writeErr(w, http.StatusBadRequest, "at most 5000 facts per import")
		return
	}

	known, err := s.visibleMemoryTexts(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	rep := importReport{Format: res.Source, DryRun: dry,
		Total: len(res.Items) + len(res.Skips), Skipped: res.Skips}
	if rep.Skipped == nil {
		rep.Skipped = []memport.Skip{}
	}
	rep.Sample = []string{}
	seenImage := map[string]bool{}
	for _, it := range res.Items {
		text := strings.TrimSpace(it.Record.Text)
		img := it.Record.Image
		if img != "" {
			// Two pictures can share a caption, so an image fact is one fact
			// per picture, not per text.
			if seenImage[img] {
				rep.Duplicates++
				continue
			}
			seenImage[img] = true
			if err := s.restoreImage(it.Record, dry); err != nil {
				rep.Skipped = append(rep.Skipped, memport.Skip{Index: rep.New, Reason: err.Error()})
				continue
			}
		} else if known[text] {
			rep.Duplicates++
			continue
		}
		known[text] = true
		rep.New++
		if len(rep.Sample) < importSampleSize {
			rep.Sample = append(rep.Sample, text)
		}
		if dry {
			continue
		}
		in := importMemoryIn(it, res.Source)
		if img != "" {
			in.imageSHA, in.capBasis = img, it.Record.CaptionBasis
		}
		rec := &captureWriter{ResponseWriter: w}
		s.rememberOne(rec, r, in)
		if rec.status >= http.StatusBadRequest {
			rep.Failed++
			rep.Errors = append(rep.Errors, memport.Skip{Index: rep.New, Reason: strings.TrimSpace(rec.buf.String())})
			continue
		}
		rep.Written++
	}
	writeJSON(w, http.StatusOK, rep)
}

// restoreImage makes an imported picture available to the bullet that refers to
// it. The bytes must hash to the address the record claims: a file cannot
// attach a picture under someone else's name. With no bytes in the export, the
// picture must already be in this store, otherwise the fact is refused rather
// than written as a dangling reference.
func (s *Server) restoreImage(rec memport.Record, dry bool) error {
	if rec.Attachment == nil {
		if _, _, ok := memimage.Find(s.Vault.Root, rec.Image); ok {
			return nil
		}
		return fmt.Errorf("image %s is not in the export or the store", rec.Image)
	}
	p, err := memimage.Prepare(rec.Attachment.Data, imageMaxBytes())
	if err != nil {
		return fmt.Errorf("image %s: %v", rec.Image, err)
	}
	if p.SHA != rec.Image || rec.Attachment.SHA != rec.Image {
		return fmt.Errorf("image %s: bytes do not match their address", rec.Image)
	}
	if dry {
		return nil
	}
	return memimage.Store(s.Vault.Root, p)
}

// importMemoryIn turns one parsed record into the write the normal path takes.
// Human is never set: a file cannot assert that a person said something.
func importMemoryIn(it memport.Item, source string) memoryIn {
	rec := it.Record
	m := memoryIn{
		Text:      rec.Text,
		Agent:     rec.Agent,
		Task:      rec.Task,
		Session:   rec.Session,
		Category:  rec.Category,
		Expires:   rec.Expires,
		Immutable: rec.Immutable,
		Origin:    rec.Origin,
		Infer:     boolPtr(false),
	}
	if m.Agent == "" {
		m.Agent = "import:" + source
	}
	if m.Origin == "" && !it.Restored {
		m.Origin = "import:" + source
	}
	if it.Restored {
		stamp := rec.Stamp
		if stamp == "" {
			stamp = vault.Now().Format("2006-01-02 15:04")
		}
		m.restore = &restoredFact{
			Entry: memory.Entry{ID: rec.ID, Stamp: stamp,
				SupersededBy: rec.SupersededBy, Challenges: rec.Challenges},
			Path: rec.Path,
		}
	} else {
		// Foreign facts land in one note per source, so they are easy to find,
		// review and delete as a group.
		m.Topic = "imported " + source
	}
	return m
}

func boolPtr(b bool) *bool { return &b }

// visibleMemoryTexts is the set of fact texts the caller can already read,
// superseded and expired ones included: an import must not re-add a fact the
// store once held. A fact the caller cannot see is not counted as present, so
// an import never reveals what is on file, only what it wrote.
func (s *Server) visibleMemoryTexts(r *http.Request) (map[string]bool, error) {
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter:            filterFor(r, true),
		IncludeSuperseded: true,
		IncludeExpired:    true,
		Now:               vault.Now(),
		Limit:             1 << 30,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(hits))
	for _, h := range hits {
		out[strings.TrimSpace(h.Text)] = true
	}
	return out, nil
}
