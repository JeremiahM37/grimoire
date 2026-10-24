package api

import (
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Less noise in explicit search.
//
// The live vault is four fifths pulled mail and drive files, and both search
// surfaces ranked them as if they were the owner's notes: a thousand-page
// textbook from Drive was the top hit for "Tailscale", a sale email for the
// router's name, and a project's own page lost to the résumés that mention
// every project. Three cheap rules fix most of it, measured on a copy of the
// vault (docs/lab/README.md, "Search noise"):
//
//   - the note a query names comes first: its title, file name or folder is
//     the query;
//   - the owner's notes come before pulled content, unless the query asks for
//     mail or files by name;
//   - indexes and very long documents, which match everything a little, give
//     way to the notes they point at.
//
// It runs on what a person or agent asks for (/api/search, /api/retrieve). It
// does not run on the context hook's path, so it does not change what memory
// is injected, which the holdout is measuring.

type rankedDoc struct {
	path      string
	untrusted bool
}

// heldHub is a hub waiting to be placed, and where it would have been.
type heldHub struct {
	doc rankedDoc
	at  int
}

// sourceWords mark a query that is asking for pulled content.
var sourceWords = regexp.MustCompile(`(?i)\b(gmail|e-?mails?|mail|inbox|sender|drive|calendar|events?|meeting|invite|attachment|newsletter|connector)\b`)

// pulledAfter is how many of the owner's results come before the first
// pulled one.
const pulledAfter = 5

// hubAfter is how many places an index or a very long document yields.
const hubAfter = 3

// hubBytes is the size past which a document matches almost any query.
const hubBytes = 60_000

func normName(s string) string {
	s = strings.ToLower(strings.TrimSuffix(s, ".md"))
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return r == '-' || r == '_' || r == ' ' || r == '.' || r == '/'
	}), " ")
}

// titleCache maps a normalised name to the notes it names, rebuilt when the
// index changes.
type titleCache struct {
	mu     sync.Mutex
	rev    int64
	byName map[string][]string
	size   map[string]int
}

func (s *Server) namesIndex() (map[string][]string, map[string]int) {
	c := &s.titles
	c.mu.Lock()
	defer c.mu.Unlock()
	rev := s.Index.Rev()
	if c.byName != nil && c.rev == rev {
		return c.byName, c.size
	}
	byName, size := map[string][]string{}, map[string]int{}
	rows, err := s.Index.DB.Query("SELECT path, COALESCE(title,''), length(body) FROM notes WHERE COALESCE(untrusted,0)=0")
	if err != nil {
		return c.byName, c.size
	}
	for rows.Next() {
		var p, title string
		var n int
		if rows.Scan(&p, &title, &n) != nil {
			continue
		}
		size[p] = n
		names := map[string]bool{normName(path.Base(p)): true}
		if title != "" {
			names[normName(title)] = true
		}
		// Projects/lectern/lectern.md is named by its folder too.
		if dir := path.Base(path.Dir(p)); dir != "." && normName(dir) == normName(path.Base(p)) {
			names[normName(dir)] = true
		}
		for n := range names {
			if n != "" {
				byName[n] = append(byName[n], p)
			}
		}
	}
	rows.Close()
	for _, ps := range byName {
		sort.Strings(ps)
	}
	c.byName, c.size, c.rev = byName, size, rev
	return byName, size
}

// named returns the notes a query names outright, best first: a page in a
// folder of the same name (a project's own page) before any other match.
func (s *Server) named(query string) []string {
	byName, _ := s.namesIndex()
	ps := append([]string(nil), byName[normName(query)]...)
	sort.SliceStable(ps, func(i, j int) bool {
		return ownPage(ps[i]) && !ownPage(ps[j])
	})
	return ps
}

func ownPage(p string) bool {
	return normName(path.Base(path.Dir(p))) == normName(path.Base(p))
}

func isHub(p string, size map[string]int) bool {
	base := path.Base(p)
	return base == "MEMORY.md" || strings.HasPrefix(base, "MEMORY-") || size[p] > hubBytes
}

// rerankDocs orders documents for an explicit search. docs is the ranking as
// retrieval produced it, one entry per document; the result is a permutation
// of it, with any note the query names put first (fetched by the caller when
// it was not among docs).
func (s *Server) rerankDocs(query string, docs []rankedDoc) []string {
	_, size := s.namesIndex()
	wantPulled := sourceWords.MatchString(query)
	var named []string
	namedSet := map[string]bool{}
	for _, p := range s.named(query) {
		named = append(named, p)
		namedSet[p] = true
	}
	var owned, pulled []rankedDoc
	for _, d := range docs {
		if namedSet[d.path] {
			continue
		}
		if d.untrusted && !wantPulled {
			pulled = append(pulled, d)
		} else {
			owned = append(owned, d)
		}
	}
	// Hubs yield a few places among the owner's results.
	var ordered []rankedDoc
	var held []heldHub
	for _, d := range owned {
		for len(held) > 0 && len(ordered)-held[0].at >= hubAfter {
			ordered = append(ordered, held[0].doc)
			held = held[1:]
		}
		if isHub(d.path, size) {
			held = append(held, heldHub{d, len(ordered)})
			continue
		}
		ordered = append(ordered, d)
	}
	for _, h := range held {
		ordered = append(ordered, h.doc)
	}
	// Pulled content joins after the first few of the owner's results, then
	// takes its original place among the rest.
	rank := map[string]int{}
	for i, d := range docs {
		rank[d.path] = i
	}
	out := append([]string(nil), named...)
	i, j := 0, 0
	for i < len(ordered) || j < len(pulled) {
		switch {
		case j >= len(pulled):
			out = append(out, ordered[i].path)
			i++
		case i >= len(ordered):
			out = append(out, pulled[j].path)
			j++
		case i < pulledAfter || rank[ordered[i].path] < rank[pulled[j].path]:
			out = append(out, ordered[i].path)
			i++
		default:
			out = append(out, pulled[j].path)
			j++
		}
	}
	return out
}

func rerankOff(q string) bool { return q == "0" || strings.EqualFold(q, "false") }
