package memstore

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/JeremiahM37/grimoire/go/internal/dream/filemem"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
)

// Defaults match what Claude Code loads from MEMORY.md.
const (
	DefaultMaxLines = 200
	DefaultBudget   = 25 * 1024
	maxPointers     = 30
	ruleSummaryLen  = 220
)

// Rule is one standing rule that goes into the core as a single line.
type Rule struct {
	Text  string
	File  string // note file for a link/reference, "" for a stored fact
	Title string
	Score int
	When  time.Time
}

// CoreOptions shapes BuildCore.
type CoreOptions struct {
	Budget   int // bytes; 0 = DefaultBudget
	MaxLines int // 0 = DefaultMaxLines
	// Links renders rule lines as "- [Title](file.md) — rule", the shape of a
	// MEMORY.md index, and marks the document as generated. Without it lines
	// are plain text, for a managed block or an MCP instructions string.
	Links bool
	// Embed clusters by meaning. Nil falls back to the hashing embedder,
	// which still groups notes that share vocabulary.
	Embed func([]string) [][]float32
	// ExtraRules are rules that are not files (stored facts of kind rule).
	ExtraRules []Rule
	// Header replaces the default intro line.
	Header string
}

// Cluster is one topic of the non-rule notes.
type Cluster struct {
	Label string
	Query string
	Notes []string // file names
}

// Core is a built core.
type Core struct {
	Text       string
	Lines      int
	Bytes      int
	Rules      int // rules available
	RulesShown int
	Pointers   int
	Notes      int // notes covered by pointers
	Clusters   []Cluster
}

// BuildCore renders every rule as one line, most useful first, then one
// pointer line per topic cluster of everything else, within the line and byte
// budget. It never reads or writes the notes it summarises.
func BuildCore(notes []Note, opt CoreOptions) Core {
	if opt.Budget <= 0 {
		opt.Budget = DefaultBudget
	}
	if opt.MaxLines <= 0 {
		opt.MaxLines = DefaultMaxLines
	}

	var rules []Rule
	var rest []Note
	for _, n := range notes {
		if n.Kind == KindRule {
			rules = append(rules, Rule{Text: n.Summary(ruleSummaryLen), File: n.File,
				Title: n.Title, Score: n.Score(), When: n.MTime})
			continue
		}
		rest = append(rest, n)
	}
	rules = append(rules, opt.ExtraRules...)
	sort.SliceStable(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if !a.When.Equal(b.When) {
			return a.When.After(b.When)
		}
		return a.File+a.Text < b.File+b.Text
	})

	clusters := ClusterNotes(rest, opt.Embed)

	var head []string
	if opt.Links {
		head = append(head, filemem.GeneratedMarker)
	}
	intro := opt.Header
	if intro == "" {
		intro = "Memory core: standing rules, then topic pointers. Run recall '<query>' (or search_notes) for anything below; the notes hold the detail."
	}
	head = append(head, intro)

	// Pointers are reserved first, so a long rule list cannot push the way to
	// the rest of memory off the end.
	pointers := make([]string, 0, len(clusters))
	for _, c := range clusters {
		pointers = append(pointers, fmt.Sprintf("- %s: %d note%s — recall '%s'",
			c.Label, len(c.Notes), plural(len(c.Notes)), c.Query))
	}
	if len(pointers) > maxPointers {
		pointers = pointers[:maxPointers]
		clusters = clusters[:maxPointers]
	}

	used := func(lines []string) int {
		n := 0
		for _, l := range lines {
			n += len(l) + 1
		}
		return n
	}
	const overhead = 64 // blanks, section titles, the "more rules" line
	// Pointers get up to 55% of the budget and sit ahead of surplus rules,
	// smallest topics dropped first; a rule list that has outgrown the budget
	// must not hide the way to the rest of memory.
	for len(pointers) > 1 && (used(head)+used(pointers)+overhead > opt.Budget*55/100 ||
		len(head)+len(pointers)+5 > opt.MaxLines/2) {
		pointers = pointers[:len(pointers)-1]
		clusters = clusters[:len(clusters)-1]
	}
	fixed := len(head) + len(pointers) + 5
	byteFixed := used(head) + used(pointers) + overhead

	var ruleLines []string
	shown := 0
	for _, r := range rules {
		line := ruleLine(r, opt.Links)
		if fixed+len(ruleLines)+1 > opt.MaxLines ||
			byteFixed+used(ruleLines)+len(line)+1 > opt.Budget {
			break
		}
		ruleLines = append(ruleLines, line)
		shown++
	}
	if shown < len(rules) {
		ruleLines = append(ruleLines, fmt.Sprintf("- %d more rule%s not shown — recall 'standing rules'",
			len(rules)-shown, plural(len(rules)-shown)))
	}

	var out []string
	out = append(out, head...)
	if len(ruleLines) > 0 {
		out = append(out, "", "## Rules")
		out = append(out, ruleLines...)
	}
	if len(pointers) > 0 {
		out = append(out, "", "## Topics")
		out = append(out, pointers...)
	}
	text := strings.Join(out, "\n") + "\n"
	covered := 0
	for _, c := range clusters {
		covered += len(c.Notes)
	}
	return Core{Text: text, Lines: len(out), Bytes: len(text), Rules: len(rules),
		RulesShown: shown, Pointers: len(pointers), Notes: covered, Clusters: clusters}
}

