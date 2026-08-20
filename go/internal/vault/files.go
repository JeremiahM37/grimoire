package vault

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

// TimeFormat is the frontmatter timestamp layout — Python's
// "%Y-%m-%dT%H:%M:%S", local time, no zone suffix.
const TimeFormat = "2006-01-02T15:04:05"

// Now is indirected so tests can pin timestamps; production leaves it alone.
var Now = func() time.Time { return time.Now() }

// Read loads and parses a note.
func (v *Vault) Read(rel string) (*Note, error) {
	p, err := v.SafePath(rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("%w: no such note: %s", ErrVault, rel)
	}
	text, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	relOf, err := v.RelOf(p)
	if err != nil {
		return nil, err
	}
	return NoteFromText(relOf, string(text), float64(info.ModTime().UnixNano())/1e9), nil
}

// Write saves a note, merging frontmatter and stamping created/updated.
//
// Round-trip fidelity: when the note already exists its raw frontmatter block
// is PATCHED rather than regenerated, so nested maps, block scalars and foreign
// keys written by another markdown app survive byte for byte.
func (v *Vault) Write(rel, body string, fm *markdown.Frontmatter) (*Note, error) {
	p, err := v.SafePath(rel)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	if fm == nil {
		fm = markdown.NewFrontmatter()
	} else {
		fm = fm.Clone()
	}
	now := Now().Format(TimeFormat)

	var rawInner *string
	if existing, err := os.ReadFile(p); err == nil {
		existingFM, _ := markdown.ParseFrontmatter(string(existing))
		if _, ok := fm.Get("created"); !ok {
			if c := existingFM.StringVal("created"); c != "" {
				fm.Set("created", c)
			} else {
				fm.Set("created", now)
			}
		}
		if inner, ok := rawFrontmatterBlock(string(existing)); ok {
			rawInner = &inner
		}
	} else if _, ok := fm.Get("created"); !ok {
		fm.Set("created", now)
	}
	fm.Set("updated", now)

	var text string
	if rawInner != nil {
		block := PatchFrontmatter(*rawInner, fm)
		b := body
		if !strings.HasPrefix(b, "\n") {
			b = "\n" + b
		}
		text = block + strings.TrimRight(b, "\n") + "\n"
	} else {
		text = Serialize(fm, body)
	}
	if err := atomicWrite(p, text); err != nil {
		return nil, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	relOf, err := v.RelOf(p)
	if err != nil {
		return nil, err
	}
	return NoteFromText(relOf, text, float64(info.ModTime().UnixNano())/1e9), nil
}

// rawFrontmatterBlock returns the inner text of a leading frontmatter block.
func rawFrontmatterBlock(text string) (string, bool) {
	if !strings.HasPrefix(text, "---") {
		return "", false
	}
	fm, body := markdown.ParseFrontmatter(text)
	if fm.Len() == 0 && body == text {
		return "", false
	}
	rest := text[3:]
	nl := strings.Index(rest, "\n")
	if nl < 0 {
		return "", false
	}
	rest = rest[nl+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// atomicWrite writes via a temp file and renames, so a crash mid-write can
// never leave a half-written note in the vault.
func atomicWrite(p, text string) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Delete removes a note. A missing note is not an error.
func (v *Vault) Delete(rel string) error {
	p, err := v.SafePath(rel)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Rename moves a note, refusing to clobber an existing target.
func (v *Vault) Rename(oldRel, newRel string) (string, error) {
	src, err := v.SafePath(oldRel)
	if err != nil {
		return "", err
	}
	dst, err := v.SafePath(newRel)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(src); err != nil {
		return "", fmt.Errorf("%w: no such note: %s", ErrVault, oldRel)
	}
	if _, err := os.Stat(dst); err == nil {
		return "", fmt.Errorf("%w: target exists: %s", ErrVault, newRel)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	return v.RelOf(dst)
}

// Walk lists every indexable .md file, excluding reserved directories.
func (v *Vault) Walk() ([]string, error) {
	var out []string
	err := WalkTree(v.Root, v.Follow, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree must not abort the whole walk
		}
		if d.IsDir() {
			for _, r := range ReservedDirs {
				if d.Name() == r {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := v.RelOf(path)
		if err != nil {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return out, err
}

// WalkDir lists .md files under one subdirectory of the vault, including
// reserved dirs like templates/ — those are excluded from the note graph but
// still readable by the surfaces that own them.
func (v *Vault) WalkDir(sub string) ([]string, error) {
	root := filepath.Join(v.Root, filepath.Clean(sub))
	if !strings.HasPrefix(root, v.Root+string(filepath.Separator)) {
		return nil, fmt.Errorf("%w: %q escapes vault", ErrVault, sub)
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		if rel, err := v.RelOf(path); err == nil {
			out = append(out, rel)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return out, err
}

// Stat reports a note's modification time without reading it.
//
// A startup sync over a large vault is dominated by what it reads: opening and
// hashing every file, then re-embedding it, costs orders of magnitude more than
// asking the filesystem when it changed. Stat is how the sync decides which
// notes it needs to look at at all.
func (v *Vault) Stat(rel string) (mtime float64, size int64, err error) {
	p, err := v.SafePath(rel)
	if err != nil {
		return 0, 0, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return 0, 0, err
	}
	return float64(info.ModTime().UnixNano()) / 1e9, info.Size(), nil
}
