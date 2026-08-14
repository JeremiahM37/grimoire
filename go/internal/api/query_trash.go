package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/queries"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Query blocks, trash (undoable delete), templates and extractive ask.

func (s *Server) runQuery(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Block          string `json:"block"`
		Query          string `json:"query"`
		IncludePrivate bool   `json:"include_private"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	block := in.Block
	if block == "" {
		block = in.Query
	}
	// authenticated surface, so private notes are visible here; /read and the
	// public renderers pass includePrivate=false instead
	writeJSON(w, http.StatusOK, queries.Run(s.Index.DB, block, true))
}

// ---------------------------------------------------------------- trash

// Deleting a note moves it to .grimoire/trash with a manifest entry, so the
// console's delete is undoable. Purge is the only irreversible path.

type trashEntry struct {
	ID        string `json:"id"`
	Original  string `json:"original"`
	Title     string `json:"title"`
	DeletedAt string `json:"deleted_at"`
}

func (s *Server) trashDir() string {
	return filepath.Join(s.Vault.Root, ".grimoire", "trash")
}

func (s *Server) trashManifestPath() string {
	return filepath.Join(s.trashDir(), "manifest.json")
}

func (s *Server) loadTrash() map[string]trashEntry {
	out := map[string]trashEntry{}
	raw, err := os.ReadFile(s.trashManifestPath())
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func (s *Server) saveTrash(m map[string]trashEntry) error {
	if err := os.MkdirAll(s.trashDir(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.trashManifestPath(), raw, 0o644)
}

// TrashNote moves a note file into the trash and returns its trash id.
func (s *Server) TrashNote(rel, title string) (string, error) {
	src, err := s.Vault.SafePath(rel)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(src); err != nil {
		return "", fmt.Errorf("%w: no such note: %s", vault.ErrVault, rel)
	}
	m := s.loadTrash()
	base := vault.Now().Format("20060102-150405")
	tid := base
	for i := 1; ; i++ {
		if _, taken := m[tid]; !taken {
			break
		}
		tid = fmt.Sprintf("%s-%d", base, i)
	}
	if err := os.MkdirAll(s.trashDir(), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(src, filepath.Join(s.trashDir(), tid+".md")); err != nil {
		return "", err
	}
	m[tid] = trashEntry{
		ID: tid, Original: rel, Title: title,
		DeletedAt: vault.Now().Format(vault.TimeFormat),
	}
	return tid, s.saveTrash(m)
}

func (s *Server) listTrash(w http.ResponseWriter, _ *http.Request) {
	m := s.loadTrash()
	out := make([]trashEntry, 0, len(m))
	for id, e := range m {
		e.ID = id
		out = append(out, e)
	}
	// newest first, matching the console's expectation
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) restoreTrash(w http.ResponseWriter, r *http.Request) {
	tid := r.PathValue("tid")
	m := s.loadTrash()
	entry, ok := m[tid]
	if !ok {
		writeErr(w, http.StatusNotFound, "no such trashed note")
		return
	}
	// restoring must never clobber a note created since the delete
	destRel, err := s.uniquePath(entry.Original)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	dest, err := s.Vault.SafePath(destRel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.Rename(filepath.Join(s.trashDir(), tid+".md"), dest); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	delete(m, tid)
	if err := s.saveTrash(m); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	note, err := s.Index.Upsert(destRel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.viewOf(note))
}

func (s *Server) purgeTrash(w http.ResponseWriter, r *http.Request) {
	tid := r.PathValue("tid")
	m := s.loadTrash()
	if _, ok := m[tid]; ok {
		_ = os.Remove(filepath.Join(s.trashDir(), tid+".md"))
		delete(m, tid)
		if err := s.saveTrash(m); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- templates

func (s *Server) saveTemplate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	rel := "templates/" + vault.Slugify(name) + ".md"
	fm := markdown.NewFrontmatter()
	fm.Set("title", name)
	if _, err := s.Vault.Write(rel, in.Body, fm); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	// templates live in a reserved dir: they are deliberately NOT indexed as
	// notes, so no Upsert here
	writeJSON(w, http.StatusCreated, map[string]string{"path": rel, "name": name})
}

// ---------------------------------------------------------------- ask

// ask answers from the vault. Without a configured LLM the answer is
// EXTRACTIVE — the best-matching passages verbatim, with citations. That is a
// deliberate floor: a wrong generated answer about your own notes is worse than
// no answer, and citations are what make it checkable.
func (s *Server) ask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Q              string `json:"q"`
		Question       string `json:"question"`
		K              int    `json:"k"`
		IncludePrivate bool   `json:"include_private"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	q := in.Q
	if q == "" {
		q = in.Question
	}
	if strings.TrimSpace(q) == "" {
		writeErr(w, http.StatusBadRequest, "q required")
		return
	}
	k := in.K
	if k <= 0 {
		k = 6
	}
	hits, err := s.Index.Retrieve(q, k, in.IncludePrivate)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	citations := []map[string]any{}
	var parts []string
	for _, h := range hits {
		citations = append(citations, map[string]any{
			"path": h.Path, "title": h.Title, "chunk": h.Chunk, "score": h.Score,
		})
		parts = append(parts, h.Chunk)
	}
	answer := "No matching notes."
	if len(parts) > 0 {
		answer = strings.Join(parts, "\n\n---\n\n")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"question": q, "answer": answer, "citations": citations,
		"backend": "extractive",
	})
}

