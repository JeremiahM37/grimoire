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
	// "Authenticated surface" was true of a server with one account. A query
	// block lists notes, so it is a read, and it answers to the same rules as
	// every other read: the results are filtered to what this caller may see.
	if !s.requireUser(w, r) {
		return
	}
	rows := queries.Run(s.Index.DB, block, true)
	writeJSON(w, http.StatusOK, s.readableRows(r, rows))
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
		// Web opts one question into a web pass. Off by default: a context
		// server answers from your notes unless you ask otherwise, and a
		// question that silently reaches the internet is a surprise nobody
		// wants from their own vault.
		Web bool `json:"web"`
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
	// If the whole corpus fits the context budget, read it rather than rank
	// it: decomposition and reranking exist to choose what to leave out, and
	// there is nothing to leave out. See internal/api/context.go.
	hits, mode, err := s.askContext(r, q, k, in.IncludePrivate, smart)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Citing every note is citing nothing. In full mode the CONTEXT is the
	// whole corpus — that is the measured win — but the citations a person
	// clicks, and the ones that make a generated answer checkable, still have
	// to be the passages that bear on the question. So they are always ranked.
	cited := hits
	if mode == "full" {
		if ranked, rerr := s.smartRetrieve(r, q, k, in.IncludePrivate, smart); rerr == nil && len(ranked) > 0 {
			cited = ranked
		}
	}
	citations := []map[string]any{}
	for _, h := range cited {
		citations = append(citations, map[string]any{
			"path": h.Path, "title": h.Title, "score": h.Score})
	}
	contexts := make([]ai.Context, 0, len(hits))
	for _, h := range hits {
		contexts = append(contexts, ai.Context{Path: h.Path, Title: h.Title, Chunk: h.Chunk})
	}
	if in.Web {
		// Web passages are appended and cited like any other, so the answer
		// shows which parts came from outside the vault.
		for _, p := range s.webContext(r, q, 3) {
			contexts = append(contexts, ai.Context{
				Path: p["path"].(string), Title: p["title"].(string), Chunk: p["chunk"].(string)})
			citations = append(citations, map[string]any{
				"path": p["path"], "title": p["title"], "web": true})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"answer": s.AI.Answer(q, contexts), "citations": citations, "mode": mode})
}

// askContext is bestContext with the answering path's smarter retrieval on the
// branch where ranking is actually needed.
func (s *Server) askContext(r *http.Request, q string, k int, includePrivate, smart bool) ([]index.Hit, string, error) {
	// Reading the whole corpus beats ranking it only when something actually
	// READS it. The extractive answer has no reader — it quotes the passages
	// it is handed, in the order it is handed them — so handing it the vault
	// turns "answer" into "dump", and the citation list into a table of
	// contents. The result that motivated this path measured a reader model
	// consuming the context; with no LLM configured, the ranking IS the
	// answer, so the size check does not apply.
	f := filterFor(r, includePrivate)
	if budget := contextBudget(); budget > 0 && s.AI.Available() {
		_, _, chars, err := s.Index.CorpusStatsFor(f)
		if err != nil {
			return nil, "", err
		}
		if chars > 0 && chars <= int64(budget) {
			hits, err := s.Index.WholeCorpusFor(f)
			return hits, "full", err
		}
	}
	hits, err := s.smartRetrieve(r, q, k, includePrivate, smart)
	return hits, "retrieved", err
}

// smartRetrieve is retrieval for ANSWERING, as opposed to /api/retrieve: with
// an LLM available it decomposes a multi-hop question into sub-questions,
// retrieves each, and reranks the merged pool. Without one it is plain
// retrieval, byte for byte — which is what keeps the offline path and the
// benchmark numbers unchanged.
func (s *Server) smartRetrieve(r *http.Request, q string, k int, includePrivate, smart bool) ([]index.Hit, error) {
	f := filterFor(r, includePrivate)
	subs := []string{q}
	if smart {
		subs = s.AI.Decompose(q)
	}
	if len(subs) == 1 {
		return s.Index.RetrieveFor(subs[0], k, f)
	}
	var pool []index.Hit
	seen := map[string]bool{}
	for _, sub := range subs {
		hits, err := s.Index.RetrieveFor(sub, k, f)
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
