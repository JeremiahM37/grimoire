// Package api serves the HTTP surface: the JSON API plus the web console.
//
// Port of server/app.py and server/routers/*. The console in web/ is unchanged
// JavaScript, so this has to match the routes and payload shapes it already
// calls — the existing Playwright suite is the acceptance gate and it does not
// know which implementation is answering.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/JeremiahM37/grimoire/go/internal/build"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/auth"
	"github.com/JeremiahM37/grimoire/go/internal/connectors"
	"github.com/JeremiahM37/grimoire/go/internal/crdtstore"
	"github.com/JeremiahM37/grimoire/go/internal/history"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
	"github.com/JeremiahM37/grimoire/go/internal/settings"
	gsync "github.com/JeremiahM37/grimoire/go/internal/sync"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
	"github.com/JeremiahM37/grimoire/go/internal/websearch"
)

// Server holds everything the handlers need.
type Server struct {
	Index        *index.Index
	Vault        *vault.Vault
	Settings     *settings.Store
	History      *history.Store
	Secrets      *secrets.Vault
	Broker       *secrets.Broker
	CRDT         *crdtstore.Store
	AI           *ai.Client
	Auth         *auth.Store
	Connectors   *connectors.Store
	Runner       *connectors.Runner
	Web          *websearch.Client
	Sync         *gsync.Client
	SyncPeer     string
	SyncToken    string
	SyncInterval int
	WebDir       string
	AuthToken    string
	// AdminToken gates the administrative surface separately from reading, so
	// a deployment can leave notes and retrieval open while closing the levers.
	AdminToken   string
	FrameOptions string
	PluginDir    string
	DailyDir     string
	InboxDir     string

	// snapshot of the space table for the indexer; see SpaceOf.
	spaceMu      sync.Mutex
	spaceAt      time.Time
	spaceEnabled bool
	spaceList    []auth.Space
}

