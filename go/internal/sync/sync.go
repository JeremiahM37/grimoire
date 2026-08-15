// Package sync is delta sync with a peer grimoire — local-first, conflict
// copies, never silent data loss.
//
// Port of server/syncclient.py and server/routers/sync.py. The wire protocol is
// the Python one, unchanged, because the point of syncing is to talk to a peer
// that may be running either implementation:
//
//  1. GET  /api/sync/manifest   → {path: {hash, mtime}}
//  2. diff against the local manifest
//  3. POST /api/sync/pull       → contents to bring down
//  4. POST /api/sync/push       → contents to push up; if the peer's current
//     hash differs from the client's base_hash (a concurrent edit), the
//     incoming version becomes a CONFLICT COPY and the peer's copy is kept.
//
// Direction is decided by mtime (last writer), but data is never lost: before a
// pull OVERWRITES a differing local note, the local version is preserved as a
// conflict copy first.
package sync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/crdtstore"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Entry is one manifest row.
type Entry struct {
	Hash  string  `json:"hash"`
	Mtime float64 `json:"mtime"`
}

// Change is one pushed note. Content nil means delete.
type Change struct {
	Path     string  `json:"path"`
	Content  *string `json:"content"`
	BaseHash *string `json:"base_hash"`
}

// Result reports what the peer did with one pushed change.
type Result struct {
	Path         string `json:"path"`
	Status       string `json:"status"`
	Detail       string `json:"detail,omitempty"`
	ConflictCopy string `json:"conflict_copy,omitempty"`
}

// Stats is the summary a sync returns.
type Stats struct {
	Pulled      int `json:"pulled"`
	Pushed      int `json:"pushed"`
	Merged      int `json:"merged"`
	Conflicts   int `json:"conflicts"`
	RemoteNotes int `json:"remote_notes"`
	LocalNotes  int `json:"local_notes"`
}

// Client syncs a local vault with a peer.
type Client struct {
	Index *index.Index
	Vault *vault.Vault
	CRDT  *crdtstore.Store
	HTTP  *http.Client
}

