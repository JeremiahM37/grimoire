// Package skillexport turns procedure memories into native agent skills: a
// folder with a SKILL.md (name and description in the frontmatter, the
// procedure as the body) and, when the procedure holds shell blocks, a
// scripts/ folder. Claude Code, Codex and other agents that load skills from a
// directory then get the procedure on demand, through their own mechanism.
//
// The skills directory is the agent's, not ours. A skill is only ever created,
// updated or removed by this package when it carries the marker file it wrote
// itself; a folder without one is never touched, a managed skill whose files
// were edited by hand is left alone unless forced, and removal deletes only
// the files the marker lists.
package skillexport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/JeremiahM37/grimoire/go/internal/memstore"
)

// MarkerFile is written inside every skill folder this package creates.
const MarkerFile = ".grimoire-managed.json"

const markerTool = "grimoire skills export"

// Marker is the content of MarkerFile.
type Marker struct {
	Tool       string    `json:"tool"`
	Agent      string    `json:"agent"`
	Source     string    `json:"source"` // the memory note's file name
	Hash       string    `json:"hash"`   // of the generated files, to notice hand edits
	Files      []string  `json:"files"`  // relative to the skill folder, marker excluded
	ExportedAt time.Time `json:"exported_at"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
}

// Options shapes Export and Remove.
type Options struct {
	Agent  string
	Dir    string // the agent's skills directory
	DryRun bool
	Force  bool // rewrite a managed skill even if it was edited by hand
	Now    func() time.Time
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

// Result is one skill's outcome.
type Result struct {
	Name   string
	Source string
	Action string // created, updated, unchanged, skipped, removed, orphan
	Detail string
}

// Skill is a procedure rendered as files.
type Skill struct {
	Name        string
	Description string
	Files       map[string]string // relative path -> content
}

var (
	slugBad  = regexp.MustCompile(`[^a-z0-9]+`)
	fenceRE  = regexp.MustCompile("(?s)```[ \\t]*(bash|sh|shell|zsh|console)[ \\t]*\\n(.*?)\\n?```")
	h1RE     = regexp.MustCompile(`(?m)\A\s*#\s+.*\n`)
	cueKeys  = []string{"cues", "metadata.cues", "when", "metadata.when", "applies_when", "metadata.applies_when"}
	nameKeys = []string{"name", "title"}
)

// Slug makes a skill folder name: lowercase letters, digits and hyphens.
func Slug(s string) string {
	s = slugBad.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 64 {
		s = strings.Trim(s[:64], "-")
	}
	return s
}

