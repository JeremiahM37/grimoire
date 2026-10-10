package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/codegraph"
)

// The code graph over HTTP (internal/codegraph).
//
// Every route here is admin-only. A code index has no owner and no space: a
// symbol table of a repository is not something a member of the vault should
// see just because they can read the notes. The same reasoning is why indexing
// is restricted by path. A request names a directory, and the server reads it,
// so a caller who could name any directory could read any file on the machine.
// The path must therefore sit under the vault or under GRIMOIRE_CODE_ROOTS,
// and must resolve to that location once symlinks are followed.

// codeKinds are the symbol kinds a caller may filter by.
var codeKinds = map[string]bool{
	codegraph.KindFunc: true, codegraph.KindMethod: true, codegraph.KindStruct: true,
	codegraph.KindInterface: true, codegraph.KindType: true, codegraph.KindConst: true,
	codegraph.KindVar: true, codegraph.KindClass: true, codegraph.KindEnum: true,
}

var errCodeForbidden = errors.New("path is outside the vault and GRIMOIRE_CODE_ROOTS")

func (s *Server) codeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/code/index", s.adminOnly(s.codeIndex))
	mux.HandleFunc("GET /api/code/symbol", s.adminOnly(s.codeSymbol))
	mux.HandleFunc("GET /api/code/callers", s.adminOnly(s.codeCallers))
	mux.HandleFunc("GET /api/code/outline", s.adminOnly(s.codeOutline))
}

// codeStore returns the code-graph store, building it over the index on first
// use the way Banks is built, so a server with no CodeRoots still answers.
func (s *Server) codeStore() *codegraph.Store {
	if s.Code == nil && s.Index != nil && s.Index.DB != nil {
		s.Code = codegraph.NewStore(s.Index.DB)
	}
	return s.Code
}

// codeAllowRoots lists the directories the server may index: the vault, and
// every entry of GRIMOIRE_CODE_ROOTS.
func (s *Server) codeAllowRoots() []string {
	var roots []string
	if s.Vault != nil && s.Vault.Root != "" {
		roots = append(roots, s.Vault.Root)
	}
	for _, r := range s.CodeRoots {
		if strings.TrimSpace(r) != "" {
			roots = append(roots, r)
		}
	}
	return roots
}

// resolveCodePath checks that p is an absolute directory inside an allowed root
// and returns it with symlinks resolved. The check runs on the resolved path,
// so a link inside an allowed root that points at /etc is refused.
func (s *Server) resolveCodePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("path must be absolute")
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", errors.New("no such directory")
	}
	if info, err := os.Stat(real); err != nil || !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	for _, root := range s.codeAllowRoots() {
		rr, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rr = filepath.Clean(rr)
		if real == rr || strings.HasPrefix(real, rr+string(filepath.Separator)) {
			return real, nil
		}
	}
	return "", errCodeForbidden
}

func (s *Server) codeIndex(w http.ResponseWriter, r *http.Request) {
	st := s.codeStore()
	if st == nil {
		writeErr(w, http.StatusNotImplemented, "the code graph is unavailable")
		return
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be JSON with a path")
		return
	}
	root, err := s.resolveCodePath(in.Path)
	if errors.Is(err, errCodeForbidden) {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	stats, err := st.Index(root)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// codeLimit reads ?limit= with a default and a ceiling.
func codeLimit(r *http.Request, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func (s *Server) codeSymbol(w http.ResponseWriter, r *http.Request) {
	st := s.codeStore()
	if st == nil {
		writeErr(w, http.StatusNotImplemented, "the code graph is unavailable")
		return
	}
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("name"))
	if name == "" || len(name) > 200 {
		writeErr(w, http.StatusBadRequest, "name is required (at most 200 characters)")
		return
	}
	kind := q.Get("kind")
	if kind != "" && !codeKinds[kind] {
		writeErr(w, http.StatusBadRequest, "unknown kind "+kind)
		return
	}
	hits, err := st.Symbols(name, kind, q.Get("root"), codeLimit(r, 100, 500))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"symbols": hits, "count": len(hits)})
}

func (s *Server) codeCallers(w http.ResponseWriter, r *http.Request) {
	st := s.codeStore()
	if st == nil {
		writeErr(w, http.StatusNotImplemented, "the code graph is unavailable")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" || len(name) > 200 {
		writeErr(w, http.StatusBadRequest, "name is required (at most 200 characters)")
		return
	}
	calls, err := st.Callers(name, codeLimit(r, 100, 500))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"callers": calls, "count": len(calls),
		"note": "matched by name; calls are not resolved to a target, so this is approximate"})
}

func (s *Server) codeOutline(w http.ResponseWriter, r *http.Request) {
	st := s.codeStore()
	if st == nil {
		writeErr(w, http.StatusNotImplemented, "the code graph is unavailable")
		return
	}
	file := strings.TrimSpace(r.URL.Query().Get("file"))
	if file == "" || len(file) > 4096 {
		writeErr(w, http.StatusBadRequest, "file is required")
		return
	}
	files, err := st.Outline(file)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "count": len(files)})
}