func ruleLine(r Rule, links bool) string {
	text := r.Text
	if text == "" {
		text = r.Title
	}
	if links && r.File != "" {
		return fmt.Sprintf("- [%s](%s) — %s", r.Title, r.File, text)
	}
	if r.File != "" {
		return fmt.Sprintf("- %s (%s)", text, strings.TrimSuffix(r.File, ".md"))
	}
	return "- " + text
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---- clustering ------------------------------------------------------------

// ClusterNotes groups notes into about sqrt(n) topics by spherical k-means
// over embeddings of "title. description", seeded farthest-first so the same
// notes always give the same clusters.
func ClusterNotes(notes []Note, embedFn func([]string) [][]float32) []Cluster {
	n := len(notes)
	if n == 0 {
		return nil
	}
	texts := make([]string, n)
	for i, note := range notes {
		texts[i] = note.Title + ". " + note.Description + " " + clip(note.Body, 200)
	}
	var vecs [][]float32
	if embedFn != nil {
		vecs = embedFn(texts)
	}
	if len(vecs) != n {
		vecs = embed.Hash{}.Embed(texts)
	}
	unit := make([][]float64, n)
	for i, v := range vecs {
		unit[i] = normalize(v)
	}

	k := int(math.Round(math.Sqrt(float64(n))))
	if k < 1 {
		k = 1
	}
	if k > maxPointers {
		k = maxPointers
	}
	if k > n {
		k = n
	}
	assign := kmeans(unit, k)

	groups := map[int][]int{}
	for i, c := range assign {
		groups[c] = append(groups[c], i)
	}
	docTerms := make([]map[string]bool, n)
	df := map[string]int{}
	for i, note := range notes {
		set := map[string]bool{}
		for _, t := range topicTerms(note.Title + " " + note.Description) {
			set[t] = true
		}
		docTerms[i] = set
		for t := range set {
			df[t]++
		}
	}

	var out []Cluster
	for _, members := range groups {
		out = append(out, labelCluster(notes, members, docTerms, df, n))
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Notes) != len(out[j].Notes) {
			return len(out[i].Notes) > len(out[j].Notes)
		}
		return out[i].Label < out[j].Label
	})
	return out
}

