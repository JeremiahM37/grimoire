package connectors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/JeremiahM37/grimoire/go/internal/metrics"
	"github.com/JeremiahM37/grimoire/go/internal/trust"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Running a connector: fetch what changed, write it as notes, remember where
// we got to.
//
// The properties that matter, in the order they bite:
//
//   - A re-sync must UPDATE a document, not duplicate it. Every source item has
//     a stable external id, and the path it was written to is remembered, so the
//     second sync of an edited Jira ticket rewrites one note instead of adding
//     "ENG-1 Fix login 2.md" beside it.
//   - A sync that fails halfway must not lose its place, and must not silently
//     skip what it did not reach. The cursor advances only over documents that
//     were actually written.
//   - A document that has not changed must not be rewritten, because rewriting
//     it means re-embedding it — the expensive part — and bumping its modified
//     time for no reason.
//   - Errors have to be legible. A connector that stops working says why, in
//     the console, with the last error it saw.

// Writer is how the runner puts a document into the vault. Implemented by the
// server, which owns note writing, indexing and spaces.
type Writer interface {
	// WriteNote creates or replaces a note and returns its path.
	WriteNote(path, body string, frontmatter map[string]any) (string, error)
	// DeleteNote removes one.
	DeleteNote(path string) error
}

// Secrets resolves a credential name to its value. The runner never stores
// what comes back.
type Secrets interface {
	Get(name string) (string, error)
}

// Identities maps a source's own user ids to Grimoire accounts, so a
// document's reader list means something here. Nil on a single-user
// deployment, where there is nobody to restrict a document to.
type Identities interface {
	ResolveIdentities(source string, external []string) (users []string, unmapped int, err error)
}

// Runner executes connectors.
type Runner struct {
	Store      *Store
	Writer     Writer
	Secrets    Secrets
	Identities Identities
	Client     *http.Client
	// Limit bounds documents per fetch.
	Limit int
	// MaxPages bounds how many fetches one run may chain when a source says
	// there is more. Without it a first sync of a large source would hold the
	// run open indefinitely; with it, progress is saved and the next run
	// continues from the cursor.
	MaxPages int

	mu      sync.Mutex
	running map[string]bool
}

// Result reports what one run did.
type Result struct {
	Written int `json:"written"`
	Skipped int `json:"skipped"`
	// Removed counts notes dropped because the source no longer has them —
	// only ever non-zero for a source that enumerated everything. See
	// Page.Complete.
	Removed int    `json:"removed"`
	Cursor  string `json:"cursor"`
	Err     string `json:"error,omitempty"`
}

// Run syncs one connector and records the outcome.
func (r *Runner) Run(ctx context.Context, id string) (Result, error) {
	r.mu.Lock()
	if r.running == nil {
		r.running = map[string]bool{}
	}
	if r.running[id] {
		r.mu.Unlock()
		// Two overlapping runs of one connector would race on the cursor and
		// could write the same document twice.
		return Result{}, fmt.Errorf("connector %s is already running", id)
	}
	r.running[id] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.running, id)
		r.mu.Unlock()
	}()

	c, err := r.Store.Get(id)
	if err != nil {
		return Result{}, err
	}
	source, err := Get(c.Kind)
	if err != nil {
		return Result{}, err
	}

	secret := ""
	if c.Secret != "" {
		if r.Secrets == nil {
			return r.fail(c, fmt.Errorf("this connector needs the credential %q, "+
				"and the secret vault is unavailable", c.Secret))
		}
		secret, err = r.Secrets.Get(c.Secret)
		if err != nil {
			// The most common cause by far, and the least obvious from a
			// generic error: the vault is locked, so no credential resolves.
			return r.fail(c, fmt.Errorf("credential %q: %w (is the vault unlocked?)",
				c.Secret, err))
		}
	}

	limit := r.Limit
	if limit <= 0 {
		limit = 50
	}
	maxPages := r.MaxPages
	if maxPages <= 0 {
		maxPages = 10
	}

	var res Result
	cursor := c.Cursor
	for page := 0; page < maxPages; page++ {
		out, err := source.Fetch(ctx, Input{
			Config: c.Config, Secret: secret, Cursor: cursor,
			Client: r.Client, Limit: limit,
		})
		if err != nil {
			// Persist what this run already wrote: losing a page of progress
			// on a transient failure means re-fetching and re-embedding it.
			c.Cursor = cursor
			res.Cursor = cursor
			r.record(c, res, err)
			return res, err
		}
		written, skipped, err := r.write(c, out.Docs)
		res.Written += written
		res.Skipped += skipped
		if err != nil {
			c.Cursor = cursor
			r.record(c, res, err)
			return res, err
		}
		if out.Cursor != "" {
			cursor = out.Cursor
		}
		if out.Complete {
			removed, err := r.reap(c, out)
			if err != nil {
				c.Cursor = cursor
				r.record(c, res, err)
				return res, err
			}
			res.Removed += removed
		}
		if !out.More || len(out.Docs) == 0 {
			break
		}
	}

	c.Cursor = cursor
	res.Cursor = cursor
	r.record(c, res, nil)
	return res, nil
}

