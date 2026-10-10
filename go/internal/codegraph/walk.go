package codegraph

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// DefaultMaxFileBytes is the largest source file the index will read. A
// generated bundle or a checked-in data file past this is skipped, and counted
// as skipped, rather than parsed in full.
const DefaultMaxFileBytes = 256 << 10

// skipDirs are directories that never hold source the repository owns:
// version control, vendored or installed dependencies, and build output.
// Dot-directories are skipped too, which covers .git, .venv and .grimoire.
var skipDirs = map[string]bool{
	"vendor": true, "node_modules": true, "dist": true,
	"__pycache__": true, "venv": true,
}

// ignoreRules is the root .gitignore, read approximately. It supports the
// common forms: a bare name (matches at any depth), a slash-anchored path, a
// trailing slash for directories only, and * / ? globs. Negation (!) is not
// supported, and nested .gitignore files are not read. An approximation is
// the point: the index should skip what a developer would skip, not be a
// faithful git implementation.
type ignoreRules struct {
	patterns []ignorePattern
}

type ignorePattern struct {
	glob     string
	anchored bool
	dirOnly  bool
}

func loadIgnore(root string) ignoreRules {
	var r ignoreRules
	b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return r
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		p := ignorePattern{}
		if strings.HasSuffix(line, "/") {
			p.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		if strings.HasPrefix(line, "/") || strings.Contains(line, "/") {
			p.anchored = true
			line = strings.TrimPrefix(line, "/")
		}
		if line == "" {
			continue
		}
		p.glob = line
		r.patterns = append(r.patterns, p)
	}
	return r
}

// ignored reports whether a slash-separated path relative to the root is
// excluded. isDir says whether rel itself is a directory; every ancestor of
// rel is a directory by definition, so a directory pattern can match them.
func (r ignoreRules) ignored(rel string, isDir bool) bool {
	if len(r.patterns) == 0 {
		return false
	}
	parts := strings.Split(rel, "/")
	for depth := 1; depth <= len(parts); depth++ {
		candidate := strings.Join(parts[:depth], "/")
		candidateIsDir := depth < len(parts) || isDir
		for _, p := range r.patterns {
			if p.dirOnly && !candidateIsDir {
				continue
			}
			if p.anchored {
				if ok, _ := path.Match(p.glob, candidate); ok {
					return true
				}
				continue
			}
			if ok, _ := path.Match(p.glob, parts[depth-1]); ok {
				return true
			}
		}
	}
	return false
}

// walkResult is one source file found under a root.
type walkResult struct {
	rel  string // slash-separated, relative to the root
	abs  string
	lang string
}

// walkSources lists the source files under root that the index should read.
// Symlinks are not followed, skipped directories are not entered, and files
// over maxBytes are counted in tooLarge rather than returned.
func walkSources(root string, maxBytes int64) (files []walkResult, tooLarge int, err error) {
	rules := loadIgnore(root)
	err = filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is skipped, not fatal: one permission
			// error should not stop the rest of the repository being indexed.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relPath, err := filepath.Rel(root, abs)
		if err != nil || relPath == "." {
			return nil
		}
		rel := filepath.ToSlash(relPath)
		name := d.Name()
		if d.IsDir() {
			if skipDirs[name] || strings.HasPrefix(name, ".") || rules.ignored(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks, devices, sockets
		}
		lang := LangFor(name)
		if lang == "" || rules.ignored(rel, false) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > maxBytes {
			tooLarge++
			return nil
		}
		files = append(files, walkResult{rel: rel, abs: abs, lang: lang})
		return nil
	})
	return files, tooLarge, err
}
