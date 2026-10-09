package memstore

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Shape says how an agent keeps memory.
const (
	ShapeDir  = "dir"  // a directory of notes: linked by symlink
	ShapeFile = "file" // one instruction file (AGENTS.md, GEMINI.md): a managed block
)

// Managed block markers. Everything between them is regenerated; everything
// outside is the agent's own and is never touched.
const (
	BlockStart = "<!-- grimoire:memory:start (generated; edit the store, not this block) -->"
	BlockEnd   = "<!-- grimoire:memory:end -->"
)

// LinkOptions shapes Link.
type LinkOptions struct {
	Agent  string // names conflict copies and the backup
	Merge  bool   // fold a differing directory into the store instead of refusing
	DryRun bool
	// Block is the text for a file-shaped target.
	Block string
	Now   func() time.Time
}

// LinkResult says what Link did (or would do).
type LinkResult struct {
	Action  string   // linked, already-linked, merged, block-written, block-unchanged
	Backup  string   // where the previous directory/file went
	Moved   []string // files moved into the store
	Deduped []string // files already in the store, identical
	Renamed []string // conflicting files kept under another name
}

func (o LinkOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o LinkOptions) stamp() string { return o.now().Format("20060102-150405") }

// Link points an agent's memory at the canonical store. kind is ShapeDir or
// ShapeFile. A directory becomes a symlink to canonical, after its backup; a
// file gets the managed block.
func Link(kind, path, canonical string, opt LinkOptions) (LinkResult, error) {
	switch kind {
	case ShapeDir:
		return linkDir(path, canonical, opt)
	case ShapeFile:
		return linkFile(path, opt)
	}
	return LinkResult{}, fmt.Errorf("unknown memory shape %q (want dir or file)", kind)
}

func linkDir(path, canonical string, opt LinkOptions) (LinkResult, error) {
	var res LinkResult
	canonical, _ = filepath.Abs(canonical)
	if !opt.DryRun {
		if err := os.MkdirAll(canonical, 0o755); err != nil {
			return res, err
		}
	}
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !opt.DryRun {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return res, err
			}
			if err := os.Symlink(canonical, path); err != nil {
				return res, err
			}
		}
		res.Action = "linked"
		return res, nil
	case err != nil:
		return res, err
	}

	source := path
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return res, fmt.Errorf("%s is a dangling symlink; remove it first", path)
		}
		if same(target, canonical) {
			res.Action = "already-linked"
			return res, nil
		}
		source = target // a link to some other directory: treat that as the source
	} else if !fi.IsDir() {
		return res, fmt.Errorf("%s exists and is not a directory", path)
	}

	plan, err := planMerge(source, canonical, opt.Agent)
	if err != nil {
		return res, err
	}
	res.Deduped = plan.same
	if len(plan.move) > 0 || len(plan.conflict) > 0 {
		if !opt.Merge {
			return res, fmt.Errorf("%s holds %d file(s) the store does not have and %d that differ; "+
				"nothing was changed. Re-run with --merge to fold them into %s (the original is kept as a backup)",
				path, len(plan.move), len(plan.conflict), canonical)
		}
		for _, m := range plan.move {
			res.Moved = append(res.Moved, m.rel)
		}
		for _, m := range plan.conflict {
			res.Renamed = append(res.Renamed, m.rel+" -> "+m.dstRel)
		}
	}
	res.Action = "linked"
	if len(res.Moved)+len(res.Renamed) > 0 {
		res.Action = "merged"
	}
	if opt.DryRun {
		return res, nil
	}
	for _, m := range append(append([]mergeItem(nil), plan.move...), plan.conflict...) {
		dst := filepath.Join(canonical, m.dstRel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return res, err
		}
		if err := copyFile(filepath.Join(source, m.rel), dst); err != nil {
			return res, err
		}
	}
	// The whole original stays as a backup, so the copy above loses nothing.
	backup := uniqueBackup(path, opt)
	if fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil {
			return res, err
		}
	} else if err := os.Rename(path, backup); err != nil {
		return res, err
	} else {
		res.Backup = backup
	}
	if err := os.Symlink(canonical, path); err != nil {
		if res.Backup != "" { // put it back rather than leave the agent with no memory
			_ = os.Rename(res.Backup, path)
		}
		return res, err
	}
	return res, nil
}

type mergeItem struct{ rel, dstRel string }

type mergePlan struct {
	move, conflict []mergeItem
	same           []string
}

// planMerge compares every regular file under src with dst. Identical files
// dedup; absent ones move; differing ones are kept under a new name rather
// than overwriting either side.
func planMerge(src, dst, agent string) (mergePlan, error) {
	var p mergePlan
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		other := filepath.Join(dst, rel)
		a, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(other)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			p.move = append(p.move, mergeItem{rel, rel})
		case err != nil:
			return err
		case bytes.Equal(a, b):
			p.same = append(p.same, rel)
		default:
			ext := filepath.Ext(rel)
			name := strings.TrimSuffix(rel, ext) + ".from-" + sanitize(agent) + ext
			p.conflict = append(p.conflict, mergeItem{rel, name})
		}
		return nil
	})
	sort.Slice(p.move, func(i, j int) bool { return p.move[i].rel < p.move[j].rel })
	return p, err
}

func sanitize(s string) string {
	if s == "" {
		return "agent"
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, s)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("refusing to overwrite %s", dst)
	}
	return os.WriteFile(dst, b, 0o644)
}

