package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/documents"
)

const maxDocumentUpload = 25 << 20

func (s *Server) importDocument(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if s.Documents == nil {
		writeErr(w, http.StatusServiceUnavailable, "document importer unavailable")
		return
	}
	if err := r.ParseMultipartForm(maxDocumentUpload); err != nil {
		writeErr(w, http.StatusBadRequest, "expected a multipart document upload")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	f, h, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field")
		return
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxDocumentUpload+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(b) > maxDocumentUpload {
		writeErr(w, http.StatusRequestEntityTooLarge, "document too large (25 MB max)")
		return
	}
	name := filepath.Base(strings.ReplaceAll(h.Filename, "\\", "/"))
	if name == "." || name == "" {
		writeErr(w, http.StatusBadRequest, "file must have a supported filename")
		return
	}
	path := strings.TrimSpace(r.FormValue("path"))
	sum := sha256.Sum256(append([]byte("upload:"), b...))
	source := "upload:" + hex.EncodeToString(sum[:])[:24]
	if path != "" {
		if owned, ok := s.Documents.SourceForPath(path); ok {
			source = owned
		} else {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
	}
	target := s.Documents.TargetPath(source, name)
	if path != "" {
		target = path
		if !s.canReadDocument(r, target) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
	}
	if !s.requireWrite(w, r, target) {
		return
	}
	var result documents.Result
	if path != "" {
		result, err = s.Documents.ImportBytesAt(source, name, b, target)
	} else {
		result, err = s.Documents.ImportBytes(source, name, b)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) listDocuments(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if s.Documents == nil {
		writeErr(w, http.StatusServiceUnavailable, "document importer unavailable")
		return
	}
	recs, err := s.Documents.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type item struct {
		Path       string `json:"path"`
		SourcePath string `json:"source_path"`
		Title      string `json:"title"`
		Format     string `json:"format"`
		Status     string `json:"status"`
		Error      string `json:"error,omitempty"`
	}
	out := make([]item, 0, len(recs))
	for _, d := range recs {
		if !s.canReadDocument(r, d.Path) {
			continue
		}
		out = append(out, item{d.Path, d.SourcePath, d.Title, d.Format, d.Status, d.Error})
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": out})
}

func (s *Server) refreshDocument(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if s.Documents == nil {
		writeErr(w, http.StatusServiceUnavailable, "document importer unavailable")
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "expected JSON {path}")
		return
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = strings.TrimSpace(r.URL.Query().Get("path"))
	}
	if path == "" {
		path = r.PathValue("path")
	}
	if path == "" || !s.canReadDocument(r, path) || !s.canWrite(r, path) {
		writeErr(w, http.StatusNotFound, "document not found")
		return
	}
	result, err := s.Documents.Refresh(path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// serveDocumentOriginal is the only original-file endpoint. It checks the
// owning generated note's ACL/private boundary before opening the reserved
// attachment, so .grimoire cannot become an ACL bypass.
func (s *Server) serveDocumentOriginal(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		path = r.PathValue("path")
	}
	if path == "" || !s.canReadDocument(r, path) {
		writeErr(w, http.StatusNotFound, "document not found")
		return
	}
	b, ext, err := s.Documents.Original(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "original unavailable")
		return
	}
	ct := mime.TypeByExtension(ext)
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", `attachment; filename="original`+filepath.Ext(ext)+`"`)
	http.ServeContent(w, r, "original", time.Unix(0, 0), bytes.NewReader(b))
}

func (s *Server) canReadDocument(r *http.Request, path string) bool {
	if s.Vault == nil {
		return false
	}
	n, err := s.Vault.Read(path)
	if err != nil {
		return false
	}
	// requireRead is the canonical ACL/space check for the owning note.
	p := principal(r)
	if n.Private && !p.Unrestricted && !p.IsAdmin() {
		return false
	}
	return s.canRead(r, n.Path)
}
