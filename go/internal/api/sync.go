package api

import (
	"archive/zip"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/crdtstore"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
	gsync "github.com/JeremiahM37/grimoire/go/internal/sync"
)

// Note encryption, CRDT sync, and whole-vault export/import.

// encryptNote seals a note's body at rest with the vault key.
func (s *Server) encryptNote(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	if !s.Secrets.IsUnlocked() {
		writeErr(w, http.StatusLocked, "unlock the secret vault first")
		return
	}
	note, err := s.Vault.Read(rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such note")
		return
	}
	if !note.Encrypted {
		sealed, err := s.Secrets.SealText(note.Body)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		fm := note.Frontmatter.Clone()
		fm.Set("encrypted", true)
		fm.Set("private", true) // an encrypted note is private by construction
		if _, err := s.Vault.Write(rel, sealed, fm); err != nil {
			writeErr(w, statusForVaultErr(err), err.Error())
			return
		}
		// replica state for a now-encrypted note must go: it holds plaintext
		if s.CRDT != nil {
			_ = s.CRDT.DeleteDoc(rel)
		}
		if _, err := s.Index.Upsert(rel); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	updated, err := s.Vault.Read(rel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.viewOf(updated))
}

// decryptNote removes at-rest encryption, restoring plain markdown.
func (s *Server) decryptNote(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	if !s.Secrets.IsUnlocked() {
		writeErr(w, http.StatusLocked, "unlock the secret vault first")
		return
	}
	note, err := s.Vault.Read(rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such note")
		return
	}
	if note.Encrypted {
		plain, err := s.Secrets.UnsealText(note.Body)
		if err != nil {
			if err == secrets.ErrLocked {
				writeErr(w, http.StatusLocked, "vault locked")
				return
			}
			writeErr(w, http.StatusBadRequest, "cannot decrypt — wrong vault key")
			return
		}
		fm := note.Frontmatter.Clone()
		fm.Delete("encrypted")
		if _, err := s.Vault.Write(rel, plain, fm); err != nil {
			writeErr(w, statusForVaultErr(err), err.Error())
			return
		}
		if _, err := s.Index.Upsert(rel); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	updated, err := s.Vault.Read(rel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.viewOf(updated))
}

// ---------------------------------------------------------------- CRDT sync

func (s *Server) getCRDTDoc(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	note, err := s.Vault.Read(rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such note")
		return
	}
	if !crdtstore.Mergeable(rel, note.Body) {
		writeErr(w, http.StatusConflict, "note is not CRDT-mergeable (encrypted or too large)")
		return
	}
	doc, err := s.CRDT.BodyDocJSON(rel, note.Body)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	fm := map[string]any{}
	for _, k := range note.Frontmatter.Keys() {
		v, _ := note.Frontmatter.Get(k)
		fm[k] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "doc": doc, "fm": fm})
}

type mergeIn struct {
	Path string         `json:"path"`
	Doc  string         `json:"doc"`
	FM   map[string]any `json:"fm"`
}

// mergeCRDT folds a peer's document into ours and writes the converged note.
func (s *Server) mergeCRDT(w http.ResponseWriter, r *http.Request) {
	var in mergeIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	rel := normPath(in.Path)
	body := ""
	fm := markdown.NewFrontmatter()
	if note, err := s.Vault.Read(rel); err == nil {
		if note.Encrypted {
			// never merge into ciphertext: it would destroy the note
			writeErr(w, http.StatusConflict, "note is encrypted — not mergeable")
			return
		}
		body = note.Body
		fm = note.Frontmatter.Clone()
	}
	merged, err := s.CRDT.Merge(rel, body, in.Doc)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid peer document: "+err.Error())
		return
	}
	// peer frontmatter fills only keys we don't have — a peer must not silently
	// overwrite local metadata
	for k, v := range in.FM {
		if _, exists := fm.Get(k); !exists {
			fm.Set(k, v)
		}
	}
	if _, err := s.Vault.Write(rel, merged, fm); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	note, err := s.Index.Upsert(rel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": rel, "merged": true, "body": note.Body,
	})
}

