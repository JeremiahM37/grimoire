package api

import (
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
)

// Experiment loaded (docs/MEMORY_USE.md): notes the agent already has in its
// context are not injected again. Most injected items were agent memory files
// whose one-line summaries every session loads from MEMORY.md, and project
// notes that a session brief already carries, so repeating them costs tokens
// and teaches the agent to skip the block. The paths come from the
// context_loaded_paths setting, plus loaded= request parameters that a client
// uses to declare what it loaded. Off, none of this runs.

const loadedFlag = "loaded"

// maxLoadedParams bounds the loaded= parameters one request may declare.
const maxLoadedParams = 32

// validContextPath reports whether a path or directory prefix is acceptable
// as a scope. Scoped context paths and loaded paths share this rule.
func validContextPath(prefix string) bool {
	cleaned := path.Clean(strings.TrimSuffix(prefix, "/"))
	return !(len(prefix) > 512 || cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") ||
		strings.Contains(prefix, "\\") || strings.ContainsRune(prefix, 0) || cleaned != strings.TrimSuffix(prefix, "/"))
}

// loadedScope returns the loaded-path patterns for a request and whether the
// flag is on. With the flag off the loaded= parameters are ignored entirely,
// so they are neither validated nor applied. A bad parameter with the flag on
// is a bad request, and a bad setting entry is skipped rather than failing
// every request.
func (s *Server) loadedScope(r *http.Request) ([]string, bool, error) {
	if !exp.On(loadedFlag) {
		return nil, false, nil
	}
	params := r.URL.Query()["loaded"]
	if len(params) > maxLoadedParams {
		return nil, true, fmt.Errorf("loaded takes at most %d paths", maxLoadedParams)
	}
	var pats []string
	for _, p := range params {
		if !validContextPath(p) {
			return nil, true, fmt.Errorf("invalid loaded path")
		}
		pats = append(pats, strings.TrimSuffix(p, "/"))
	}
	for _, p := range strings.Split(s.setting("context_loaded_paths"), ",") {
		p = strings.TrimSpace(p)
		if p != "" && validContextPath(p) {
			pats = append(pats, strings.TrimSuffix(p, "/"))
		}
	}
	return pats, true, nil
}

// loadedMatch reports whether a note path sits under one of the patterns. A
// pattern matches a path when it matches the path itself or any directory
// above it, so "Projects/" covers everything below Projects and "Projects/*"
// covers every project folder. Literal patterns are exact matches.
func loadedMatch(notePath string, pats []string) bool {
	segs := strings.Split(notePath, "/")
	for _, p := range pats {
		for i := range segs {
			if ok, _ := path.Match(p, strings.Join(segs[:i+1], "/")); ok {
				return true
			}
		}
	}
	return false
}

// dropLoaded removes note items the agent already has. A memory item (one
// with an id) is never dropped here, and neither is a note that a cue fired
// on by name at the action stage: that is the situation the cue was written
// for, so the agent is shown it again. It returns the kept items and how many
// were dropped.
func dropLoaded(items []contextItem, pats []string, action bool) ([]contextItem, int) {
	if len(pats) == 0 {
		return items, 0
	}
	kept := make([]contextItem, 0, len(items))
	skipped := 0
	for _, it := range items {
		if it.ID == "" && loadedMatch(it.Path, pats) && !(action && it.cueFired) {
			skipped++
			continue
		}
		kept = append(kept, it)
	}
	return kept, skipped
}
