package api

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memstore"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// The shared memory store: kinds on items, the core plus pointers an agent
// loads at session start, and the light verification of procedures. The
// logic lives in internal/memstore; this file is the server's side of it.
// See docs/MEMORY_STORE.md.

// canonicalMemoryDir is the directory every agent's memory is linked to, from
// the memory_canonical_dir setting (falling back to dream_canonical_memory,
// which names the same thing for the fragmentation check).
func (s *Server) canonicalMemoryDir() string {
	dir := s.setting("memory_canonical_dir")
	if dir == "" {
		dir = s.setting("dream_canonical_memory")
	}
	if dir == "" {
		return ""
	}
	return expandHome(dir)
}

// verifyPorts is the allowlist of ports a procedure check may probe.
func (s *Server) verifyPorts() []int {
	var out []int
	for _, f := range strings.FieldsFunc(s.setting("memory_verify_ports"), func(r rune) bool { return r == ',' || r == ' ' }) {
		if n, err := strconv.Atoi(f); err == nil && n > 0 && n < 65536 {
			out = append(out, n)
		}
	}
	return out
}

func (s *Server) procStatePath() string {
	return filepath.Join(s.Vault.Root, ".grimoire", "procedure-verify.json")
}

var procCache = struct {
	sync.Mutex
	path  string
	mtime time.Time
	size  int64
	state memstore.ProcStates
}{}

// procStates returns the verification state, re-read only when the file
// changes, since recall asks for it on every item.
func (s *Server) procStates() memstore.ProcStates {
	p := s.procStatePath()
	procCache.Lock()
	defer procCache.Unlock()
	fi, err := os.Stat(p)
	if err != nil {
		return memstore.ProcStates{}
	}
	if procCache.path != p || !fi.ModTime().Equal(procCache.mtime) || fi.Size() != procCache.size {
		procCache.state = memstore.LoadStates(p)
		procCache.path, procCache.mtime, procCache.size = p, fi.ModTime(), fi.Size()
	}
	return procCache.state
}

// ---- GET /api/memory/core --------------------------------------------------

// memoryCore returns the always-loaded part of memory: every standing rule on
// one line, then one pointer per topic. The canonical store's notes are used
// when the caller is an administrator (they live outside the vault's access
// model), and stored facts of kind rule are added for everyone, filtered as
// recall is.
func (s *Server) memoryCore(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	budget := memstore.DefaultBudget
	if v, err := strconv.Atoi(q.Get("budget")); err == nil && v > 0 {
		budget = min(max(v, 300), 1<<20)
	}
	format := q.Get("format")
	if format != "" && format != "md" && format != "text" {
		writeErr(w, http.StatusBadRequest, "format must be md or text")
		return
	}
	opt := memstore.CoreOptions{Budget: budget, Links: q.Get("links") == "1"}
	if s.Index != nil && s.Index.Emb != nil {
		opt.Embed = s.Index.Emb.Embed
	}

	var notes []memstore.Note
	source := ""
	if p := principal(r); p.Unrestricted || p.IsAdmin() {
		if dir := s.canonicalMemoryDir(); dir != "" {
			if loaded, err := memstore.LoadDir(dir); err == nil {
				notes, source = loaded, dir
			}
		}
	}
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{Filter: filterFor(r, false), AcceptedOnly: true,
		Limit: 500, Now: vault.Now()})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, h := range hits {
		if h.Untrusted() || memstore.Normalize(h.Category) != memstore.KindRule {
			continue
		}
		when, _ := time.Parse("2006-01-02 15:04", h.Stamp)
		opt.ExtraRules = append(opt.ExtraRules, memstore.Rule{Text: strings.Join(strings.Fields(h.Text), " "),
			Score: h.Helpful - h.Unhelpful, When: when})
	}
	core := memstore.BuildCore(notes, opt)
	if q.Get("raw") == "1" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(core.Text))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"core": core.Text, "lines": core.Lines, "bytes": core.Bytes, "max_bytes": budget,
		"rules": core.Rules, "rules_shown": core.RulesShown, "pointers": core.Pointers,
		"notes_covered": core.Notes, "source": source,
	})
}

