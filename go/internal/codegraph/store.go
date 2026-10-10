package codegraph

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

// Store reads and writes the code-graph tables in the index.
type Store struct {
	DB *db.DB
	// MaxFileBytes caps the size of one source file. Zero means
	// DefaultMaxFileBytes.
	MaxFileBytes int64

	// runMu serializes Index runs. Two runs over one root would each read the
	// stored hashes before either wrote, and both would parse the same files.
	runMu sync.Mutex
}

// NewStore wraps the index connection.
func NewStore(database *db.DB) *Store { return &Store{DB: database} }

// Stats reports what one Index run did.
type Stats struct {
	Root      string `json:"root"`
	Files     int    `json:"files"`     // source files found
	Indexed   int    `json:"indexed"`   // parsed because new or changed
	Unchanged int    `json:"unchanged"` // hash matched; not re-parsed
	Removed   int    `json:"removed"`   // were indexed, now gone
	Failed    int    `json:"failed"`    // could not be read or parsed
	TooLarge  int    `json:"too_large"` // over the size cap, skipped
	Symbols   int    `json:"symbols"`   // total under this root afterwards
	Edges     int    `json:"edges"`     // total under this root afterwards
}

// Index brings the code graph for one repository root up to date. root must be
// an absolute path to a directory; the caller is responsible for deciding that
// the server may read it.
//
// Files are keyed by path and content hash. A file whose hash is unchanged is
// skipped without being parsed, and a file that has disappeared has its rows
// removed. Each changed file is written in its own transaction under the index
// write lock, so a large repository does not hold the lock for its whole run.
func (s *Store) Index(root string) (Stats, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if !filepath.IsAbs(root) {
		return Stats{}, errors.New("codegraph: root must be an absolute path")
	}
	root = filepath.Clean(root)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return Stats{}, fmt.Errorf("codegraph: %s is not a directory", root)
	}
	maxBytes := s.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFileBytes
	}
	st := Stats{Root: root}

	existing, err := s.hashes(root)
	if err != nil {
		return st, err
	}
	files, tooLarge, err := walkSources(root, maxBytes)
	if err != nil {
		return st, err
	}
	st.TooLarge = tooLarge
	st.Files = len(files)

	seen := make(map[string]bool, len(files))
	for _, f := range files {
		seen[f.rel] = true
		src, err := os.ReadFile(f.abs)
		if err != nil {
			st.Failed++
			// An empty hash never matches, so the next run retries the file.
			if err := s.write(root, f.rel, "", f.lang, err.Error(), FileGraph{}); err != nil {
				return st, err
			}
			continue
		}
		sum := sha256.Sum256(src)
		hash := hex.EncodeToString(sum[:])
		if old, ok := existing[f.rel]; ok && old == hash {
			st.Unchanged++
			continue
		}
		fg, err := Extract(f.rel, src)
		if err != nil {
			st.Failed++
			if err := s.write(root, f.rel, hash, f.lang, err.Error(), FileGraph{}); err != nil {
				return st, err
			}
			continue
		}
		if err := s.write(root, f.rel, hash, f.lang, "", fg); err != nil {
			return st, err
		}
		st.Indexed++
	}

	for rel := range existing {
		if seen[rel] {
			continue
		}
		if err := s.remove(root, rel); err != nil {
			return st, err
		}
		st.Removed++
	}

	if err := s.DB.Conn().QueryRow(
		"SELECT (SELECT COUNT(*) FROM code_symbols WHERE root=?), (SELECT COUNT(*) FROM code_edges WHERE root=?)",
		root, root).Scan(&st.Symbols, &st.Edges); err != nil {
		return st, err
	}
	return st, nil
}

// hashes returns the stored content hash of every indexed file under root.
func (s *Store) hashes(root string) (map[string]string, error) {
	rows, err := s.DB.Query("SELECT path, hash FROM code_files WHERE root=?", root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, h string
		if err := rows.Scan(&p, &h); err != nil {
			return nil, err
		}
		out[p] = h
	}
	return out, rows.Err()
}

// write replaces one file's rows. A file that failed to parse is recorded with
// its error and no symbols, so the failure is visible in outline.
func (s *Store) write(root, rel, hash, lang, errMsg string, fg FileGraph) error {
	s.DB.Lock()
	defer s.DB.Unlock()
	tx, err := s.DB.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteRows(tx, root, rel); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO code_files(root, path, hash, lang, error) VALUES(?,?,?,?,?)
		ON CONFLICT(root, path) DO UPDATE SET hash=excluded.hash, lang=excluded.lang, error=excluded.error`,
		root, rel, hash, lang, errMsg); err != nil {
		return err
	}
	for _, sym := range fg.Symbols {
		if _, err := tx.Exec(`INSERT INTO code_symbols(root, path, name, kind, scope, line, end_line)
			VALUES(?,?,?,?,?,?,?)`, root, rel, sym.Name, sym.Kind, sym.Scope, sym.Line, sym.EndLine); err != nil {
			return err
		}
	}
	for _, e := range fg.Edges {
		if _, err := tx.Exec(`INSERT INTO code_edges(root, path, line, kind, caller, callee, qual)
			VALUES(?,?,?,?,?,?,?)`, root, rel, e.Line, e.Kind, e.Caller, e.Callee, e.Qual); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// remove drops every row for one file.
func (s *Store) remove(root, rel string) error {
	s.DB.Lock()
	defer s.DB.Unlock()
	tx, err := s.DB.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteRows(tx, root, rel); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteRows(tx *sql.Tx, root, rel string) error {
	for _, q := range []string{
		"DELETE FROM code_symbols WHERE root=? AND path=?",
		"DELETE FROM code_edges WHERE root=? AND path=?",
		"DELETE FROM code_files WHERE root=? AND path=?",
	} {
		if _, err := tx.Exec(q, root, rel); err != nil {
			return err
		}
	}
	return nil
}

// Hit is one symbol found by name.
type Hit struct {
	Root      string `json:"root"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Scope     string `json:"scope,omitempty"`
	Qualified string `json:"qualified"`
	Line      int    `json:"line"`
	EndLine   int    `json:"end_line,omitempty"`
}