// Routes builds the mux. Specific paths are registered before the catch-all
// note routes: Go's ServeMux prefers the longest pattern, but keeping the
// ordering explicit documents the same hazard the Python side has, where a
// greedy /notes/{path} would otherwise swallow /notes/random.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.health)
	s.authRoutes(mux)
	s.connectorRoutes(mux)
	s.webRoutes(mux)
	s.metricsRoutes(mux)
	mux.HandleFunc("POST /api/reindex", s.adminOnly(s.reindex))
	mux.HandleFunc("GET /api/aliases", s.aliases)
	mux.HandleFunc("GET /api/notes", s.listNotes)
	mux.HandleFunc("POST /api/notes", s.createNote)
	mux.HandleFunc("GET /api/notes/random", s.randomNote)
	mux.HandleFunc("GET /api/notes/{path...}", s.noteGet)
	mux.HandleFunc("POST /api/notes/{path...}", s.notePost)
	mux.HandleFunc("PUT /api/notes/{path...}", s.updateNote)
	mux.HandleFunc("DELETE /api/notes/{path...}", s.deleteNote)
	mux.HandleFunc("GET /api/search", s.search)
	mux.HandleFunc("GET /api/retrieve", s.retrieve)
	mux.HandleFunc("GET /api/context", s.contextEndpoint)
	mux.HandleFunc("GET /api/tags", s.tags)
	mux.HandleFunc("GET /api/templates", s.listTemplates)
	mux.HandleFunc("POST /api/templates/apply", s.applyTemplate)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)
	mux.HandleFunc("GET /api/daily", s.daily)
	mux.HandleFunc("GET /api/daily/dates", s.dailyDates)
	mux.HandleFunc("POST /api/capture", s.capture)
	mux.HandleFunc("GET /api/facts", s.facts)
	mux.HandleFunc("POST /api/tags/rename", s.renameTag)
	mux.HandleFunc("GET /api/graph", s.graph)
	mux.HandleFunc("GET /api/tasks", s.tasks)
	mux.HandleFunc("GET /api/complete", s.complete)
	mux.HandleFunc("POST /api/memory", s.remember)
	mux.HandleFunc("GET /api/memory", s.recall)
	mux.HandleFunc("GET /api/briefing", s.briefing)
	// The credential vault is instance-wide, so managing it is an
	// administrator's job: one shared store of secrets, and a grant issued
	// from it acts with the instance's authority rather than the caller's.
	// Brokering is deliberately NOT admin-gated — the grant token is itself
	// the capability, which is the whole point of handing one to an agent.
	mux.HandleFunc("GET /api/vault/status", s.vaultStatus)
	mux.HandleFunc("POST /api/vault/init", s.adminOnly(s.vaultInit))
	mux.HandleFunc("POST /api/vault/unlock", s.adminOnly(s.vaultUnlock))
	mux.HandleFunc("POST /api/vault/lock", s.adminOnly(s.vaultLock))
	mux.HandleFunc("GET /api/secrets", s.adminOnly(s.listSecrets))
	mux.HandleFunc("POST /api/secrets", s.adminOnly(s.addSecret))
	mux.HandleFunc("DELETE /api/secrets/{name}", s.adminOnly(s.deleteSecret))
	mux.HandleFunc("POST /api/secrets/{name}/grant", s.adminOnly(s.makeGrant))
	mux.HandleFunc("POST /api/secrets/broker", s.brokerUse)
	mux.HandleFunc("GET /api/grants", s.adminOnly(s.listGrants))
	mux.HandleFunc("DELETE /api/grants", s.adminOnly(s.revokeAllGrants))
	mux.HandleFunc("DELETE /api/grants/{token}", s.adminOnly(s.revokeGrant))
	mux.HandleFunc("GET /api/audit", s.adminOnly(s.auditLog))
	mux.HandleFunc("POST /api/attach", s.attach)
	mux.HandleFunc("GET /api/file/{path...}", s.serveFile)
	mux.HandleFunc("GET /api/canvas", s.listCanvases)
	mux.HandleFunc("POST /api/canvas", s.createCanvas)
	mux.HandleFunc("GET /api/canvas/{path...}", s.getCanvas)
	mux.HandleFunc("PUT /api/canvas/{path...}", s.putCanvas)
	mux.HandleFunc("DELETE /api/canvas/{path...}", s.deleteCanvas)
	mux.HandleFunc("GET /api/crdt/doc/{path...}", s.getCRDTDoc)
	mux.HandleFunc("POST /api/crdt/merge", s.mergeCRDT)
	mux.HandleFunc("GET /api/sync/status", s.syncStatus)
	mux.HandleFunc("GET /api/sync/manifest", s.syncManifest)
	mux.HandleFunc("GET /api/export/vault", s.exportVault)
	mux.HandleFunc("POST /api/import/vault", s.importVault)
	mux.HandleFunc("GET /api/plugins", s.listPlugins)
	mux.HandleFunc("POST /api/plugins/scaffold", s.scaffoldPlugin)
	mux.HandleFunc("POST /api/plugins/{name}/enable", s.enablePlugin)
	mux.HandleFunc("GET /plugins/{name}/{rel...}", s.servePluginAsset)
	mux.HandleFunc("POST /api/query", s.runQuery)
	mux.HandleFunc("GET /api/trash", s.listTrash)
	mux.HandleFunc("POST /api/trash/{tid}/restore", s.restoreTrash)
	mux.HandleFunc("DELETE /api/trash/{tid}", s.purgeTrash)
	mux.HandleFunc("POST /api/templates", s.saveTemplate)
	mux.HandleFunc("POST /api/ask", s.ask)
	mux.HandleFunc("POST /api/actions", s.actions)
	mux.HandleFunc("POST /api/sync/now", s.syncNow)
	mux.HandleFunc("POST /api/sync/pull", s.syncPull)
	mux.HandleFunc("POST /api/sync/push", s.syncPush)
	mux.HandleFunc("POST /api/facts", s.setFact)
	mux.HandleFunc("POST /api/memory/consolidate", s.consolidateMemory)
	mux.HandleFunc("POST /api/audio", s.audioMemo)
	mux.HandleFunc("POST /api/vault/change-passphrase", s.changePassphrase)
	mux.HandleFunc("GET /notes/{path...}", s.noteGet)
	mux.HandleFunc("GET /read", s.readIndex)
	mux.HandleFunc("GET /read/{path...}", s.readNote)

	if s.WebDir != "" {
		mux.Handle("/", s.staticHandler())
	}
	// Order matters: bodies are capped before anything reads them, the rate
	// limit is applied before work is done, and the principal is resolved
	// before a handler can ask who is calling.
	return securityHeaders(s.FrameOptions,
		instrument(limitBodies(s.throttle(s.requireAuth(
			s.withPrincipal(s.requireAdminToken(mux)))))))
}