// ---- kinds on items -------------------------------------------------------

// annotateItem fills Kind on a context item and marks a procedure whose last
// check failed as needing verification. A fact is looked up by id for its
// category; a note is read for its frontmatter. Failures leave the item as it
// was: kind is extra information, never a reason to drop memory.
func (s *Server) annotateItem(it *contextItem) {
	var key string
	switch {
	case it.ID != "":
		hits, err := s.Index.MemoryEntries(index.MemoryQuery{Filter: index.Filter{IgnoreACLs: true}, ID: it.ID,
			Note: it.Path, Limit: 1, Now: vault.Now()})
		category := ""
		if err == nil && len(hits) == 1 {
			category = hits[0].Category
		}
		it.Kind = memstore.KindOfFact(category, it.Text)
		key = "fact:" + it.ID
	case strings.HasSuffix(it.Path, ".md"):
		n, err := s.Vault.Read(it.Path)
		if err != nil || n.Encrypted {
			return
		}
		note := memstore.ParseNote(path.Base(it.Path), n.Raw, time.Time{})
		if !isFileMemoryNote(it.Path, note) {
			return
		}
		it.Kind = note.Kind
		key = "note:" + path.Base(it.Path)
	}
	if it.Kind == memstore.KindProcedure && it.Verify == "" {
		if st, ok := s.procStates()[key]; ok && st.Failed {
			it.Verify = "procedure check failed: " + st.Detail
		}
	}
}

// isFileMemoryNote is true for notes that are file-memory entries: they carry
// the name/description frontmatter. Ordinary notes get no kind.
func isFileMemoryNote(p string, n memstore.Note) bool {
	return n.Fields["name"] != "" && n.Fields["description"] != ""
}

// markProcedures adds Kind and the failed-check verify tag to recalled facts.
func (s *Server) markProcedures(out []entryOut) []entryOut {
	states := s.procStates()
	for i := range out {
		out[i].Kind = memstore.KindOfFact(out[i].Category, out[i].Text)
		if out[i].Kind == memstore.KindProcedure {
			if st, ok := states["fact:"+out[i].ID]; ok && st.Failed {
				out[i].Verify = "procedure check failed: " + st.Detail
			}
		}
	}
	return out
}

// ---- dream checks ---------------------------------------------------------