func New(ix *index.Index, v *vault.Vault, c *crdtstore.Store) *Client {
	return &Client{Index: ix, Vault: v, CRDT: c,
		HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) req(u, method string, body any, token string, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s: %s", method, u, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// LocalManifest is what this vault currently holds.
func (c *Client) LocalManifest() (map[string]Entry, error) {
	rows, err := c.Index.DB.Query("SELECT path, hash, mtime FROM notes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Entry{}
	for rows.Next() {
		var path, hash string
		var mtime float64
		if err := rows.Scan(&path, &hash, &mtime); err != nil {
			return nil, err
		}
		out[path] = Entry{Hash: hash, Mtime: mtime}
	}
	return out, rows.Err()
}

// WriteRaw stores the full note text verbatim, frontmatter and all. Sync moves
// whole files: re-serializing would rewrite a peer's formatting on every hop.
func (c *Client) WriteRaw(rel, raw string) error {
	p, err := c.Vault.SafePath(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(raw), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ConflictName is where a losing version is preserved.
func ConflictName(rel string) string {
	base := strings.TrimSuffix(rel, ".md")
	return fmt.Sprintf("%s (conflict %s).md", base, vault.Now().Format("20060102-150405"))
}

// Apply is the server half: accept one pushed change, refusing to overwrite a
// note that changed underneath the pusher.
func (c *Client) Apply(ch Change) Result {
	var curHash string
	haveLocal := c.Index.DB.QueryRow(
		"SELECT hash FROM notes WHERE path=?", ch.Path).Scan(&curHash) == nil

	if ch.Content == nil { // delete request
		if haveLocal && ch.BaseHash != nil && curHash != *ch.BaseHash {
			return Result{Path: ch.Path, Status: "conflict-keep",
				Detail: "changed on server; not deleting"}
		}
		_ = c.Vault.Delete(ch.Path)
		_ = c.Index.Remove(ch.Path)
		return Result{Path: ch.Path, Status: "deleted"}
	}

	if haveLocal && ch.BaseHash != nil && curHash != *ch.BaseHash {
		copyRel := ConflictName(ch.Path)
		if err := c.WriteRaw(copyRel, *ch.Content); err != nil {
			return Result{Path: ch.Path, Status: "error", Detail: err.Error()}
		}
		if _, err := c.Index.Upsert(copyRel); err != nil {
			return Result{Path: ch.Path, Status: "error", Detail: err.Error()}
		}
		return Result{Path: ch.Path, Status: "conflict", ConflictCopy: copyRel}
	}

	if err := c.WriteRaw(ch.Path, *ch.Content); err != nil {
		return Result{Path: ch.Path, Status: "error", Detail: err.Error()}
	}
	if _, err := c.Index.Upsert(ch.Path); err != nil {
		return Result{Path: ch.Path, Status: "error", Detail: err.Error()}
	}
	if haveLocal {
		return Result{Path: ch.Path, Status: "ok"}
	}
	return Result{Path: ch.Path, Status: "created"}
}

func (c *Client) localConflictCopy(path string) bool {
	note, err := c.Vault.Read(path)
	if err != nil {
		return false
	}
	copyRel := ConflictName(path)
	if c.WriteRaw(copyRel, note.Raw) != nil {
		return false
	}
	_, _ = c.Index.Upsert(copyRel)
	return true
}

// SyncWithPeer diffs manifests and reconciles both directions.
//
// Every differing path is handled CRDT-first so both replicas end up sharing
// atom ids; a note that cannot be merged as text (encrypted) falls back to
// last-writer pull/push.
func (c *Client) SyncWithPeer(peer, device, token string) (Stats, error) {
	peer = strings.TrimRight(peer, "/")
	var remote map[string]Entry
	if err := c.req(peer+"/api/sync/manifest", "GET", nil, token, &remote); err != nil {
		return Stats{}, err
	}
	local, err := c.LocalManifest()
	if err != nil {
		return Stats{}, err
	}
	st := Stats{RemoteNotes: len(remote), LocalNotes: len(local)}

	paths := map[string]bool{}
	for p := range local {
		paths[p] = true
	}
	for p := range remote {
		paths[p] = true
	}

	var toPull []string
	var pushChanges []Change

	for path := range paths {
		lm, haveLocal := local[path]
		rm, haveRemote := remote[path]
		if haveLocal && haveRemote && lm.Hash == rm.Hash {
			continue // already in sync
		}

		var ourBody, ourRaw string
		if haveLocal {
			if note, err := c.Vault.Read(path); err == nil {
				ourBody, ourRaw = note.Body, note.Raw
			}
		}

		// not mergeable locally (encrypted) → last writer wins
		if haveLocal && !crdtstore.Mergeable(path, ourBody) {
			if haveRemote && rm.Mtime > lm.Mtime {
				toPull = append(toPull, path)
			} else {
				pushChanges = append(pushChanges, Change{
					Path: path, Content: strPtr(ourRaw), BaseHash: hashPtr(haveRemote, rm.Hash)})
			}
			continue
		}

		if err := c.crdtExchange(peer, path, ourBody, ourRaw, token,
			haveLocal, haveRemote, &st); err != nil {
			// peer 409 (encrypted there) or transient → last-writer fallback
			if haveRemote && (!haveLocal || rm.Mtime > lm.Mtime) {
				toPull = append(toPull, path)
			} else if haveLocal {
				pushChanges = append(pushChanges, Change{
					Path: path, Content: strPtr(ourRaw), BaseHash: hashPtr(haveRemote, rm.Hash)})
			}
		}
	}

	if len(toPull) > 0 {
		var pulled struct {
			Contents map[string]*string `json:"contents"`
		}
		if err := c.req(peer+"/api/sync/pull", "POST",
			map[string]any{"paths": toPull}, token, &pulled); err != nil {
			return st, err
		}
		for path, raw := range pulled.Contents {
			if raw == nil {
				continue
			}
			if _, had := local[path]; had { // about to overwrite — preserve ours
				if c.localConflictCopy(path) {
					st.Conflicts++
				}
			}
			if c.WriteRaw(path, *raw) != nil {
				continue
			}
			if _, err := c.Index.Upsert(path); err != nil {
				continue
			}
			st.Pulled++
		}
	}

	if len(pushChanges) > 0 {
		var pushed struct {
			Results []Result `json:"results"`
		}
		if err := c.req(peer+"/api/sync/push", "POST",
			map[string]any{"changes": pushChanges, "device": device}, token, &pushed); err != nil {
			return st, err
		}
		for _, r := range pushed.Results {
			switch {
			case r.Status == "ok" || r.Status == "created" || r.Status == "deleted":
				st.Pushed++
			case strings.HasPrefix(r.Status, "conflict"):
				st.Conflicts++
			}
		}
	}
	log.Printf("sync %s: %+v", peer, st)
	return st, nil
}

// crdtExchange merges the peer's body document into ours and pushes ours back,
// so both replicas converge on shared atom ids rather than one overwriting the
// other. Our own document is snapshotted BEFORE the merge, because pushing the
// post-merge document would tell the peer its own edits were ours.
func (c *Client) crdtExchange(peer, path, ourBody, ourRaw, token string,
	haveLocal, haveRemote bool, st *Stats) error {
	var ourDoc string
	if haveLocal {
		var err error
		if ourDoc, err = c.CRDT.BodyDocJSON(path, ourBody); err != nil {
			return err
		}
	}
	if haveRemote {
		var pr struct {
			Doc json.RawMessage `json:"doc"`
		}
		enc := (&url.URL{Path: path}).EscapedPath()
		if err := c.req(peer+"/api/crdt/doc/"+enc, "GET", nil, token, &pr); err != nil {
			return err
		}
		mergedBody, err := c.CRDT.Merge(path, ourBody, string(pr.Doc))
		if err != nil {
			return err
		}
		if mergedBody != ourBody {
			if haveLocal && strings.TrimSpace(ourRaw) != "" {
				if c.localConflictCopy(path) {
					st.Conflicts++
				}
			}
			if err := c.writeBody(path, mergedBody); err != nil {
				return err
			}
		}
		if haveLocal {
			st.Merged++
		} else {
			st.Pulled++
		}
	}
	if haveLocal {
		if err := c.req(peer+"/api/crdt/merge", "POST",
			map[string]any{"path": path, "doc": json.RawMessage(ourDoc)}, token, nil); err != nil {
			return err
		}
		if !haveRemote {
			st.Pushed++
		}
	}
	return nil
}

// writeBody rewrites a note's body while keeping its frontmatter, so a merge
// does not drop the note's metadata.
func (c *Client) writeBody(rel, body string) error {
	note, err := c.Vault.Read(rel)
	if err != nil {
		if _, err := c.Vault.Write(rel, body, nil); err != nil {
			return err
		}
	} else if _, err := c.Vault.Write(rel, body, note.Frontmatter); err != nil {
		return err
	}
	_, err = c.Index.Upsert(rel)
	return err
}

func strPtr(s string) *string { return &s }

func hashPtr(have bool, h string) *string {
	if !have {
		return nil
	}
	return &h
}

// Loop runs a sync every interval until ctx-like stop channel closes. Errors
// are logged and retried — a peer that is offline right now is the normal case
// for a laptop, not a fault.
func (c *Client) Loop(peer, token string, interval time.Duration, stop <-chan struct{}) {
	if peer == "" || interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if _, err := c.SyncWithPeer(peer, "timer", token); err != nil {
				log.Printf("sync with %s failed: %v", peer, err)
			}
		}
	}
}