func labelCluster(notes []Note, members []int, docTerms []map[string]bool, df map[string]int, total int) Cluster {
	sort.Ints(members)
	c := Cluster{}
	for _, i := range members {
		c.Notes = append(c.Notes, notes[i].File)
	}
	cf := map[string]int{}
	for _, i := range members {
		for t := range docTerms[i] {
			cf[t]++
		}
	}
	type scored struct {
		term  string
		score float64
	}
	var terms []scored
	for t, f := range cf {
		if f < 2 && len(members) > 1 {
			continue
		}
		terms = append(terms, scored{t, float64(f) * math.Log(1+float64(total)/float64(df[t]))})
	}
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].score != terms[j].score {
			return terms[i].score > terms[j].score
		}
		return terms[i].term < terms[j].term
	})
	if len(terms) == 0 {
		t := notes[members[0]].Title
		c.Label, c.Query = clip(t, 60), clip(t, 60)
		return c
	}
	var top []string
	for i := 0; i < len(terms) && i < 4; i++ {
		top = append(top, terms[i].term)
	}
	c.Label = strings.Join(top[:min(3, len(top))], ", ")
	c.Query = strings.Join(top, " ")
	return c
}

var stopTerms = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`the a an and or of to in on for with is are was were be been by at as it its this that
	these those from into than then not no but if when while after before over under via per use used using uses new old
	has have had will would should can could may might do does did done just also only more most all any each one two
	not never always how now real live call agent agents memory note notes file files project feedback reference user session sessions`) {
		m[w] = true
	}
	return m
}()

func topicTerms(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-')
	}) {
		w = strings.Trim(w, "-")
		if utf8.RuneCountInString(w) < 3 || stopTerms[w] || (w[0] >= '0' && w[0] <= '9') {
			continue
		}
		out = append(out, w)
	}
	return out
}

func normalize(v []float32) []float64 {
	out := make([]float64, len(v))
	var sum float64
	for i, x := range v {
		out[i] = float64(x)
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return out
	}
	inv := 1 / math.Sqrt(sum)
	for i := range out {
		out[i] *= inv
	}
	return out
}

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// kmeans returns a cluster index per point. Seeds are farthest-first from
// point 0, so the result depends only on the input order.
func kmeans(pts [][]float64, k int) []int {
	n := len(pts)
	assign := make([]int, n)
	if k <= 1 {
		return assign
	}
	seeds := []int{0}
	best := make([]float64, n) // similarity to nearest seed
	for i := range best {
		best[i] = dot(pts[i], pts[0])
	}
	for len(seeds) < k {
		far, farSim := -1, 2.0
		for i := 0; i < n; i++ {
			if best[i] < farSim {
				far, farSim = i, best[i]
			}
		}
		if far < 0 {
			break
		}
		seeds = append(seeds, far)
		for i := 0; i < n; i++ {
			if s := dot(pts[i], pts[far]); s > best[i] {
				best[i] = s
			}
		}
	}
	cent := make([][]float64, len(seeds))
	for i, s := range seeds {
		cent[i] = append([]float64(nil), pts[s]...)
	}
	for iter := 0; iter < 15; iter++ {
		changed := false
		for i, p := range pts {
			bi, bs := 0, math.Inf(-1)
			for c := range cent {
				if s := dot(p, cent[c]); s > bs {
					bi, bs = c, s
				}
			}
			if assign[i] != bi || iter == 0 {
				changed = changed || assign[i] != bi
				assign[i] = bi
			}
		}
		sums := make([][]float64, len(cent))
		for c := range sums {
			sums[c] = make([]float64, len(pts[0]))
		}
		for i, p := range pts {
			for d, x := range p {
				sums[assign[i]][d] += x
			}
		}
		for c := range cent {
			var norm float64
			for _, x := range sums[c] {
				norm += x * x
			}
			if norm > 0 {
				inv := 1 / math.Sqrt(norm)
				for d := range sums[c] {
					cent[c][d] = sums[c][d] * inv
				}
			}
		}
		if !changed && iter > 0 {
			break
		}
	}
	return assign
}