// memstoreFindings are the dream's checks over the shared store: procedures
// that are due for a light verification, and memories that break the writing
// guidelines. The first updates the verification schedule; neither changes a
// note.
func (s *Server) memstoreFindings(docs []dream.Doc) []dream.Finding {
	type item struct {
		key, path, text string
		init            memstore.ProcState
		kind            string
	}
	var items []item
	seen := map[string]bool{}
	add := func(n memstore.Note, p string) {
		key := "note:" + n.File
		if seen[key] {
			return
		}
		seen[key] = true
		items = append(items, item{key: key, path: p, text: n.Title + "\n" + n.Description + "\n" + n.Body,
			init: memstore.InitialState(n.Fields), kind: n.Kind})
	}
	for _, d := range docs {
		if d.Kind != dream.KindFileMemory {
			continue
		}
		n := memstore.ParseNote(path.Base(d.Path), d.Body, d.Mtime)
		if path.Base(d.Path) != memstore.IndexName && isFileMemoryNote(d.Path, n) {
			add(n, d.Path)
		}
	}
	if dir := s.canonicalMemoryDir(); dir != "" {
		if notes, err := memstore.LoadDir(dir); err == nil {
			for _, n := range notes {
				add(n, filepath.Join(dir, n.File))
			}
		}
	}
	if hits, err := s.Index.MemoryEntries(index.MemoryQuery{Filter: index.Filter{IgnoreACLs: true},
		AcceptedOnly: true, Limit: 5000, Now: vault.Now()}); err == nil {
		for _, h := range hits {
			if h.Untrusted() {
				continue
			}
			items = append(items, item{key: "fact:" + h.ID, path: h.Note, text: h.Text,
				kind: memstore.KindOfFact(h.Category, h.Text)})
		}
	}

	var out []dream.Finding
	now := time.Now()
	states := s.procStates()
	next := memstore.ProcStates{}
	for k, v := range states {
		next[k] = v
	}
	type due struct {
		item
		st memstore.ProcState
	}
	var dues []due
	for _, it := range items {
		if it.kind != memstore.KindProcedure {
			continue
		}
		st, ok := next[it.key]
		if !ok {
			st = it.init
		}
		if st.Due(now) {
			dues = append(dues, due{it, st})
		}
	}
	// Failed first, then the longest unchecked: a failed procedure stays on
	// the list until it passes; the rest take turns.
	sort.SliceStable(dues, func(i, j int) bool {
		if dues[i].st.Failed != dues[j].st.Failed {
			return dues[i].st.Failed
		}
		return dues[i].st.VerifiedAt.Before(dues[j].st.VerifiedAt)
	})
	limit := 3
	if v, err := strconv.Atoi(s.setting("memory_verify_per_dream")); err == nil && v >= 0 {
		limit = v
	}
	if len(dues) > limit {
		dues = dues[:limit]
	}
	checkers := memstore.Checkers(memstore.DefaultEnv(s.verifyPorts()))
	for _, d := range dues {
		all, failed := memstore.RunChecks(d.text, checkers)
		if len(all) == 0 {
			// Nothing deterministic to check: count it as seen, not as proven.
			next[d.key] = d.st.Record(true, "", now)
			continue
		}
		detail := memstore.FailureDetail(failed)
		next[d.key] = d.st.Record(len(failed) == 0, detail, now)
		if len(failed) > 0 {
			out = append(out, dream.Finding{Check: "procedure_verify", Category: dream.Hygiene,
				Severity: dream.Medium, Path: d.path,
				Message: "procedure may be out of date: " + detail + " (shown with verify on recall; not changed)"})
		}
	}
	if len(dues) > 0 {
		if err := next.Save(s.procStatePath()); err != nil {
			out = append(out, dream.Finding{Check: "procedure_verify", Category: dream.Hygiene, Severity: dream.Info,
				Path: s.procStatePath(), Message: "could not save verification state: " + err.Error()})
		}
	}
	// Failed procedures that were not re-checked this run still deserve a line.
	checked := map[string]bool{}
	for _, d := range dues {
		checked[d.key] = true
	}
	for _, it := range items {
		if st := next[it.key]; st.Failed && !checked[it.key] && it.kind == memstore.KindProcedure {
			out = append(out, dream.Finding{Check: "procedure_verify", Category: dream.Hygiene, Severity: dream.Medium,
				Path: it.path, Message: "procedure may be out of date: " + st.Detail})
		}
	}

	// Writing quality, listed once per memory and capped so a large old store
	// does not bury the report.
	const maxQuality = 30
	n := 0
	for _, it := range items {
		var ws []string
		for _, w := range memstore.Lint(it.kind, it.text) {
			if w.Code != "looks_like_secret" { // the security sweep already owns secrets
				ws = append(ws, w.Code)
			}
		}
		if len(ws) == 0 {
			continue
		}
		if n++; n > maxQuality {
			continue
		}
		out = append(out, dream.Finding{Check: "memory_quality", Category: dream.Hygiene, Severity: dream.Info,
			Path: it.path, Message: "breaks the writing guidelines (docs/MEMORY_WRITING.md): " + strings.Join(ws, ", ")})
	}
	if n > maxQuality {
		out = append(out, dream.Finding{Check: "memory_quality", Category: dream.Hygiene, Severity: dream.Info,
			Message: fmt.Sprintf("%d more memories break the writing guidelines; showing the first %d", n-maxQuality, maxQuality)})
	}
	return out
}