// Cues returns the situations a note says it applies in.
func Cues(n memstore.Note) []string {
	for _, k := range cueKeys {
		v := strings.TrimSpace(n.Fields[k])
		if v == "" {
			continue
		}
		v = strings.Trim(v, "[]")
		var out []string
		for _, p := range strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == '|' || r == '\n' || r == ',' && !strings.Contains(v, ";") }) {
			if p = strings.Trim(strings.TrimSpace(p), `"'`); p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// Render builds the skill for a procedure note.
func Render(n memstore.Note) (Skill, error) {
	title := n.Title
	for _, k := range nameKeys {
		if v := strings.TrimSpace(n.Fields[k]); v != "" {
			title = v
			break
		}
	}
	name := Slug(title)
	if name == "" {
		return Skill{}, errors.New("no usable name")
	}
	desc := describe(n, title)
	body := strings.TrimSpace(h1RE.ReplaceAllString(n.Body, ""))
	sk := Skill{Name: name, Description: desc, Files: map[string]string{}}

	var md strings.Builder
	md.WriteString("---\nname: " + name + "\ndescription: " + yamlLine(desc) + "\n---\n\n")
	md.WriteString("# " + oneLine(title) + "\n\n")
	md.WriteString(body + "\n")
	if blocks := fenceRE.FindAllStringSubmatch(n.Body, -1); len(blocks) > 0 {
		var sh strings.Builder
		sh.WriteString("#!/usr/bin/env bash\n# Generated from the Grimoire procedure memory \"" + oneLine(title) + "\".\n" +
			"# Review before running: the steps are a record of what worked once, on one machine.\nset -euo pipefail\n")
		for i, b := range blocks {
			fmt.Fprintf(&sh, "\n# step %d\n%s\n", i+1, strings.TrimSpace(b[2]))
		}
		sk.Files["scripts/steps.sh"] = sh.String()
		md.WriteString("\nThe shell steps above are also collected in `scripts/steps.sh` (run with `bash`; read it first).\n")
	}
	md.WriteString("\n---\nSource: Grimoire procedure memory `" + n.File + "`. Edit the memory, then re-run `grimoire skills export`; changes made here are not synced back.\n")
	sk.Files["SKILL.md"] = md.String()
	return sk, nil
}

// describe builds the skill description: what it is, then when it applies.
func describe(n memstore.Note, title string) string {
	var d string
	if cues := Cues(n); len(cues) > 0 {
		d = strings.TrimRight(oneLine(title), ".") + ". Use when " + strings.Join(cues, "; or when ") + "."
	} else if n.Description != "" {
		d = oneLine(n.Description)
		if !strings.HasSuffix(d, ".") {
			d += "."
		}
		d += " Use when this task comes up."
	} else {
		d = oneLine(title) + "."
	}
	if r := []rune(d); len(r) > 600 {
		d = string(r[:599]) + "…"
	}
	return d
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// yamlLine quotes a one-line value when YAML would misread it.
func yamlLine(s string) string {
	if strings.ContainsAny(s, ":#{}[],&*!|>'\"%@`") || strings.TrimSpace(s) != s || strings.HasPrefix(s, "-") {
		raw, _ := json.Marshal(s)
		return string(raw)
	}
	return s
}

func hashFiles(files map[string]string) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n + "\x00" + files[n] + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func readMarker(dir string) (Marker, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, MarkerFile))
	if err != nil {
		return Marker{}, false
	}
	var m Marker
	if json.Unmarshal(raw, &m) != nil || m.Tool != markerTool {
		return Marker{}, false
	}
	return m, true
}

func diskHash(dir string, files []string) string {
	m := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return "missing"
		}
		m[f] = string(b)
	}
	return hashFiles(m)
}

// Export writes every procedure in notes as a skill under opt.Dir.
func Export(notes []memstore.Note, opt Options) ([]Result, error) {
	if opt.Dir == "" {
		return nil, errors.New("this agent's profile has no skills_dir; set skills_dir in its profile or pass --dest")
	}
	var out []Result
	wanted := map[string]bool{}
	for _, n := range notes {
		if n.Kind != memstore.KindProcedure {
			continue
		}
		sk, err := Render(n)
		if err != nil {
			out = append(out, Result{Source: n.File, Action: "skipped", Detail: err.Error()})
			continue
		}
		wanted[sk.Name] = true
		if memstoreSecret(n) {
			out = append(out, Result{Name: sk.Name, Source: n.File, Action: "skipped", Detail: "the memory looks like it holds a secret; not exported"})
			continue
		}
		res := exportOne(sk, n, opt)
		out = append(out, res)
	}
	// Managed skills whose procedure is gone: reported, never deleted here.
	for _, m := range ListManaged(opt.Dir) {
		if m.Marker.Agent == opt.Agent && !wanted[m.Name] {
			out = append(out, Result{Name: m.Name, Source: m.Marker.Source, Action: "orphan",
				Detail: "its procedure memory is gone or is no longer a procedure; remove with --remove"})
		}
	}
	return out, nil
}

func memstoreSecret(n memstore.Note) bool {
	for _, w := range memstore.Lint(n.Kind, n.Body) {
		if w.Code == "looks_like_secret" {
			return true
		}
	}
	return false
}