// securityHeaders applies the same defence-in-depth headers as the Python app.
// The strict CSP is why first-party plugins work while remote scripts don't:
// everything is served same-origin.
func securityHeaders(frameOptions string, next http.Handler) http.Handler {
	const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob:; font-src 'self'; connect-src 'self'; " +
		"base-uri 'none'; form-action 'self'"
	// SECURITY.md documents both of these; neither was actually being sent.
	// Framing control matters here because the console is same-origin with the
	// secrets routes, so a clickjacked frame acts with the user's session.
	if frameOptions == "" {
		frameOptions = "SAMEORIGIN"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", frameOptions)
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) staticHandler() http.Handler {
	fs := http.FileServer(http.Dir(s.WebDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Without an explicit directive browsers fall back to heuristic caching
		// (~10% of the file's age), so a shell asset that had sat unchanged for
		// days kept being served from disk cache for hours after a deploy — the
		// console would keep running stale CSS/JS. "no-cache" means revalidate,
		// not "don't cache": ServeFile still answers 304 from Last-Modified.
		w.Header().Set("Cache-Control", "no-cache")
		// serve index.html for the app shell, files otherwise
		if r.URL.Path == "/" {
			http.ServeFile(w, r, filepath.Join(s.WebDir, "index.html"))
			return
		}
		fs.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// the status line is already sent; log-and-continue is all that's left
		fmt.Fprintf(os.Stderr, "encoding response: %v\n", err)
	}
}

func writeErr(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}

// normPath applies the same normalization the Python side does: strip a leading
// slash and ensure the .md suffix.
func normPath(p string) string {
	p = strings.TrimPrefix(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	if !strings.HasSuffix(p, ".md") {
		p += ".md"
	}
	return p
}

type noteView struct {
	Path        string         `json:"path"`
	Title       string         `json:"title"`
	Body        string         `json:"body"`
	Private     bool           `json:"private"`
	MTime       float64        `json:"mtime"`
	Hash        string         `json:"hash"`
	Created     string         `json:"created"`
	Updated     string         `json:"updated"`
	Frontmatter map[string]any `json:"frontmatter"`
	Tags        []string       `json:"tags"`
	Encrypted   bool           `json:"encrypted"`
	Locked      bool           `json:"locked"`
}

// viewOf presents a note for the console. An encrypted note is decrypted when
// the vault is unlocked and BLANKED with locked=true when it is not — the UI
// must never receive ciphertext, both because it is useless and because a
// locked vault should not hand out the sealed bytes.
func (s *Server) viewOf(n *vault.Note) noteView {
	v := viewOf(n)
	if !n.Encrypted {
		return v
	}
	if s.Secrets != nil && s.Secrets.IsUnlocked() {
		if plain, err := s.Secrets.UnsealText(n.Body); err == nil {
			v.Body = plain
			v.Locked = false
			return v
		}
	}
	v.Body = ""
	v.Locked = true
	return v
}

func viewOf(n *vault.Note) noteView {
	fm := map[string]any{}
	for _, k := range n.Frontmatter.Keys() {
		v, _ := n.Frontmatter.Get(k)
		fm[k] = v
	}
	tags := n.Tags
	if tags == nil {
		tags = []string{}
	}
	return noteView{
		Path: n.Path, Title: n.Title, Body: n.Body, Private: n.Private,
		MTime: n.MTime, Hash: n.Hash,
		Created:     n.Frontmatter.StringVal("created"),
		Updated:     n.Frontmatter.StringVal("updated"),
		Frontmatter: fm, Tags: tags, Encrypted: n.Encrypted, Locked: false,
	}
}

// ---------------------------------------------------------------- handlers

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	notes, _ := s.Index.DB.Count("SELECT COUNT(*) FROM notes")
	tags, _ := s.Index.DB.Count("SELECT COUNT(DISTINCT tag) FROM tags")
	unresolved, _ := s.Index.DB.Count("SELECT COUNT(*) FROM links WHERE resolved=0")
	var latest string
	_ = s.Index.DB.QueryRow("SELECT COALESCE(MAX(updated),'') FROM notes").Scan(&latest)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"version":          build.String(),
		"vault":            s.Vault.Root,
		"notes":            notes,
		"tags":             tags,
		"unresolved_links": unresolved,
		// which embedding backend the semantic leg actually got. The chain
		// falls back silently by design — a missing model must degrade
		// retrieval, not prevent startup — so without this an operator cannot
		// tell a semantic index from the hashing floor, and neither can a
		// test that means to gate the shipped configuration.
		"embedder": s.Index.Emb.Signature(),
		// cheap change signature the open console polls to notice edits made
		// outside it (device sync, MCP, another editor)
		"rev": fmt.Sprintf("%d:%s", notes, latest),
	})
}

