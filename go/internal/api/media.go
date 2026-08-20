package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Attachments and canvases.
//
// Attachments live INSIDE the vault so they sync with the notes that embed
// them — an image stored elsewhere would silently break on another device.

// AttachDir is where uploads land, relative to the vault root.
const AttachDir = "attachments"

// MaxAttachBytes caps an upload. A vault is for notes; a 25 MB ceiling keeps a
// misdirected upload from filling the disk (and the user's phone, via sync).
const MaxAttachBytes = 25 << 20

// MaxCanvasBytes caps a canvas document — a canvas is metadata, not a data store.
const MaxCanvasBytes = 1 << 20

var imageExts = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "gif": true, "webp": true,
	"svg": true, "avif": true, "bmp": true, "heic": true,
}

// attach stores an uploaded file and returns the relative path the editor
// embeds as ![[path]] for an image or [[path]] for anything else.
func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	// Uploading writes a file into the vault, so it is a write: an
	// unauthenticated caller must not be able to fill a disk with it.
	if !s.requireUser(w, r) {
		return
	}
	if err := r.ParseMultipartForm(MaxAttachBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "expected a multipart upload")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field")
		return
	}
	defer file.Close()

	// read one byte past the limit so an oversized upload is detected rather
	// than silently truncated
	data, err := io.ReadAll(io.LimitReader(file, MaxAttachBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(data) > MaxAttachBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "attachment too large (25 MB max)")
		return
	}

	name := header.Filename
	// a client may send a path; keep only the final component
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	if name == "" {
		name = "file"
	}
	base, ext := name, "bin"
	if i := strings.LastIndex(name, "."); i > 0 {
		base, ext = name[:i], strings.ToLower(name[i+1:])
		if len(ext) > 8 {
			ext = ext[:8]
		}
	}
	slug := vault.Slugify(base)
	if len(slug) > 40 {
		slug = slug[:40]
	}
	if slug == "" || slug == "untitled" {
		slug = "file"
	}
	rel := fmt.Sprintf("%s/%s-%s.%s", AttachDir, vault.Now().Format("20060102-150405"), slug, ext)

	// SafeRawPath, not SafePath: attachments must keep their real extension
	p, err := s.Vault.SafeRawPath(rel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"path": rel, "is_image": imageExts[ext], "name": name, "bytes": len(data),
	})
}

// serveFile serves a raw vault file for embeds and the read surface.
// executable reports whether a browser would run this file if it rendered it.
func executable(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".htm", ".xhtml", ".svg", ".xml", ".js", ".mjs", ".mhtml", ".xsl":
		return true
	}
	return false
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	// Attachments live beside the notes that reference them, so they inherit
	// the space of their path. Serving them unchecked would make every private
	// image and PDF readable by URL.
	if !s.requireRead(w, r, normPath(r.PathValue("path"))) {
		return
	}
	p, err := s.Vault.SafeRawPath(strings.Trim(r.PathValue("path"), "/"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad path")
		return
	}
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		writeErr(w, http.StatusNotFound, "no such file")
		return
	}
	// nosniff does NOT make an uploaded document safe, which the comment here
	// used to claim: it stops MIME SNIFFING, and an .html or .svg served with
	// its own correct content type still executes — same origin, same session,
	// same everything. An attachment is a file somebody uploaded; it must not
	// be able to act as the app.
	//
	// Two defences, because either alone has gaps. A sandbox CSP puts the
	// response in an opaque origin with no script, so even a rendered document
	// can reach nothing. And the types that execute are sent as downloads
	// rather than rendered at all, since nobody uploads an .html attachment to
	// a notes app expecting it to run.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if executable(p) {
		w.Header().Set("Content-Disposition",
			"attachment; filename="+strconv.Quote(filepath.Base(p)))
	}
	http.ServeFile(w, r, p)
}

// ---------------------------------------------------------------- canvases

// canvasPath resolves a canvas name to its .canvas file.
func (s *Server) canvasPath(rel string) (string, string, error) {
	if !strings.HasSuffix(rel, ".canvas") {
		rel += ".canvas"
	}
	p, err := s.Vault.SafeRawPath(rel)
	return rel, p, err
}

// validateCanvas checks structure, not semantics: unknown keys are preserved
// verbatim so boards round-trip with other JSON Canvas apps, but the shape must
// be right and ids must be strings.
func validateCanvas(raw []byte) (map[string]any, error) {
	if len(raw) > MaxCanvasBytes {
		return nil, fmt.Errorf("canvas too large (1 MB max)")
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("canvas must be a JSON object")
	}
	for _, key := range []string{"nodes", "edges"} {
		v, ok := doc[key]
		if !ok {
			doc[key] = []any{}
			continue
		}
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("nodes and edges must be lists")
		}
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("every %s entry must be an object", key)
			}
			if _, ok := m["id"].(string); !ok {
				return nil, fmt.Errorf("every %s entry needs a string id", key)
			}
		}
	}
	return doc, nil
}

type canvasIn struct {
	Name string `json:"name"`
}

// createCanvas makes an empty board under canvases/, the directory the console
// expects them in.
func (s *Server) createCanvas(w http.ResponseWriter, r *http.Request) {
	var in canvasIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	slug := vault.Slugify(in.Name)
	if slug == "" || slug == "untitled" {
		slug = "canvas"
	}
	rel, p, err := s.canvasPath("canvases/" + slug)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := os.Stat(p); err == nil {
		writeErr(w, http.StatusConflict, "canvas already exists")
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(p, []byte(`{"nodes": [], "edges": []}`), 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": rel, "name": slug})
}

func (s *Server) listCanvases(w http.ResponseWriter, _ *http.Request) {
	out := []map[string]string{}
	err := filepath.WalkDir(s.Vault.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".canvas") {
			return nil
		}
		if rel, err := s.Vault.RelOf(path); err == nil && !strings.Contains(rel, ".grimoire") {
			out = append(out, map[string]string{
				"path": rel, "name": strings.TrimSuffix(d.Name(), ".canvas")})
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getCanvas(w http.ResponseWriter, r *http.Request) {
	_, p, err := s.canvasPath(r.PathValue("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such canvas")
		return
	}
	doc, err := validateCanvas(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rel, _, _ := s.canvasPath(r.PathValue("path"))
	doc["path"] = rel
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) putCanvas(w http.ResponseWriter, r *http.Request) {
	rel, p, err := s.canvasPath(r.PathValue("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxCanvasBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	doc, err := validateCanvas(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := json.Marshal(doc)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": rel})
}

func (s *Server) deleteCanvas(w http.ResponseWriter, r *http.Request) {
	_, p, err := s.canvasPath(r.PathValue("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