// Symbols finds declarations. name matches the bare name of any declaration,
// or its qualified form "Type.Method". kind and root, when set, narrow it.
func (s *Store) Symbols(name, kind, root string, limit int) ([]Hit, error) {
	q := `SELECT root, path, name, kind, scope, line, end_line FROM code_symbols
		WHERE (name = ? OR (scope <> '' AND scope || '.' || name = ?))`
	args := []any{name, name}
	if kind != "" {
		q += " AND kind = ?"
		args = append(args, kind)
	}
	if root != "" {
		q += " AND root = ?"
		args = append(args, root)
	}
	q += " ORDER BY root, path, line LIMIT ?"
	args = append(args, limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Hit{}
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Root, &h.Path, &h.Name, &h.Kind, &h.Scope, &h.Line, &h.EndLine); err != nil {
			return nil, err
		}
		h.Qualified = Symbol{Name: h.Name, Scope: h.Scope}.Qualified()
		out = append(out, h)
	}
	return out, rows.Err()
}

// Caller is one call site.
type Caller struct {
	Root   string `json:"root"`
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Caller string `json:"caller,omitempty"`
	Callee string `json:"callee"`
	Qual   string `json:"qual,omitempty"`
}

// Callers finds call sites of a name. The match is by name, not by resolved
// target: "Save" finds every call to something called Save on any type. A
// dotted name "pkg.Save" narrows to calls written as pkg.Save(). Method calls
// on a variable (x.Save()) are recorded with the variable as the qualifier, so
// a type-qualified query such as "Store.Save" does not find them. Use the bare
// name for the broadest answer.
func (s *Store) Callers(name string, limit int) ([]Caller, error) {
	callee, qual := name, ""
	if i := strings.LastIndex(name, "."); i >= 0 {
		qual, callee = name[:i], name[i+1:]
	}
	q := `SELECT root, path, line, caller, callee, qual FROM code_edges
		WHERE kind = 'call' AND callee = ?`
	args := []any{callee}
	if qual != "" {
		q += " AND qual = ?"
		args = append(args, qual)
	}
	q += " ORDER BY root, path, line LIMIT ?"
	args = append(args, limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Caller{}
	for rows.Next() {
		var c Caller
		if err := rows.Scan(&c.Root, &c.Path, &c.Line, &c.Caller, &c.Callee, &c.Qual); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FileOutline is what one indexed file declares and imports.
type FileOutline struct {
	Root    string   `json:"root"`
	Path    string   `json:"path"`
	Abs     string   `json:"abs"`
	Lang    string   `json:"lang"`
	Error   string   `json:"error,omitempty"`
	Symbols []Symbol `json:"symbols"`
	Imports []Edge   `json:"imports"`
}

// Outline returns the outline of a file. file is either an absolute path or a
// path relative to a root; a relative path can match in more than one root, and
// all matches are returned.
func (s *Store) Outline(file string) ([]FileOutline, error) {
	file = filepath.ToSlash(filepath.Clean(file))
	rows, err := s.DB.Query(
		"SELECT root, path, lang, error FROM code_files WHERE (root || '/' || path) = ? OR path = ? ORDER BY root, path",
		file, strings.TrimPrefix(file, "/"))
	if err != nil {
		return nil, err
	}
	type fileRow struct{ root, path, lang, errMsg string }
	var found []fileRow
	for rows.Next() {
		var r fileRow
		if err := rows.Scan(&r.root, &r.path, &r.lang, &r.errMsg); err != nil {
			rows.Close()
			return nil, err
		}
		found = append(found, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := []FileOutline{}
	for _, r := range found {
		fo := FileOutline{Root: r.root, Path: r.path, Abs: path.Join(r.root, r.path),
			Lang: r.lang, Error: r.errMsg, Symbols: []Symbol{}, Imports: []Edge{}}
		srows, err := s.DB.Query(
			"SELECT name, kind, scope, line, end_line FROM code_symbols WHERE root=? AND path=? ORDER BY line, name",
			r.root, r.path)
		if err != nil {
			return nil, err
		}
		for srows.Next() {
			var sym Symbol
			if err := srows.Scan(&sym.Name, &sym.Kind, &sym.Scope, &sym.Line, &sym.EndLine); err != nil {
				srows.Close()
				return nil, err
			}
			fo.Symbols = append(fo.Symbols, sym)
		}
		srows.Close()
		erows, err := s.DB.Query(
			"SELECT line, callee, qual FROM code_edges WHERE root=? AND path=? AND kind='import' ORDER BY line",
			r.root, r.path)
		if err != nil {
			return nil, err
		}
		for erows.Next() {
			e := Edge{Kind: EdgeImport}
			if err := erows.Scan(&e.Line, &e.Callee, &e.Qual); err != nil {
				erows.Close()
				return nil, err
			}
			fo.Imports = append(fo.Imports, e)
		}
		erows.Close()
		out = append(out, fo)
	}
	return out, nil
}