// actions are the inline editor AI helpers. Without a configured LLM they fall
// back to deterministic behaviour so the feature still works offline — a dead
// button is worse than a simple answer.
func (s *Server) actions(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string `json:"action"`
		Text   string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	text := strings.TrimSpace(in.Text)
	switch in.Action {
	case "tags":
		writeJSON(w, http.StatusOK, map[string]any{"result": suggestTags(text)})
	case "title":
		writeJSON(w, http.StatusOK, map[string]any{"result": firstLineTitle(text)})
	case "summarize":
		writeJSON(w, http.StatusOK, map[string]any{"result": firstSentences(text, 3)})
	case "expand":
		// no generative backend: return the selection unchanged rather than
		// inventing content about the user's own notes
		writeJSON(w, http.StatusOK, map[string]any{"result": text})
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"result": "", "error": fmt.Sprintf("unknown action %q", in.Action)})
	}
}

var wordRE = regexp.MustCompile(`[\p{L}\p{N}][\p{L}\p{N}'-]{2,}`)

// stopWords keeps the suggestions to words that actually characterise the text.
var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for that with this from have will your you not
are was were but they their there then than when what which who how why can could should would
into out about over under more most some any all one two get got has had its it's about`) {
		stopWords[w] = true
	}
}

func suggestTags(text string) []string {
	counts := map[string]int{}
	for _, w := range wordRE.FindAllString(strings.ToLower(text), -1) {
		if stopWords[w] || len(w) < 4 {
			continue
		}
		counts[w]++
	}
	type kv struct {
		w string
		n int
	}
	var list []kv
	for w, n := range counts {
		list = append(list, kv{w, n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].w < list[j].w
	})
	out := []string{}
	for i, kv := range list {
		if i >= 5 {
			break
		}
		out = append(out, kv.w)
	}
	return out
}

func firstLineTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "# "))
		if line != "" {
			if len(line) > 80 {
				line = line[:80]
			}
			return line
		}
	}
	return "Untitled"
}

func firstSentences(text string, n int) string {
	parts := regexp.MustCompile(`(?:[.!?])\s+`).Split(strings.TrimSpace(text), -1)
	if len(parts) > n {
		parts = parts[:n]
	}
	out := strings.Join(parts, ". ")
	if out != "" && !strings.HasSuffix(out, ".") {
		out += "."
	}
	return out
}

// syncNow is a no-op without a configured peer; it reports that rather than
// failing, so the console's button is honest instead of broken.
func (s *Server) syncNow(w http.ResponseWriter, _ *http.Request) {
	if s.SyncPeer == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"synced": false, "reason": "no sync peer configured"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"synced": false, "peer": s.SyncPeer})
}