func (s *Server) reindex(w http.ResponseWriter, _ *http.Request) {
	n, err := s.Index.Reindex()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"indexed": n})
}

func (s *Server) aliases(w http.ResponseWriter, _ *http.Request) {
	m, err := s.Index.AliasMap()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

type listItem struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Updated string `json:"updated"`
	Private bool   `json:"private"`
	Pinned  bool   `json:"pinned"`
}

func (s *Server) listNotes(w http.ResponseWriter, r *http.Request) {
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	tag := r.URL.Query().Get("tag")

	// Restricted in SQL rather than after the fact: filtering the result of a
	// LIMIT would silently return fewer notes than asked for, and the shortfall
	// would be exactly the notes the caller cannot see.
	where, spaceArgs := s.whereSpace(r, "space", "")
	query := "SELECT path, title, updated, private, frontmatter_json, acl FROM notes" +
		where + " ORDER BY updated DESC, path LIMIT ?"
	args := append(append([]any{}, spaceArgs...), limit)
	if tag != "" {
		nWhere, nSpaceArgs := s.whereSpace(r, "n.space", " WHERE t.tag=?")
		query = "SELECT n.path, n.title, n.updated, n.private, n.frontmatter_json, n.acl FROM notes n " +
			"JOIN tags t ON t.note=n.path" + nWhere + " ORDER BY n.updated DESC, n.path LIMIT ?"
		args = append(append([]any{tag}, nSpaceArgs...), limit)
	}
	rows, err := s.Index.DB.Query(query, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	items := []listItem{}
	for rows.Next() {
		var it listItem
		var private int
		var fmJSON, acl string
		if err := rows.Scan(&it.Path, &it.Title, &it.Updated, &private, &fmJSON, &acl); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// The reader list is selected with the row rather than looked up: a
		// query inside an open cursor waits for the connection the cursor
		// holds, which on one connection is a deadlock.
		if !s.canReadNote(r, it.Path, acl) {
			continue
		}
		it.Private = private != 0
		it.Pinned = pinnedFlag(fmJSON)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// pinned notes float to the top, stable within each group
	sort.SliceStable(items, func(i, j int) bool { return items[i].Pinned && !items[j].Pinned })
	writeJSON(w, http.StatusOK, items)
}

func pinnedFlag(fmJSON string) bool {
	var m map[string]any
	if json.Unmarshal([]byte(fmJSON), &m) != nil {
		return false
	}
	switch v := m["pinned"].(type) {
	case bool:
		return v
	case string:
		return v != ""
	}
	return false
}

type noteIn struct {
	Path        string         `json:"path"`
	Title       string         `json:"title"`
	Body        string         `json:"body"`
	Tags        []string       `json:"tags"`
	Frontmatter map[string]any `json:"frontmatter"`
}

func (s *Server) createNote(w http.ResponseWriter, r *http.Request) {
	var in noteIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.Path == "" && in.Title == "" {
		writeErr(w, http.StatusBadRequest, "path or title required")
		return
	}
	var rel string
	if in.Path != "" {
		rel = normPath(in.Path)
		p, err := s.Vault.SafePath(rel)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := os.Stat(p); err == nil {
			// an explicit path collision is an error the caller should see
			writeErr(w, http.StatusConflict, "note already exists")
			return
		}
	} else {
		// a title-derived slug collision auto-suffixes (Meeting → meeting-2)
		var err error
		rel, err = s.uniquePath(vault.Slugify(in.Title) + ".md")
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if !s.requireWrite(w, r, rel) {
		return
	}

	fm := markdown.NewFrontmatter()
	for k, v := range in.Frontmatter {
		fm.Set(k, v)
	}
	if in.Title != "" {
		if _, ok := fm.Get("title"); !ok {
			fm.Set("title", in.Title)
		}
	}
	if len(in.Tags) > 0 {
		vals := make([]markdown.Value, len(in.Tags))
		for i, t := range in.Tags {
			vals[i] = t
		}
		fm.Set("tags", vals)
	}
	if _, err := s.Vault.Write(rel, in.Body, fm); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	note, err := s.Index.Upsert(rel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.viewOf(note))
}

func (s *Server) uniquePath(base string) (string, error) {
	rel := base
	for i := 2; ; i++ {
		p, err := s.Vault.SafePath(rel)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return rel, nil
		}
		rel = fmt.Sprintf("%s-%d.md", strings.TrimSuffix(base, ".md"), i)
		if i > 9999 {
			return "", errors.New("could not find a free path")
		}
	}
}

func (s *Server) getNote(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	note, err := s.Vault.Read(rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such note")
		return
	}
	out := map[string]any{}
	b, _ := json.Marshal(s.viewOf(note))
	_ = json.Unmarshal(b, &out)

	backlinks, err := s.backlinks(rel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out["backlinks"] = backlinks
	links, err := s.outgoingLinks(rel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out["links"] = links
	writeJSON(w, http.StatusOK, out)
}

// outgoingLinks returns this note's links WITH their resolution state, which is
// what distinguishes a working link from a dangling one in the console.
func (s *Server) outgoingLinks(rel string) ([]map[string]any, error) {
	rows, err := s.Index.DB.Query(
		"SELECT target, COALESCE(dst,''), alias, resolved FROM links WHERE src=? ORDER BY rowid", rel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var target, dst, alias string
		var resolved int
		if err := rows.Scan(&target, &dst, &alias, &resolved); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"target": target, "dst": dst, "alias": alias, "resolved": resolved,
		})
	}
	return out, rows.Err()
}

func (s *Server) backlinks(rel string) ([]map[string]any, error) {
	// DISTINCT + a deterministic order: a note may link to another more than
	// once, and ordering by title alone leaves ties unspecified, so the
	// rendered "Linked from" line could reorder between identical requests.
	rows, err := s.Index.DB.Query(
		"SELECT DISTINCT l.src, n.title, l.alias FROM links l "+
			"JOIN notes n ON n.path=l.src WHERE l.dst=? ORDER BY n.title, l.src", rel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var src, title, alias string
		if err := rows.Scan(&src, &title, &alias); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"path": src, "title": title, "alias": alias})
	}
	return out, rows.Err()
}

