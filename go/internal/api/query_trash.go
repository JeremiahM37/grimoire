package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/index"
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
		Smart          *bool  `json:"smart"`
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
		writeJSON(w, http.StatusOK, map[string]any{"answer": "", "citations": []any{}})
		return
	}
	k := in.K
	if k <= 0 {
		k = 6
	}
	smart := in.Smart == nil || *in.Smart
	hits, err := s.smartRetrieve(q, k, in.IncludePrivate, smart)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	citations := []map[string]any{}
	contexts := make([]ai.Context, 0, len(hits))
	for _, h := range hits {
		citations = append(citations, map[string]any{
			"path": h.Path, "title": h.Title, "score": h.Score})
		contexts = append(contexts, ai.Context{Path: h.Path, Title: h.Title, Chunk: h.Chunk})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"answer": s.AI.Answer(q, contexts), "citations": citations})
}

// smartRetrieve is retrieval for ANSWERING, as opposed to /api/retrieve: with
// an LLM available it decomposes a multi-hop question into sub-questions,
// retrieves each, and reranks the merged pool. Without one it is plain
// retrieval, byte for byte — which is what keeps the offline path and the
// benchmark numbers unchanged.
func (s *Server) smartRetrieve(q string, k int, includePrivate, smart bool) ([]index.Hit, error) {
	subs := []string{q}
	if smart {
		subs = s.AI.Decompose(q)
	}
	if len(subs) == 1 {
		return s.Index.Retrieve(subs[0], k, includePrivate)
	}
	var pool []index.Hit
	seen := map[string]bool{}
	for _, sub := range subs {
		hits, err := s.Index.Retrieve(sub, k, includePrivate)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			key := h.Path + "\x00" + firstN(h.Chunk, 80)
			if seen[key] {
				continue
			}
			seen[key] = true
			pool = append(pool, h)
		}
	}
	keep := max(k, 6)
	byKey := map[string]index.Hit{}
	ctxs := make([]ai.Context, 0, len(pool))
	for _, h := range pool {
		c := ai.Context{Path: h.Path, Title: h.Title, Chunk: h.Chunk}
		byKey[c.Path+"\x00"+firstN(c.Chunk, 80)] = h
		ctxs = append(ctxs, c)
	}
	out := make([]index.Hit, 0, keep)
	for _, c := range s.AI.Rerank(q, ctxs, keep) {
		out = append(out, byKey[c.Path+"\x00"+firstN(c.Chunk, 80)])
	}
	return out, nil
}

// firstN truncates to n CHARACTERS — this key must match the Python
// dedupe key (c["chunk"][:80]) or the two pools differ.
func firstN(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// actions are the inline editor AI helpers. Without a configured LLM they fall
// back to deterministic behaviour so the feature still works offline — a dead
// button is worse than a simple answer.
// pyRepr renders a string the way Python's %r does — single quotes — because
// this text reaches the console's error toast, and a client that string-matches
// it should not care which implementation answered.
func pyRepr(s string) string {
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	return strconv.Quote(s)
}

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
		writeJSON(w, http.StatusOK, map[string]any{"result": ai.SuggestTags(text)})
		return
	case "title":
		writeJSON(w, http.StatusOK, map[string]any{"result": ai.FirstLineTitle(text)})
		return
	}
	var prompt string
	switch in.Action {
	case "summarize":
		prompt = "Summarize these notes in 2-3 sentences:\n\n" + text
	case "expand":
		prompt = "Expand these notes into a fuller draft, keeping the meaning:\n\n" + text
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"result": "", "error": fmt.Sprintf("unknown action %s", pyRepr(in.Action))})
		return
	}
	// routed through Answer so the offline fallback is the same extractive
	// path the rest of the product uses — a dead button is worse than a
	// simple answer
	out := s.AI.Answer(prompt, []ai.Context{{Path: "_", Title: "selection", Chunk: text}})
	writeJSON(w, http.StatusOK, map[string]any{"result": out})
}
