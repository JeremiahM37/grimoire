package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/memimage"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Image memory. A memory bullet can point at an image: its text is the caption
// and `img=<sha256>` names the bytes, which live in the vault attachment store
// (memimage.Dir). Retrieval is by caption only: the caption is the bullet's
// text, and the existing full-text and vector arms index that text. No pixels
// are embedded anywhere.

const (
	uncaptionedText = "image (uncaptioned)"
	maxCaptionChars = 500
	// imageMaxEnv overrides the per-image size cap in bytes.
	imageMaxEnv = "GRIMOIRE_MEMORY_IMAGE_MAX_BYTES"
)

// imageRef is the image part of a recall result.
type imageRef struct {
	SHA   string `json:"sha"`
	MIME  string `json:"mime,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
	URL   string `json:"url"`
}

func imageURL(sha string) string { return "/api/memory/image/" + sha }

// imageMaxBytes reads the size cap. A malformed or non-positive value falls
// back to the default rather than disabling the cap.
func imageMaxBytes() int64 {
	if v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(imageMaxEnv)), 10, 64); err == nil && v > 0 {
		return v
	}
	return memimage.DefaultMaxBytes
}

// imageRequest is the JSON form of a memory image upload. The multipart form
// carries the same fields, with the bytes in a "file" part.
type imageRequest struct {
	DataBase64 string `json:"data_base64"`
	Caption    string `json:"caption"`
	Topic      string `json:"topic"`
	Agent      string `json:"agent"`
}

// postImage stores an image and writes the memory bullet that refers to it.
//
// The bullet is written directly rather than through reconcileFact: a caption
// is a description of a picture, and two pictures of similar things must not
// supersede each other the way two competing facts do.
func (s *Server) postImage(w http.ResponseWriter, r *http.Request) {
	maxBytes := imageMaxBytes()
	// base64 inflates by 4/3; the slack covers the JSON envelope.
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes*2+64<<10)

	var raw []byte
	var req imageRequest
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(maxBytes + 64<<10); err != nil {
			writeErr(w, http.StatusBadRequest, "expected a multipart upload with a file field, or JSON with data_base64")
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			writeErr(w, http.StatusBadRequest, "missing file field")
			return
		}
		defer file.Close()
		raw, err = io.ReadAll(io.LimitReader(file, maxBytes+1))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Caption = r.FormValue("caption")
		req.Topic = r.FormValue("topic")
		req.Agent = r.FormValue("agent")
	default:
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json")
			return
		}
		if int64(len(req.DataBase64)) > maxBytes*4/3+4 {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("image too large: the limit is %d bytes", maxBytes))
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(req.DataBase64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "data_base64 is not valid base64")
			return
		}
		raw = decoded
	}
	s.prepared(w, r, req, raw, maxBytes)
}

// prepared is the second half of postImage, shared by both body encodings.
func (s *Server) prepared(w http.ResponseWriter, r *http.Request, req imageRequest, raw []byte, maxBytes int64) {
	p, err := memimage.Prepare(raw, maxBytes)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "too large") {
			status = http.StatusRequestEntityTooLarge
		}
		writeErr(w, status, err.Error())
		return
	}

	caption := strings.TrimSpace(req.Caption)
	if len([]rune(caption)) > maxCaptionChars {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("caption must be at most %d characters", maxCaptionChars))
		return
	}
	basis := memory.CaptionStated
	if caption == "" {
		// No caption and no model to write one: the bytes are kept, but the
		// bullet is flagged so recall never presents the placeholder as a fact.
		caption, basis = uncaptionedText, memory.CaptionNone
	}

	agent, ok := writerAgent(r, req.Agent)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid agent name")
		return
	}
	rel := normPath(s.memoryRel(req.Topic))
	if !s.requireWrite(w, r, rel) {
		return
	}
	if err := memimage.Store(s.Vault.Root, p); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	m := memoryIn{Topic: req.Topic, imageSHA: p.SHA, capBasis: basis}
	id, err := s.appendEntry(w, r, rel, caption, agent, "", m, "", "")
	if err != nil {
		// appendEntry has answered. Do not leave the bytes behind if nothing
		// refers to them.
		s.gcImage(p.SHA)
		return
	}
	mime, size := p.Format.MIME, int64(len(p.Data))
	writeJSON(w, http.StatusCreated, map[string]any{
		"path": rel, "id": id, "text": caption, "caption_basis": basis,
		"searchable": basis != memory.CaptionNone,
		"image":      imageRef{SHA: p.SHA, MIME: mime, Bytes: size, URL: imageURL(p.SHA)},
		"stripped":   p.Stripped,
	})
}

// writerAgent names the author of a write the way rememberOne does: a verified
// network identity first, then the claimed header, then "agent".
func writerAgent(r *http.Request, bodyAgent string) (string, bool) {
	agent := strings.TrimSpace(bodyAgent)
	if v, ok := verifiedAgent(r); ok && agentRE.MatchString(v) {
		agent = v
	}
	if agent == "" {
		agent = claimedAgent(r)
	}
	if agent == "" {
		agent = "agent"
	}
	return agent, agentRE.MatchString(agent)
}

// imageVisible reports whether the caller may see the image: some non-private
// memory entry that the caller may read references it. Hash alone is not a
// capability; an image nobody visible refers to answers as absent.
func (s *Server) imageVisible(r *http.Request, sha string) bool {
	rows, err := s.Index.DB.Query(
		"SELECT note, acl FROM memory_entries WHERE image=? AND private=0 AND visibility=''", sha)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var note, acl string
		if rows.Scan(&note, &acl) == nil && s.canReadNote(r, note, acl) {
			return true
		}
	}
	return false
}

// getImage serves a stored image to a caller who can see a bullet that uses it.
func (s *Server) getImage(w http.ResponseWriter, r *http.Request) {
	sha := strings.ToLower(r.PathValue("sha"))
	if !memimage.ValidSHA(sha) || !s.imageVisible(r, sha) {
		writeErr(w, http.StatusNotFound, "no such image")
		return
	}
	f, p, ok := memimage.Find(s.Vault.Root, sha)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such image")
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such image")
		return
	}
	w.Header().Set("Content-Type", f.MIME)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	// Content-addressed, so the bytes behind a URL never change.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	_, _ = w.Write(data)
}

// fillImageRefs adds mime and size to recall results that carry an image. A
// reference whose bytes are missing keeps its sha and url and reports nothing
// else, which is the honest answer.
func (s *Server) fillImageRefs(out []entryOut) {
	for i := range out {
		if out[i].Image == nil {
			continue
		}
		if f, p, ok := memimage.Find(s.Vault.Root, out[i].Image.SHA); ok {
			out[i].Image.MIME = f.MIME
			if info, err := os.Stat(p); err == nil {
				out[i].Image.Bytes = info.Size()
			}
		}
	}
}

// gcImage deletes a stored image once no memory bullet refers to it. It is the
// only place bytes are removed, so a soft forget (which strikes a bullet
// through but keeps it) keeps its picture, and a hard forget of the last
// referencing bullet takes the picture with it.
func (s *Server) gcImage(sha string) {
	if sha == "" {
		return
	}
	var n int
	if err := s.Index.DB.QueryRow("SELECT COUNT(*) FROM memory_entries WHERE image=?", sha).Scan(&n); err != nil {
		return // unknown is not zero: keep the bytes
	}
	if n == 0 {
		_ = memimage.Remove(s.Vault.Root, sha)
	}
}

// imageOfLine returns the image a removed bullet referred to, so removeEntry can
// collect it once the index no longer lists the bullet.
func imageOfLine(line string) string {
	if e, ok := memory.ParseLine(line); ok {
		return e.Image
	}
	return ""
}