type noteUpdate struct {
	Body        string          `json:"body"`
	Frontmatter *map[string]any `json:"frontmatter"`
}

func (s *Server) updateNote(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	if !s.requireWrite(w, r, rel) {
		return
	}
	var u noteUpdate
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	existing, readErr := s.Vault.Read(rel)
	if readErr == nil && strings.TrimRight(existing.Body, "\n") != strings.TrimRight(u.Body, "\n") {
		// compare modulo the trailing newline the serializer guarantees, or
		// every save of unchanged content would count as a new version
		s.History.Snapshot(rel, existing.Body)
	}

	fm := markdown.NewFrontmatter()
	switch {
	case u.Frontmatter != nil:
		for k, v := range *u.Frontmatter {
			fm.Set(k, v)
		}
	case readErr == nil:
		fm = existing.Frontmatter.Clone()
	}
	if _, err := s.Vault.Write(rel, u.Body, fm); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	note, err := s.Index.Upsert(rel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.viewOf(note))
}

// deleteNote moves the note to the trash rather than unlinking it, so the
// console's delete is undoable. Permanent removal is an explicit purge.
func (s *Server) deleteNote(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	if !s.requireWrite(w, r, rel) {
		return
	}
	title := rel
	if note, err := s.Vault.Read(rel); err == nil {
		title = note.Title
	}
	tid, err := s.TrashNote(rel, title)
	if err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	if err := s.Index.Remove(rel); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.CRDT != nil {
		_ = s.CRDT.DeleteDoc(rel)
	}
	writeJSON(w, http.StatusOK, map[string]any{"trashed": tid, "path": rel})
}

