package api

import (
	"regexp"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

// Keeping [[wiki-links]] pointing at a note that moved.
//
// Renaming used to move the file and reindex it, and stop there — so every link
// aimed at the old name broke, silently. Nothing errors: the links simply
// become unresolved, the graph loses the edges, and backlinks that were there
// yesterday are gone. It is the failure Obsidian's "update links on rename"
// exists to prevent, and the reason renaming a vault file from outside an app
// is normally a bad idea.
//
// The rewrite is deliberately narrow. It only touches links the INDEX says
// resolved to the note being moved, so it cannot rewrite an unrelated `[[Deploy]]`
// that happened to share a name, and it preserves whatever form each link was
// written in — path, title, alias, heading anchor, display text.

// linkTargetRE matches one wiki-link and splits it into target, anchor and
// display. `[[Folder/Note#Heading|shown as this]]` is all four parts at once,
// and a rewrite that keeps only the target silently rewrites what the reader
// sees.
var linkTargetRE = regexp.MustCompile(`\[\[([^\[\]|#]+?)((?:#[^\[\]|]*)?)((?:\|[^\[\]]*)?)\]\]`)

// relinkBody rewrites links in one note body from oldTargets to newTarget.
//
// oldTargets holds every spelling that used to reach the note — its path, its
// path without .md, its bare filename, its title and any aliases — because a
// vault written by a person contains all of them and rewriting only one form
// leaves the rest broken.
func relinkBody(body string, oldTargets map[string]bool, newTarget string) (string, int) {
	if len(oldTargets) == 0 {
		return body, 0
	}
	n := 0
	out := linkTargetRE.ReplaceAllStringFunc(body, func(m string) string {
		parts := linkTargetRE.FindStringSubmatch(m)
		if parts == nil {
			return m
		}
		target, anchor, display := parts[1], parts[2], parts[3]
		if !oldTargets[strings.ToLower(strings.TrimSpace(target))] {
			return m
		}
		n++
		// An embed keeps its bang; the regex matched only from "[[" so the "!"
		// is outside the match and survives untouched.
		//
		// If the link had no display text it was showing the old name, so one
		// is added to preserve what the reader sees. Renaming a note should not
		// silently change the words in a sentence somewhere else.
		if display == "" {
			display = "|" + strings.TrimSpace(target)
		}
		return "[[" + newTarget + anchor + display + "]]"
	})
	return out, n
}

// linkAliases returns every spelling that used to reach a note.
func linkAliases(rel, title string, aliases []string) map[string]bool {
	out := map[string]bool{}
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			out[s] = true
		}
	}
	add(rel)
	stem := strings.TrimSuffix(rel, ".md")
	add(stem)
	if i := strings.LastIndex(stem, "/"); i >= 0 {
		add(stem[i+1:]) // the bare filename, which is how most links are written
	}
	add(title)
	for _, a := range aliases {
		add(a)
	}
	return out
}

// inboundLinkSources lists the notes whose links the index resolved to rel.
//
// Read from the links table rather than by searching bodies for the name: the
// index already did the resolution, so this cannot catch an unrelated
// `[[Deploy]]` in another folder that happens to share a title.
func (s *Server) inboundLinkSources(rel string) []string {
	rows, err := s.Index.DB.Query("SELECT DISTINCT src FROM links WHERE dst=?", rel)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var src string
		if err := rows.Scan(&src); err != nil {
			return out
		}
		out = append(out, src)
	}
	return out
}

// linkNamesFor returns every spelling that reached this note.
func (s *Server) linkNamesFor(rel string) map[string]bool {
	note, err := s.Vault.Read(rel)
	if err != nil {
		return linkAliases(rel, "", nil)
	}
	var aliases []string
	if note.Frontmatter != nil {
		if v, ok := note.Frontmatter.Get("aliases"); ok {
			switch t := v.(type) {
			case string:
				aliases = append(aliases, t)
			case []markdown.Value:
				for _, a := range t {
					if str, ok := a.(string); ok {
						aliases = append(aliases, str)
					}
				}
			}
		}
	}
	return linkAliases(rel, note.Title, aliases)
}

// relinkInbound rewrites each source note and returns how many were changed.
//
// A failure on one note is logged into the count rather than aborting: a rename
// that half-succeeds and then errors leaves the vault in a worse state than one
// that repoints what it can and reports the number.
func (s *Server) relinkInbound(sources []string, oldNames map[string]bool, newTarget string) int {
	changed := 0
	for _, src := range sources {
		note, err := s.Vault.Read(src)
		if err != nil {
			continue
		}
		body, n := relinkBody(note.Body, oldNames, newTarget)
		if n == 0 {
			continue
		}
		if _, err := s.Vault.Write(src, body, note.Frontmatter); err != nil {
			continue
		}
		if _, err := s.Index.Upsert(src); err != nil {
			continue
		}
		changed++
	}
	return changed
}