func (s *Server) syncStatus(w http.ResponseWriter, _ *http.Request) {
	var peer any
	if s.SyncPeer != "" {
		peer = s.SyncPeer
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"peer": peer, "interval": s.SyncInterval})
}

// syncManifest lists every note with its content hash and mtime, so a peer can
// work out what it is missing without transferring bodies. Keyed by path —
// this is the wire format a peer of EITHER implementation expects.
func (s *Server) syncManifest(w http.ResponseWriter, r *http.Request) {
	if !s.allowSync(w, r) {
		return
	}
	m, err := s.Sync.LocalManifest()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.readableManifest(r, m))
}

// syncPull returns the raw text of the requested notes. A note missing here is
// reported as null rather than omitted, so the caller can tell "deleted on the
// peer" from "never asked for".
func (s *Server) syncPull(w http.ResponseWriter, r *http.Request) {
	if !s.allowSync(w, r) {
		return
	}
	var in struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	out := map[string]*string{}
	for _, rel := range in.Paths {
		// A note the caller may not read is reported as absent, exactly like
		// one that does not exist: sync is a bulk read of note BODIES, and it
		// answered every one of them to anybody before this check existed.
		if !s.canReadSync(r, rel) {
			out[rel] = nil
			continue
		}
		note, err := s.Vault.Read(rel)
		if err != nil {
			out[rel] = nil
			continue
		}
		raw := note.Raw
		out[rel] = &raw
	}
	writeJSON(w, http.StatusOK, map[string]any{"contents": out})
}

// syncPush accepts a peer's changes. A change whose base_hash no longer matches
// becomes a conflict copy: the local version is never overwritten blindly.
func (s *Server) syncPush(w http.ResponseWriter, r *http.Request) {
	if !s.allowSync(w, r) {
		return
	}
	var in struct {
		Changes []gsync.Change `json:"changes"`
		Device  string         `json:"device"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	results := make([]gsync.Result, 0, len(in.Changes))
	for _, ch := range in.Changes {
		// Push WRITES. Requiring a principal to call the route said nothing
		// about where they may write, so a member could push over a
		// colleague's personal note or into a space they only read.
		if !s.canWriteSync(r, ch.Path) {
			results = append(results, gsync.Result{Path: ch.Path, Status: "refused",
				Detail: "you cannot write there"})
			continue
		}
		results = append(results, s.Sync.Apply(ch))
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// syncNow runs a bidirectional sync with the configured peer right now.
func (s *Server) syncNow(w http.ResponseWriter, _ *http.Request) {
	if s.SyncPeer == "" {
		writeErr(w, http.StatusBadRequest, "no peer configured (set GRIMOIRE_SYNC_PEER)")
		return
	}
	st, err := s.Sync.SyncWithPeer(s.SyncPeer, "manual", s.SyncToken)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "sync failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ---------------------------------------------------------------- export/import

// exportVault streams the whole vault as a zip — plain markdown, so the archive
// is useful without grimoire.
func (s *Server) exportVault(w http.ResponseWriter, r *http.Request) {
	// A zip of the whole vault is the most complete read there is, and it
	// answered anyone. It exports what the caller can read — which for a
	// single-user deployment is still everything.
	if !s.requireUser(w, r) {
		return
	}
	rels, err := s.Vault.Walk()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="grimoire-vault.zip"`)
	zw := zip.NewWriter(w)
	defer zw.Close()
	for _, rel := range rels {
		if !s.canRead(r, rel) {
			continue
		}
		// An export is the most complete read there is, so each restricted
		// document in it is recorded exactly as opening it one at a time
		// would be.
		s.auditRead(r, rel, true)
		note, err := s.Vault.Read(rel)
		if err != nil {
			continue
		}
		f, err := zw.Create(rel)
		if err != nil {
			return
		}
		if _, err := f.Write([]byte(note.Raw)); err != nil {
			return
		}
	}
}