func exportOne(sk Skill, n memstore.Note, opt Options) Result {
	dir := filepath.Join(opt.Dir, sk.Name)
	res := Result{Name: sk.Name, Source: n.File}
	files := make([]string, 0, len(sk.Files))
	for f := range sk.Files {
		files = append(files, f)
	}
	sort.Strings(files)
	hash := hashFiles(sk.Files)

	fi, err := os.Lstat(dir)
	exists := err == nil
	if exists && !fi.IsDir() {
		res.Action, res.Detail = "skipped", "a file with this name exists in the skills directory"
		return res
	}
	var cur Marker
	managed := false
	if exists {
		if cur, managed = readMarker(dir); !managed {
			res.Action, res.Detail = "skipped", "a skill with this name exists that Grimoire did not write; left alone"
			return res
		}
		if cur.Agent != opt.Agent {
			res.Action, res.Detail = "skipped", "managed for another agent ("+cur.Agent+")"
			return res
		}
		if cur.Hash == hash && diskHash(dir, files) == hash {
			res.Action = "unchanged"
			return res
		}
		if !opt.Force && diskHash(dir, cur.Files) != cur.Hash {
			res.Action, res.Detail = "skipped", "edited by hand since it was exported; re-run with --force to overwrite"
			return res
		}
		res.Action = "updated"
	} else {
		res.Action = "created"
	}
	if opt.DryRun {
		return res
	}
	now := opt.now()
	mk := Marker{Tool: markerTool, Agent: opt.Agent, Source: n.File, Hash: hash, Files: files, ExportedAt: now}
	if managed {
		mk.ExportedAt, mk.UpdatedAt = cur.ExportedAt, now
		for _, old := range cur.Files { // a file the new version no longer has
			if _, keep := sk.Files[old]; !keep {
				_ = os.Remove(filepath.Join(dir, old))
			}
		}
	}
	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			res.Action, res.Detail = "skipped", err.Error()
			return res
		}
		if err := writeAtomic(p, []byte(sk.Files[f])); err != nil {
			res.Action, res.Detail = "skipped", err.Error()
			return res
		}
	}
	raw, _ := json.MarshalIndent(mk, "", "  ")
	if err := writeAtomic(filepath.Join(dir, MarkerFile), append(raw, '\n')); err != nil {
		res.Action, res.Detail = "skipped", err.Error()
	}
	return res
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".grimoire-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Managed is a skill folder this package wrote.
type Managed struct {
	Name   string
	Dir    string
	Marker Marker
}

// ListManaged finds the skill folders under dir that carry our marker.
func ListManaged(dir string) []Managed {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Managed
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if m, ok := readMarker(p); ok {
			out = append(out, Managed{Name: e.Name(), Dir: p, Marker: m})
		}
	}
	return out
}

// Remove deletes the skills this package wrote for opt.Agent: the files its
// marker lists, then the folder if nothing else is left in it. A folder
// without the marker is never touched, and neither is a file the marker does
// not list.
func Remove(opt Options) ([]Result, error) {
	if opt.Dir == "" {
		return nil, errors.New("this agent's profile has no skills_dir")
	}
	var out []Result
	for _, m := range ListManaged(opt.Dir) {
		if m.Marker.Agent != opt.Agent {
			continue
		}
		res := Result{Name: m.Name, Source: m.Marker.Source, Action: "removed"}
		if !opt.DryRun {
			for _, f := range m.Marker.Files {
				if !safeRel(f) {
					continue
				}
				_ = os.Remove(filepath.Join(m.Dir, f))
			}
			_ = os.Remove(filepath.Join(m.Dir, MarkerFile))
			pruneEmpty(m.Dir)
			if _, err := os.Stat(m.Dir); err == nil {
				res.Detail = "kept the folder: it holds files Grimoire did not write"
			}
		}
		out = append(out, res)
	}
	return out, nil
}

func safeRel(f string) bool {
	return f != "" && !filepath.IsAbs(f) && !strings.Contains(f, "..") && !strings.ContainsFunc(f, unicode.IsControl)
}

// pruneEmpty removes dir and any empty subfolders, bottom up, stopping at
// anything that holds a file.
func pruneEmpty(dir string) {
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // fails, harmlessly, when not empty
	}
}
