package api

import (
	"archive/zip"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/crdtstore"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
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
	notes, _ := s.Index.DB.Count("SELECT COUNT(*) FROM notes")
	writeJSON(w, http.StatusOK, map[string]any{
		"site_id": s.CRDT.SiteID(),
		"notes":   notes,
		"peer":    s.SyncPeer,
	})
}

// syncManifest lists every note with its content hash, so a peer can work out
// what it is missing without transferring bodies.
func (s *Server) syncManifest(w http.ResponseWriter, _ *http.Request) {
	rows, err := s.Index.DB.Query("SELECT path, hash, updated FROM notes ORDER BY path")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var path, hash, updated string
		if err := rows.Scan(&path, &hash, &updated); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, map[string]string{"path": path, "hash": hash, "updated": updated})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site_id": s.CRDT.SiteID(), "notes": out,
	})
}

// ---------------------------------------------------------------- export/import

// exportVault streams the whole vault as a zip — plain markdown, so the archive
// is useful without grimoire.
func (s *Server) exportVault(w http.ResponseWriter, _ *http.Request) {
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
