package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/memimage"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Visual similarity between stored pictures.
//
// This answers "does memory already hold a picture that looks like this one",
// by comparing pixel fingerprints (memimage.Signature). It needs no caption and
// no model, so an uncaptioned picture is findable here even though recall
// never returns it. It is a measure of how alike the pixels are. It does not
// know what a picture shows, so two photographs of different things that share
// a bright top half will score as near.
//
// The candidate set is exactly the set of pictures the caller may see through
// GET /api/memory/image/{sha} (imageVisible): a picture behind a private or
// sensitive entry is never compared against, never returned, and never
// changes a distance that is returned.

// defaultSimilarMax is the Hamming threshold when none is given. Of 64 bits,
// a value this low admits near-identical pictures (same layout, re-encoded or
// resized) and refuses unrelated ones, which measured about 26 bits apart.
const defaultSimilarMax = 10

const similarMethod = "dhash64 (9x8 luminance grid) + rgb 4x4x4 histogram"

// similarMatch is one visible picture close to the query.
type similarMatch struct {
	SHA           string  `json:"sha"`
	URL           string  `json:"url"`
	HashDistance  int     `json:"hash_distance"`
	ColorDistance float64 `json:"color_distance"`
	// Caption is the text of the first visible entry that holds the picture,
	// and CaptionBasis says how it was made. An uncaptioned picture has neither.
	Caption      string `json:"caption,omitempty"`
	CaptionBasis string `json:"caption_basis,omitempty"`
}

type visibleImage struct {
	text, basis string
}

// similarThreshold reads the optional max=<bits> parameter.
func similarThreshold(r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("max"))
	if raw == "" {
		return defaultSimilarMax, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 64 {
		return 0, false
	}
	return n, true
}

// getSimilarImages answers GET /api/memory/image?similar_to=<sha>[&max=<bits>].
func (s *Server) getSimilarImages(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("similar_to"))
	if raw == "" {
		writeErr(w, http.StatusBadRequest, "similar_to=<sha256> is required")
		return
	}
	sha := strings.ToLower(raw)
	if !memimage.ValidSHA(sha) || !s.imageVisible(r, sha) {
		writeErr(w, http.StatusNotFound, "no such image")
		return
	}
	maxBits, ok := similarThreshold(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "max must be an integer from 0 to 64 (differing bits)")
		return
	}
	q, err := s.imageSignature(sha)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "this picture has no visual signature: "+err.Error())
		return
	}
	s.answerSimilar(w, r, q, sha, maxBits)
}

// postSimilarImage answers POST /api/memory/image/similar. It takes the same
// body as an upload, measures the picture, and stores nothing. It is the
// "is this already in memory" probe, so it never writes a note or a file.
func (s *Server) postSimilarImage(w http.ResponseWriter, r *http.Request) {
	raw, _, maxBytes, ok := readImageUpload(w, r)
	if !ok {
		return
	}
	p, err := memimage.Prepare(raw, maxBytes)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "too large") {
			status = http.StatusRequestEntityTooLarge
		}
		writeErr(w, status, err.Error())
		return
	}
	maxBits, ok := similarThreshold(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "max must be an integer from 0 to 64 (differing bits)")
		return
	}
	q, err := memimage.ComputeSignature(p.Data)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "this picture has no visual signature: "+err.Error())
		return
	}
	s.answerSimilar(w, r, q, "", maxBits)
}

// answerSimilar ranks every visible picture against q. self, when set, is the
// query's own stored address and is left out of the matches.
func (s *Server) answerSimilar(w http.ResponseWriter, r *http.Request, q memimage.Signature, self string, maxBits int) {
	vis, err := s.visibleImages(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cands := make(map[string]memimage.Signature, len(vis))
	for sha := range vis {
		if sha == self {
			continue
		}
		sig, err := s.imageSignature(sha)
		if err != nil {
			// A picture whose bytes are missing or cannot be measured is left
			// out of the comparison rather than failing the whole search.
			continue
		}
		cands[sha] = sig
	}
	ranked := memimage.Rank(q, cands, maxBits)
	matches := make([]similarMatch, 0, len(ranked))
	for _, m := range ranked {
		v := vis[m.SHA]
		sm := similarMatch{
			SHA: m.SHA, URL: imageURL(m.SHA),
			HashDistance:  m.HashDistance,
			ColorDistance: float64(int(m.ColorDistance*1000+0.5)) / 1000,
		}
		if v.basis != memory.CaptionNone && v.text != "" {
			sm.Caption, sm.CaptionBasis = v.text, v.basis
		}
		matches = append(matches, sm)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"method":       similarMethod,
		"max_distance": maxBits,
		"matches":      matches,
		"note":         "visual similarity of pixels only; not recognition of what a picture shows",
	})
}

// visibleImages maps every picture the caller may see to its first visible
// caption. The row filter is imageVisible's, so the two cannot disagree about
// what is visible.
func (s *Server) visibleImages(r *http.Request) (map[string]visibleImage, error) {
	rows, err := s.Index.DB.Query(
		"SELECT note, acl, text, image, capb FROM memory_entries WHERE image<>'' AND private=0 AND visibility=''")
	if err != nil {
		return nil, fmt.Errorf("list pictures: %w", err)
	}
	defer rows.Close()
	out := map[string]visibleImage{}
	for rows.Next() {
		var note, acl, text, sha, basis string
		if err := rows.Scan(&note, &acl, &text, &sha, &basis); err != nil {
			return nil, fmt.Errorf("list pictures: %w", err)
		}
		if !s.canReadNote(r, note, acl) {
			continue
		}
		// Keep the first captioned visible entry; fall back to whatever is
		// visible when none of them has a caption.
		if prev, seen := out[sha]; seen && prev.basis != memory.CaptionNone {
			continue
		}
		out[sha] = visibleImage{text: text, basis: basis}
	}
	return out, rows.Err()
}

// errNoPictureBytes is returned when an indexed picture has no file behind it.
var errNoPictureBytes = errors.New("picture bytes are missing from the vault")

// imageSignature returns the fingerprint of a stored picture. The index keeps
// it, keyed by the content address, so each picture is decoded once; a missing
// row is computed from the file and written back. Nothing here is authority:
// the content address fixes the bytes, so the cached value cannot go stale.
func (s *Server) imageSignature(sha string) (memimage.Signature, error) {
	var dh int64
	var hist []byte
	if err := s.Index.DB.QueryRow("SELECT dhash, hist FROM image_signatures WHERE sha=?", sha).Scan(&dh, &hist); err == nil {
		if sig, err := memimage.SignatureFromStored(dh, hist); err == nil {
			return sig, nil
		}
	}
	_, p, ok := memimage.Find(s.Vault.Root, sha)
	if !ok {
		return memimage.Signature{}, errNoPictureBytes
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return memimage.Signature{}, errNoPictureBytes
	}
	sig, err := memimage.ComputeSignature(data)
	if err != nil {
		return memimage.Signature{}, err
	}
	// A failed cache write only costs a recompute next time.
	_ = s.Index.DB.Exec("INSERT OR REPLACE INTO image_signatures(sha, dhash, hist) VALUES(?, ?, ?)",
		sha, sig.HashInt(), sig.Hist64())
	return sig, nil
}
