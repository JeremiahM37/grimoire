package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// setFact writes a `key:: value` fact into a note. Markdown stays the source of
// truth: the existing line for that key is updated in place, else the fact is
// appended — so a fact set through the API is the same text a human would type.
func (s *Server) setFact(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Note  string `json:"note"`
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	rel := strings.TrimSpace(in.Note)
	key := strings.TrimSpace(in.Key)
	val := strings.TrimSpace(in.Value)
	if rel == "" || key == "" || val == "" {
		writeErr(w, http.StatusBadRequest, "note, key and value are required")
		return
	}
	note, err := s.Vault.Read(rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such note")
		return
	}
	if note.Encrypted {
		writeErr(w, http.StatusBadRequest, "cannot set a fact on an encrypted note")
		return
	}
	// (?im) matches Python's IGNORECASE|MULTILINE; the capture preserves the
	// line's leading indent and any list bullet
	pat, err := regexp.Compile(`(?im)^(\s*(?:[-*]\s+)?)` + regexp.QuoteMeta(key) + `\s*::\s+.*$`)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid key")
		return
	}
	body := note.Body
	if loc := pat.FindStringSubmatchIndex(body); loc != nil {
		prefix := body[loc[2]:loc[3]]
		body = body[:loc[0]] + prefix + key + ":: " + val + body[loc[1]:]
	} else {
		body = strings.TrimRight(body, " \t\r\n") + "\n\n" + key + ":: " + val + "\n"
	}
	if _, err := s.Vault.Write(rel, body, note.Frontmatter); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := s.Index.Upsert(rel); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"note": rel, "key": strings.ToLower(key), "value": val})
}

// consolidateMemory compacts agent memory so recall stays sharp as it grows:
// merge redundant entries, supersede stale ones. Each rewrite is snapshotted
// first, so the human reviews and rolls back like any note — memory stays
// auditable rather than being quietly rewritten by a model.
func (s *Server) consolidateMemory(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path  string `json:"path"`
		Topic string `json:"topic"`
	}
	// an empty body means "all memory notes", so a decode failure is not fatal
	_ = json.NewDecoder(r.Body).Decode(&in)

	var rels []string
	switch {
	case strings.TrimSpace(in.Path) != "":
		rels = []string{strings.TrimSpace(in.Path)}
	case strings.TrimSpace(in.Topic) != "":
		rels = []string{s.memoryRel(in.Topic)}
	default:
		rows, err := s.Index.DB.Query(
			"SELECT path FROM notes WHERE path LIKE ? ORDER BY updated DESC",
			MemoryDir+"/%")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				rels = append(rels, p)
			}
		}
		if err := rows.Err(); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	out := []map[string]any{}
	for _, rel := range rels {
		note, err := s.Vault.Read(rel)
		if err != nil || note.Encrypted {
			continue
		}
		before := note.Body
		after := s.AI.ConsolidateMemory(before)
		if after == "" || strings.TrimSpace(after) == strings.TrimSpace(before) {
			continue
		}
		s.History.Snapshot(rel, before) // rollback-able
		if _, err := s.Vault.Write(rel, after, note.Frontmatter); err != nil {
			continue
		}
		if _, err := s.Index.Upsert(rel); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"path":           rel,
			"before_entries": strings.Count(before, "- **"),
			"after_entries":  strings.Count(after, "- **"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"consolidated": out, "notes_changed": len(out)})
}

// audioMemo is record → transcribe → note. The audio itself lands in the vault
// (so it syncs and backs up with everything else) and the note carries the
// transcript plus a link to the recording. Without a transcription service the
// note is still created — losing the recording because a service is missing
// would be the worst outcome.
func (s *Server) audioMemo(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "expected a multipart upload")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "file field required")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	filename := "memo.webm"
	if header != nil && header.Filename != "" {
		filename = header.Filename
	}
	ext := "webm"
	if i := strings.LastIndex(filename, "."); i >= 0 && i < len(filename)-1 {
		if e := filename[i+1:]; e != "" {
			ext = e
			if len(ext) > 8 {
				ext = ext[:8]
			}
		}
	}

	stamp := vault.Now().Format("20060102-150405")
	audioRel := fmt.Sprintf("%s/%s.%s", AttachDir, stamp, ext)
	apath, err := s.Vault.SafeRawPath(audioRel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Dir(apath), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(apath, data, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	transcript := s.AI.Transcribe(data, filename)
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = "Audio memo " + stamp
	}
	rel := fmt.Sprintf("%s/%s-audio.md", s.InboxDir, stamp)
	fm := markdown.NewFrontmatter()
	fm.Set("title", title)
	fm.Set("tags", []markdown.Value{"audio", "capture"})
	body := fmt.Sprintf("🎙 [audio](../%s)\n\n%s", audioRel, transcript)
	if _, err := s.Vault.Write(rel, body, fm); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := s.Index.Upsert(rel); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"path": rel, "audio": audioRel, "transcript": transcript})
}