func same(a, b string) bool {
	ra, e1 := filepath.EvalSymlinks(a)
	rb, e2 := filepath.EvalSymlinks(b)
	return e1 == nil && e2 == nil && ra == rb
}

func linkFile(path string, opt LinkOptions) (LinkResult, error) {
	var res LinkResult
	cur, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}
	next := SetBlock(string(cur), opt.Block)
	if next == string(cur) && err == nil {
		res.Action = "block-unchanged"
		return res, nil
	}
	res.Action = "block-written"
	if opt.DryRun {
		return res, nil
	}
	if err == nil {
		res.Backup = uniqueBackup(path, opt)
		if werr := os.WriteFile(res.Backup, cur, 0o600); werr != nil {
			return res, werr
		}
	} else if merr := os.MkdirAll(filepath.Dir(path), 0o755); merr != nil {
		return res, merr
	}
	return res, os.WriteFile(path, []byte(next), 0o644)
}

// SetBlock puts block between the markers of text, replacing an existing
// block or appending one. An empty block removes it.
func SetBlock(text, block string) string {
	start := strings.Index(text, "<!-- grimoire:memory:start")
	end := strings.Index(text, BlockEnd)
	var before, after string
	if start >= 0 && end > start {
		before, after = text[:start], strings.TrimPrefix(text[end+len(BlockEnd):], "\n")
	} else {
		before = text
	}
	if strings.TrimSpace(block) == "" {
		return strings.TrimRight(before, "\n") + trailingNL(before, after) + after
	}
	body := BlockStart + "\n" + strings.TrimRight(block, "\n") + "\n" + BlockEnd + "\n"
	if start >= 0 && end > start {
		return before + body + after
	}
	if strings.TrimSpace(before) == "" {
		return body
	}
	return strings.TrimRight(before, "\n") + "\n\n" + body
}

func trailingNL(before, after string) string {
	if strings.TrimSpace(before) == "" && after == "" {
		return ""
	}
	return "\n"
}

// Block extracts the managed block's body, "" when there is none.
func Block(text string) string {
	start := strings.Index(text, "<!-- grimoire:memory:start")
	end := strings.Index(text, BlockEnd)
	if start < 0 || end <= start {
		return ""
	}
	inner := text[start:end]
	if i := strings.Index(inner, "\n"); i >= 0 {
		inner = inner[i+1:]
	}
	return strings.TrimRight(inner, "\n") + "\n"
}

// LinkState describes where an agent's memory location stands.
type LinkState struct {
	Shape  string
	Path   string
	State  string // absent, linked, linked-elsewhere, directory, file, block, block-stale, no-block
	Target string // for a symlink
	Notes  int    // notes in a directory
	Detail string
}

// Status inspects a memory location. want is the block text a file should
// hold (for staleness); ignored for directories.
func Status(kind, path, canonical, want string) LinkState {
	st := LinkState{Shape: kind, Path: path}
	fi, err := os.Lstat(path)
	if err != nil {
		st.State = "absent"
		return st
	}
	if kind == ShapeFile {
		b, err := os.ReadFile(path)
		if err != nil {
			st.State, st.Detail = "unreadable", err.Error()
			return st
		}
		got := Block(string(b))
		switch {
		case got == "":
			st.State = "no-block"
		case want != "" && strings.TrimSpace(got) != strings.TrimSpace(want):
			st.State = "block-stale"
		default:
			st.State = "block"
		}
		return st
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, _ := filepath.EvalSymlinks(path)
		st.Target = target
		if same(path, canonical) {
			st.State = "linked"
		} else {
			st.State = "linked-elsewhere"
		}
	} else if fi.IsDir() {
		st.State = "directory"
	} else {
		st.State = "file"
	}
	if entries, err := os.ReadDir(path); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".md") && e.Name() != IndexName {
				st.Notes++
			}
		}
	}
	return st
}

// Unlink undoes Link. A symlinked directory is replaced by the most recent
// backup if there is one, else by a real copy of the store, so the agent never
// loses its memory. A managed block is removed and the rest of the file kept.
func Unlink(kind, path, canonical string, opt LinkOptions) (string, error) {
	if kind == ShapeFile {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		next := SetBlock(string(b), "")
		if next == string(b) {
			return "no managed block", nil
		}
		if opt.DryRun {
			return "block-removed", nil
		}
		return "block-removed", os.WriteFile(path, []byte(next), 0o644)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return "not a link", nil
	}
	if !same(path, canonical) {
		return "", fmt.Errorf("%s does not point at the store; not touching it", path)
	}
	backups, _ := filepath.Glob(path + ".grimoire-bak-*")
	sort.Strings(backups)
	if opt.DryRun {
		return "unlinked", nil
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	if len(backups) > 0 {
		last := backups[len(backups)-1]
		if err := os.Rename(last, path); err != nil {
			return "", err
		}
		return "restored " + last, nil
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", err
	}
	err = filepath.WalkDir(canonical, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(canonical, p)
		dst := filepath.Join(path, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return copyFile(p, dst)
	})
	return "unlinked; left a copy of the store", err
}

// uniqueBackup names a backup beside path that does not exist yet.
func uniqueBackup(path string, opt LinkOptions) string {
	base := path + ".grimoire-bak-" + opt.stamp()
	name := base
	for i := 2; ; i++ {
		if _, err := os.Lstat(name); err != nil {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}