// importVault unpacks a zip into the vault. Every entry is confined through
// SafePath — a zip is untrusted input, and "zip slip" entries like
// ../../.ssh/authorized_keys are exactly what that guards against.
func (s *Server) importVault(w http.ResponseWriter, r *http.Request) {
	// Importing writes notes into the vault from an uploaded archive — the
	// widest write there is, and it answered anyone.
	if !s.requireUser(w, r) {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 200<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	zr, err := zip.NewReader(strings.NewReader(string(raw)), int64(len(raw)))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "not a zip archive")
		return
	}
	imported, skipped := 0, 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !strings.HasSuffix(f.Name, ".md") {
			skipped++
			continue
		}
		if _, err := s.Vault.SafePath(f.Name); err != nil {
			skipped++ // zip-slip or reserved path: refused, not cleaned
			continue
		}
		rc, err := f.Open()
		if err != nil {
			skipped++
			continue
		}
		content, err := io.ReadAll(io.LimitReader(rc, 10<<20))
		rc.Close()
		if err != nil {
			skipped++
			continue
		}
		fm, body := markdown.ParseFrontmatter(string(content))
		if !s.canWrite(r, normPath(f.Name)) {
			skipped++
			continue
		}
		if _, err := s.Vault.Write(f.Name, body, fm); err != nil {
			skipped++
			continue
		}
		imported++
	}
	if _, err := s.Index.Reindex(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"imported": imported, "skipped": skipped})
}

// Who may sync, and to what.
//
// Sync predates every access control in this server, and moved whole-vault
// content without consulting any of them: the manifest listed every note and
// pull returned any body, to an UNAUTHENTICATED caller on a multi-user
// instance. It is a bulk read of the vault, so it answers to the same rules as
// every other read.
//
// A peer holding the sync token is the deployment's own other device — that is
// what the token is for — and keeps whole-vault access. Anyone else is a
// principal, and gets what that principal can read.

// allowSync gates the peer routes.
func (s *Server) allowSync(w http.ResponseWriter, r *http.Request) bool {
	if s.isSyncPeer(r) {
		return true
	}
	p := principal(r)
	if p.Unrestricted || !p.Anonymous {
		return true
	}
	writeErr(w, http.StatusUnauthorized, "sign in, or present the sync token")
	return false
}

// isSyncPeer reports whether the caller authenticated as the deployment's own
// peer rather than as a person.
func (s *Server) isSyncPeer(r *http.Request) bool {
	if strings.TrimSpace(s.SyncToken) == "" {
		return false
	}
	presented, _ := presentedToken(r)
	if presented == "" {
		presented = strings.TrimSpace(r.Header.Get("X-Grimoire-Sync"))
	}
	want := sha256.Sum256([]byte(s.SyncToken))
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// canWriteSync is the write half: a peer is the deployment's own device, and
// anyone else may only write where they could write through any other route.
func (s *Server) canWriteSync(r *http.Request, path string) bool {
	if s.isSyncPeer(r) {
		return true
	}
	return s.canWrite(r, normPath(path))
}

// canReadSync is canRead for the sync routes, with the peer exemption.
func (s *Server) canReadSync(r *http.Request, path string) bool {
	if s.isSyncPeer(r) {
		return true
	}
	return s.canRead(r, normPath(path))
}

// readableManifest drops entries the caller may not read. The manifest is a
// list of paths and hashes — which is to say, a list of what exists and when it
// changed, and that is worth hiding on its own.
func (s *Server) readableManifest(r *http.Request, m map[string]gsync.Entry) map[string]gsync.Entry {
	if s.isSyncPeer(r) || principal(r).Unrestricted {
		return m
	}
	// Read the reader lists in one pass rather than per path: this runs over
	// the whole vault, and a query per note would be both slow and, inside the
	// wrong loop, a deadlock.
	acls := map[string]string{}
	rows, err := s.Index.DB.Query("SELECT path, acl FROM notes")
	if err == nil {
		for rows.Next() {
			var path, acl string
			if err := rows.Scan(&path, &acl); err == nil {
				acls[path] = acl
			}
		}
		rows.Close()
	}
	out := make(map[string]gsync.Entry, len(m))
	for path, entry := range m {
		if s.canReadNote(r, path, acls[path]) {
			out[path] = entry
		}
	}
	return out
}