func (s *Server) randomNote(w http.ResponseWriter, r *http.Request) {
	randWhere, randArgs := s.whereSpace(r, "space", " WHERE acl=''")
	var path string
	err := s.Index.DB.QueryRow(
		"SELECT path FROM notes"+randWhere+" ORDER BY RANDOM() LIMIT 1", randArgs...).Scan(&path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no notes")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path})
}

func (s *Server) retrieve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	k := 8
	if v := r.URL.Query().Get("k"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			k = n
		}
	}
	hits, err := s.Index.RetrieveFor(q, k, filterFor(r, false))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hits == nil {
		hits = []index.Hit{}
	}
	writeJSON(w, http.StatusOK, hits)
}

func (s *Server) tags(w http.ResponseWriter, r *http.Request) {
	// Tag counts are computed over readable notes only: a count that includes
	// notes the caller cannot open tells them those notes exist.
	where, args := s.whereSpace(r, "n.space", "")
	rows, err := s.Index.DB.Query(
		"SELECT t.tag, COUNT(*) c FROM tags t JOIN notes n ON n.path=t.note"+where+
			" GROUP BY t.tag ORDER BY c DESC, t.tag", args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var tag string
		var c int
		if err := rows.Scan(&tag, &c); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// field name is "c", the raw SQL alias — the console reads that, and
		// renaming it here would silently empty the tag list
		out = append(out, map[string]any{"tag": tag, "c": c})
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// linkMap resolves wiki-link targets for the public read surface. Titles,
// full paths and bare stems all resolve, so [[Folder/Note]] works.
func (s *Server) linkMap() (map[string]string, error) {
	rows, err := s.Index.DB.Query("SELECT path, title FROM notes WHERE private=0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, title string
		if err := rows.Scan(&path, &title); err != nil {
			return nil, err
		}
		out[strings.ToLower(title)] = path
		out[strings.ToLower(path)] = path
		out[strings.ToLower(strings.TrimSuffix(path, ".md"))] = path
		stem := path
		if i := strings.LastIndex(stem, "/"); i >= 0 {
			stem = stem[i+1:]
		}
		out[strings.ToLower(strings.TrimSuffix(stem, ".md"))] = path
	}
	return out, rows.Err()
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return strings.ReplaceAll(s, "'", "&#x27;")
}

func statusForVaultErr(err error) int {
	if errors.Is(err, vault.ErrVault) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}
