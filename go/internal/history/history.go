// Package history is note version history — automatic file-recovery snapshots.
//
// Port of server/history.py. Every content-changing save snapshots the note's
// *previous* on-disk body into .grimoire/history/<flattened-path>/<millis>.md
// before the write. A per-note ring buffer keeps the newest Keep versions;
// identical consecutive snapshots are skipped. Restoring never discards work:
// the current body is snapshotted first, so a restore is itself undoable.
//
// Security: what is snapshotted is whatever is on disk — for an encrypted note
// that is the ciphertext. Plaintext of encrypted notes never lands in history.
package history

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Keep is the number of newest versions retained per note.
const Keep = 25

// idRE validates a version id strictly: it becomes part of a filesystem path,
// so anything outside a plain millisecond stamp is refused rather than cleaned.
var idRE = regexp.MustCompile(`^\d{10,16}$`)

// Now is indirected so tests can pin timestamps.
var Now = func() time.Time { return time.Now() }

// Store holds snapshots under a .grimoire directory.
type Store struct{ Dir string }

func New(grimoireDir string) *Store {
	return &Store{Dir: filepath.Join(grimoireDir, "history")}
}

// dirFor flattens a note path into one safe directory name:
// journal/2026.md → journal__2026.md
func (s *Store) dirFor(rel string) string {
	return filepath.Join(s.Dir, strings.ReplaceAll(rel, "/", "__"))
}

// Version describes one stored snapshot.
type Version struct {
	ID   string  `json:"id"`
	TS   float64 `json:"ts"`
	Size int64   `json:"size"`
}

// Snapshot stores body as the newest version of rel. Exact duplicates of the
// most recent snapshot are skipped and anything beyond Keep is pruned.
//
// Errors are swallowed deliberately: history is a safety net, never a reason a
// save may fail.
func (s *Store) Snapshot(rel, body string) {
	d := s.dirFor(rel)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return
	}
	existing := s.versionFiles(d)
	if len(existing) > 0 {
		if prev, err := os.ReadFile(filepath.Join(d, existing[len(existing)-1])); err == nil {
			if string(prev) == body {
				return
			}
		}
	}
	name := strconv.FormatInt(Now().UnixMilli(), 10) + ".md"
	if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o600); err != nil {
		return
	}
	// prune oldest beyond the ring size
	if over := len(existing) + 1 - Keep; over > 0 {
		for _, old := range existing[:over] {
			_ = os.Remove(filepath.Join(d, old))
		}
	}
}

// versionFiles returns snapshot filenames in ascending id order.
func (s *Store) versionFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if idRE.MatchString(strings.TrimSuffix(e.Name(), ".md")) {
			out = append(out, e.Name())
		}
	}
	// numeric order, not lexical: ids are variable-length millisecond stamps
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.ParseInt(strings.TrimSuffix(out[i], ".md"), 10, 64)
		b, _ := strconv.ParseInt(strings.TrimSuffix(out[j], ".md"), 10, 64)
		return a < b
	})
	return out
}

// ListVersions returns a note's versions, newest first.
func (s *Store) ListVersions(rel string) []Version {
	d := s.dirFor(rel)
	files := s.versionFiles(d)
	out := []Version{}
	for i := len(files) - 1; i >= 0; i-- {
		id := strings.TrimSuffix(files[i], ".md")
		ms, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			continue
		}
		var size int64
		if info, err := os.Stat(filepath.Join(d, files[i])); err == nil {
			size = info.Size()
		}
		out = append(out, Version{ID: id, TS: float64(ms) / 1000, Size: size})
	}
	return out
}

// GetVersion returns one version's body, or ok=false when absent.
func (s *Store) GetVersion(rel, versionID string) (string, bool) {
	if !idRE.MatchString(versionID) {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(s.dirFor(rel), versionID+".md"))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// Scan calls fn with every stored version body, for read-only checks.
func (s *Store) Scan(fn func(rel, body string)) {
	dirs, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		rel := strings.ReplaceAll(d.Name(), "__", "/")
		dir := filepath.Join(s.Dir, d.Name())
		for _, f := range s.versionFiles(dir) {
			if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
				fn(rel, string(b))
			}
		}
	}
}

// Rewrite replaces, in place, every stored version body for which fn returns
// changed=true, and reports how many versions it rewrote. It is for removing
// information that must not survive in history, such as a forgotten fact; the
// version ring and its ordering are untouched.
func (s *Store) Rewrite(fn func(rel, body string) (string, bool)) (int, error) {
	dirs, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		rel := strings.ReplaceAll(d.Name(), "__", "/")
		dir := filepath.Join(s.Dir, d.Name())
		for _, f := range s.versionFiles(dir) {
			p := filepath.Join(dir, f)
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			out, changed := fn(rel, string(b))
			if !changed {
				continue
			}
			if err := os.WriteFile(p, []byte(out), 0o600); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
