package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/agentprofile"
	"github.com/JeremiahM37/grimoire/go/internal/memstore"
	"github.com/JeremiahM37/grimoire/go/internal/settings"
)

// `grimoire agent install --link-memory` points an agent's own memory at the
// canonical store, using what the profile's memory fields say: directory
// locations (memory.dir, memory.glob matches) become symlinks, instruction
// files (memory.files) get the managed block. It is the same operation as
// `grimoire memory link`, driven from the profile instead of typed paths.

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// linkProfileMemory links every memory location the profile names. It never
// stops at the first problem: each location reports its own outcome.
func linkProfileMemory(e *env, p *agentprofile.Profile, store string, merge, dry bool) ([]string, error) {
	var out []string
	var firstErr error
	note := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }
	prefix := ""
	if dry {
		prefix = "(dry run) "
	}
	links := e.loadLinks()
	record := func(kind, path string) {
		if dry {
			return
		}
		kept := links[:0]
		for _, l := range links {
			if !(l.Agent == p.Name && l.Path == path) {
				kept = append(kept, l)
			}
		}
		links = append(kept, memoryLink{Agent: p.Name, Kind: kind, Path: path})
	}

	dirs := p.MemoryDirs()
	if len(dirs) == 0 && len(p.Memory.Files) == 0 {
		note("memory link: this profile names no memory location (set memory.dir, memory.glob or memory.files)")
		return out, nil
	}
	for _, d := range dirs {
		if sameDir(d, store) {
			if fi, err := os.Lstat(d); err == nil && fi.Mode()&os.ModeSymlink == 0 {
				note("memory link: %s is the store itself", d)
			} else {
				note("memory link: %s already points at the store", d)
				record(memstore.ShapeDir, d)
			}
			continue
		}
		res, err := memstore.Link(memstore.ShapeDir, d, store, memstore.LinkOptions{Agent: p.Name, Merge: merge, DryRun: dry})
		if err != nil {
			note("memory link: %s: %v", d, err)
			if firstErr == nil {
				firstErr = fmt.Errorf("could not link %s", d)
			}
			continue
		}
		note("memory link: %s%s %s", prefix, res.Action, d)
		if res.Backup != "" {
			note("  backup: %s", res.Backup)
		}
		record(memstore.ShapeDir, d)
	}
	if len(p.Memory.Files) > 0 {
		if _, err := os.Stat(store); err != nil {
			note("memory link: the store %s does not exist yet; files are not linked", store)
			if firstErr == nil {
				firstErr = fmt.Errorf("store %s missing", store)
			}
		} else {
			block := e.blockText(store, 6000)
			for _, f := range p.Memory.Files {
				if !dry {
					if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
						note("memory link: %s: %v", f, err)
						continue
					}
				}
				res, err := memstore.Link(memstore.ShapeFile, f, store, memstore.LinkOptions{Agent: p.Name, DryRun: dry, Block: block})
				if err != nil {
					note("memory link: %s: %v", f, err)
					if firstErr == nil {
						firstErr = fmt.Errorf("could not link %s", f)
					}
					continue
				}
				note("memory link: %s%s %s", prefix, res.Action, f)
				record(memstore.ShapeFile, f)
			}
		}
	}
	if !dry {
		if err := e.saveLinks(links); err != nil {
			return out, fmt.Errorf("linked, but could not record it: %w", err)
		}
	}
	return out, firstErr
}

// profileMemoryStatus describes the link state of a profile's memory
// locations without opening the vault database (agent status must stay
// read-only and cheap). The store is read from the same setting the memory
// commands use.
func profileMemoryStatus(p *agentprofile.Profile) []string {
	if len(p.MemoryDirs()) == 0 && len(p.Memory.Files) == 0 {
		return []string{"memory link: no memory location in this profile"}
	}
	vaultPath := envOr("GRIMOIRE_VAULT", filepath.Join(os.Getenv("HOME"), "notes"))
	gdir := filepath.Join(vaultPath, ".grimoire")
	st := settings.New(gdir)
	store := st.Get("memory_canonical_dir")
	if store == "" {
		store = st.Get("dream_canonical_memory")
	}
	if store == "" {
		store = filepath.Join(os.Getenv("HOME"), ".grimoire", "memory")
	}
	store = expandPath(store)
	var out []string
	counts := map[string]int{}
	var odd []string
	for _, d := range p.MemoryDirs() {
		s := memstore.Status(memstore.ShapeDir, d, store, "")
		if fi, err := os.Lstat(d); err == nil && fi.Mode()&os.ModeSymlink == 0 && sameDir(d, store) {
			s.State = "store"
		}
		counts[s.State]++
		if s.State != "linked" && s.State != "store" {
			odd = append(odd, fmt.Sprintf("%s (%s)", d, s.State))
		}
	}
	if n := len(p.MemoryDirs()); n > 0 {
		var parts []string
		for k, v := range counts {
			parts = append(parts, fmt.Sprintf("%d %s", v, k))
		}
		sort.Strings(parts)
		out = append(out, fmt.Sprintf("memory link: %d dir(s) -> %s: %s", n, store, strings.Join(parts, ", ")))
		for i, o := range odd {
			if i == 3 {
				out = append(out, fmt.Sprintf("  ... and %d more not linked", len(odd)-3))
				break
			}
			out = append(out, "  "+o)
		}
	}
	for _, f := range p.Memory.Files {
		s := memstore.Status(memstore.ShapeFile, f, store, "")
		out = append(out, fmt.Sprintf("memory link: %s %s", s.State, f))
	}
	return out
}