func (r *Runner) fail(c Connector, err error) (Result, error) {
	r.record(c, Result{}, err)
	return Result{Err: err.Error()}, err
}

// record writes the run's outcome back to the connector row.
func (r *Runner) record(c Connector, res Result, err error) {
	c.LastRun = time.Now().UTC().Format(time.RFC3339)
	c.LastOK = err == nil
	outcome := "ok"
	if err != nil {
		outcome = "failed"
	}
	metrics.Count("grimoire_connector_runs_total", "Connector runs by kind and outcome.",
		map[string]string{"kind": c.Kind, "outcome": outcome})
	metrics.Add("grimoire_connector_documents_total", "Documents written by connectors.",
		map[string]string{"kind": c.Kind}, int64(res.Written))
	c.LastErr = ""
	if err != nil {
		c.LastErr = err.Error()
		log.Printf("connector %s (%s): %v", c.Name, c.Kind, err)
	}
	c.Docs += res.Written
	if saveErr := r.Store.Save(c); saveErr != nil {
		log.Printf("connector %s: recording the run failed: %v", c.Name, saveErr)
	}
}

// reap removes notes whose source document is gone.
//
// Only called for a page that claims to be a complete enumeration, because
// only then does "absent from this page" mean "deleted at the source" rather
// than "unchanged since the cursor". Getting that distinction wrong deletes a
// vault, so the claim has to be made explicitly by the source.
func (r *Runner) reap(c Connector, page Page) (int, error) {
	present := make(map[string]bool, len(page.Docs)+len(page.Seen))
	for _, d := range page.Docs {
		present[d.ExternalID] = true
	}
	for _, id := range page.Seen {
		present[id] = true
	}
	known, err := r.Store.docsFor(c.ID)
	if err != nil {
		return 0, err
	}
	removed := 0
	for externalID, rec := range known {
		if present[externalID] {
			continue
		}
		if err := r.Writer.DeleteNote(rec.Path); err != nil {
			// A note somebody has already deleted by hand is not an error.
			log.Printf("connector %s: removing %s: %v", c.Name, rec.Path, err)
		}
		if err := r.Store.forgetDoc(c.ID, externalID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// write turns documents into notes.
func (r *Runner) write(c Connector, docs []Document) (written, skipped int, err error) {
	for _, d := range docs {
		if strings.TrimSpace(d.Body) == "" && strings.TrimSpace(d.Title) == "" {
			skipped++
			continue
		}
		hash := documentHash(d)
		prev, known := r.Store.docFor(c.ID, d.ExternalID)
		if known && prev.Hash == hash {
			// Unchanged since the last sync. Rewriting it would re-embed it
			// and move its modified time for nothing.
			skipped++
			continue
		}

		path := prev.Path
		if path == "" {
			path = notePath(routePrefix(c, d), d)
		}
		fm := map[string]any{
			"title":       d.Title,
			"source":      c.Kind,
			"connector":   c.Name,
			"external_id": d.ExternalID,
			// Provenance the rest of the system enforces on, not just
			// documentation. `source` has been here since connectors shipped
			// and nothing downstream read it; `origin` is the same fact in the
			// form the index, retrieval and the reader prompt all agree on.
			// A pulled document is text other people can write — that is the
			// whole point of pulling it — so it must not be able to instruct
			// an agent. See internal/trust.
			"origin": trust.Connector(c.Kind, c.ID),
		}
		// A reader list from the source, mapped to accounts here. Written into
		// the note's frontmatter so it survives a reindex and is visible to
		// whoever opens the file.
		if len(d.Readers) > 0 && r.Identities != nil {
			users, unmapped, err := r.Identities.ResolveIdentities(c.Kind, d.Readers)
			if err != nil {
				return written, skipped, err
			}
			if len(users) == 0 {
				// Every reader is unmapped: nobody here can read it. Writing it
				// anyway would put a note in the vault that answers to no one —
				// visible in the file tree, absent from every search — which is
				// more confusing than not pulling it.
				skipped++
				continue
			}
			fm["readers"] = strings.Join(users, ", ")
			fm["readers_unmapped"] = fmt.Sprint(unmapped)
		}
		if d.URL != "" {
			fm["url"] = d.URL
		}
		if d.Updated != "" {
			fm["source_updated"] = d.Updated
		}
		if d.Author != "" {
			fm["author"] = d.Author
		}
		for k, v := range d.Meta {
			if v != "" {
				fm[k] = v
			}
		}
		actual, werr := r.Writer.WriteNote(path, body(d), fm)
		if werr != nil {
			return written, skipped, fmt.Errorf("writing %s: %w", path, werr)
		}
		if err := r.Store.putDoc(c.ID, d.ExternalID, docRecord{
			Path: actual, Hash: hash, Updated: d.Updated,
		}); err != nil {
			return written, skipped, err
		}
		written++
	}
	return written, skipped, nil
}

// body renders a document, leading with a heading so a note reads on its own.
func body(d Document) string {
	var b strings.Builder
	if d.Title != "" {
		b.WriteString("# " + d.Title + "\n\n")
	}
	if d.URL != "" {
		b.WriteString("[source](" + d.URL + ")\n\n")
	}
	b.WriteString(strings.TrimSpace(d.Body))
	b.WriteString("\n")
	return b.String()
}

// documentHash decides whether a document changed. It covers everything that
// ends up in the note, so a title-only edit is still a change.
func documentHash(d Document) string {
	h := sha256.New()
	fmt.Fprint(h, d.Title, "\x00", d.Body, "\x00", d.URL, "\x00", d.Author)
	for _, k := range sortedKeys(d.Meta) {
		fmt.Fprint(h, "\x00", k, "=", d.Meta[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Routing a document to a folder, and therefore to an access boundary.
//
// This is as close to permission sync as an honest implementation gets without
// mirroring per-document ACLs. A source has structure — a Confluence space, a
// Slack channel, a Jira project, a Drive folder — and that structure is usually
// what its permissions follow. Routing on it lets an operator put each part of
// a source into a Grimoire space whose membership matches, so access is
// enforced by the same mechanism as everything else.
//
// What it is NOT: a copy of the source's ACL. If someone loses access in
// Confluence, nothing here notices. That limit is in the README, and this
// feature narrows it rather than closing it — an operator can now separate
// "Engineering's wiki" from "HR's wiki" instead of pulling both into one folder
// that everyone with access to the connector can read.
//
// The rule is deliberately plain: a metadata field, and a mapping from its
// values to folders. Anything cleverer would need a language, and a language
// needs a debugger.

// routePrefix picks a document's destination folder.
//
// route_by names a metadata field (space, channel, project, repo…); route_map
// maps its values to folders, as "value=folder" pairs. A value with no mapping
// falls back to the connector's own prefix, so a rule that does not cover
// everything still puts the rest somewhere sensible rather than dropping it.
func routePrefix(c Connector, d Document) string {
	field := c.Config.Get("route_by")
	if field == "" {
		return c.Prefix
	}
	value := strings.TrimSpace(d.Meta[field])
	if value == "" {
		return c.Prefix
	}
	for _, pair := range strings.Split(c.Config.Get("route_map"), ",") {
		name, folder, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(name), value) {
			if f := strings.Trim(strings.TrimSpace(folder), "/"); f != "" {
				return f
			}
		}
	}
	// No explicit mapping: a subfolder named after the value keeps documents
	// separated by source structure even when the operator has not said where
	// each one goes, which is the arrangement a space can be drawn around later.
	if strings.EqualFold(c.Config.Get("route_map"), "") {
		return strings.Trim(c.Prefix, "/") + "/" + slugify(value, 40)
	}
	return c.Prefix
}

var (
	unsafePath = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)
	// A run of dots is collapsed rather than kept: the vault's own path
	// sandbox already refuses traversal, but a file literally named
	// "..-..-etc-passwd.md" is a thing an operator has to think about, and
	// there is no reason to make them.
	dotRunRE  = regexp.MustCompile(`\.{2,}`)
	dashRunRE = regexp.MustCompile(`-{2,}`)
)

// slugify reduces a title or an id to something safe and readable as a filename.
func slugify(s string, max int) string {
	s = dotRunRE.ReplaceAllString(strings.ToLower(s), "-")
	s = unsafePath.ReplaceAllString(s, "-")
	s = dashRunRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	if max > 0 && len([]rune(s)) > max {
		s = string([]rune(s)[:max])
	}
	return strings.Trim(s, "-.")
}

// notePath is where a document lands: the connector's prefix, then a slug of
// its title, then its external id to keep two documents with the same title
// apart. The id goes in the filename rather than being the filename, because a
// vault is meant to be readable in a file browser.
func notePath(prefix string, d Document) string {
	slug := slugify(d.Title, 60)
	id := slugify(d.ExternalID, 0)
	if len([]rune(id)) > 40 {
		// Keep the END of a long id: the distinguishing part of a path or a
		// URL-shaped id is at the end, not the start.
		id = strings.Trim(string([]rune(id)[len([]rune(id))-40:]), "-.")
	}
	name := strings.Trim(slug+"-"+id, "-.")
	if name == "" {
		name = "document"
	}
	return strings.Trim(prefix, "/") + "/" + name + ".md"
}

// Due returns the connectors owed a scheduled run.
func (r *Runner) Due(now time.Time) ([]Connector, error) {
	all, err := r.Store.List()
	if err != nil {
		return nil, err
	}
	var out []Connector
	for _, c := range all {
		if c.Due(now) {
			out = append(out, c)
		}
	}
	return out, nil
}

// Loop runs due connectors until done is closed. One at a time: these are
// network-bound and a self-hosted instance would rather be polite to the
// systems it pulls from than fast.
func (r *Runner) Loop(interval time.Duration, done <-chan struct{}) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-tick.C:
			due, err := r.Due(now)
			if err != nil {
				continue
			}
			// After a restart every connector is due at once, which would hit
			// six APIs in the same second and look like a burst to each of
			// them. They run one at a time anyway; this spreads the first
			// round rather than stacking it.
			if len(due) > 1 {
				due = due[:1]
			}
			for _, c := range due {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				if _, err := r.Run(ctx, c.ID); err != nil {
					// Already recorded on the connector; the log line is for
					// an operator watching the service.
					log.Printf("connector %s: %v", c.Name, err)
				}
				cancel()
				select {
				case <-done:
					return
				default:
				}
			}
		}
	}
}
