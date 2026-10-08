package bank

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Entity resolution: deciding whether "Alice", "alice" and "Alice Chen" in
// three different conversations are one person.
//
// The rule is deliberately conservative, because a wrong merge is worse than a
// missed one: a missed merge leaves two entries for one person, which recall
// still finds through meaning and words; a wrong merge welds two people's
// facts together and every graph hop afterwards follows the weld. So a name
// that is not the same words needs corroboration beyond spelling to merge —
// shared company (the other entities it appears with) or recency — and names
// that carry different numbers ("Room 101", "Room 102") never merge at all.

// MaxEntityName bounds an entity name; anything longer is a sentence a model
// put in the wrong field.
const MaxEntityName = 512

var wsRE = regexp.MustCompile(`\s+`)

// NormalizeEntity collapses whitespace and trims; case is kept, because the
// first spelling seen is the one shown.
func NormalizeEntity(s string) string {
	s = strings.TrimSpace(wsRE.ReplaceAllString(s, " "))
	if utf8.RuneCountInString(s) > MaxEntityName {
		return ""
	}
	return s
}

// words splits a name into lowercase alphanumeric words.
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// trigrams is the padded word-trigram set two names are compared on: each
// word is padded with two spaces in front and one behind, so word starts
// weigh more than word ends — "Alex" and "Alexander" share their beginning.
func trigrams(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, w := range words(s) {
		r := []rune("  " + w + " ")
		for i := 0; i+3 <= len(r); i++ {
			out[string(r[i:i+3])] = struct{}{}
		}
	}
	return out
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// seqRatio is the Ratcliff/Obershelp similarity, 2·M/T, where M is the number
// of characters in the matching blocks found by repeatedly taking the longest
// common substring and recursing on either side of it.
func seqRatio(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra)+len(rb) == 0 {
		return 1
	}
	return 2 * float64(matchingChars(ra, rb)) / float64(len(ra)+len(rb))
}

func matchingChars(a, b []rune) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	// longest common substring, earliest in a then in b on ties
	bestI, bestJ, bestN := 0, 0, 0
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
				if cur[j] > bestN {
					bestN, bestI, bestJ = cur[j], i-cur[j], j-cur[j]
				}
			} else {
				cur[j] = 0
			}
		}
		prev, cur = cur, prev
		for j := range cur {
			cur[j] = 0
		}
	}
	if bestN == 0 {
		return 0
	}
	return bestN + matchingChars(a[:bestI], b[:bestJ]) + matchingChars(a[bestI+bestN:], b[bestJ+bestN:])
}

var digitRunRE = regexp.MustCompile(`\d+`)

func numbersIn(s string) map[string]bool {
	out := map[string]bool{}
	for _, n := range digitRunRE.FindAllString(s, -1) {
		n = strings.TrimLeft(n, "0")
		if n == "" {
			n = "0"
		}
		out[n] = true
	}
	return out
}

// tokensCompatible rejects pairs that look alike but cannot be one entity:
// names carrying different numbers, and multi-word names whose words do not
// line up. "Alice" and "Alice Chen" are compatible; "Alice Chen" and
// "Bob Chen" are not.
func tokensCompatible(a, b string) bool {
	na, nb := numbersIn(a), numbersIn(b)
	aHas, bHas := false, false
	for n := range na {
		if !nb[n] {
			aHas = true
		}
	}
	for n := range nb {
		if !na[n] {
			bHas = true
		}
	}
	if aHas && bHas {
		return false
	}
	wa, wb := words(a), words(b)
	if len(wa) <= 1 && len(wb) <= 1 {
		return true
	}
	short, long := wa, wb
	if len(short) > len(long) {
		short, long = long, short
	}
	for _, w := range short {
		ok := false
		for _, v := range long {
			if w == v || strings.HasPrefix(w, v) || strings.HasPrefix(v, w) || seqRatio(w, v) >= 0.6 {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// Resolution thresholds.
const (
	entityCandidateFloor = 0.15 // trigram prefilter
	entityMergeFloor     = 0.3  // a reused entity must at least look this alike
	entityAccept         = 0.6  // combined score needed to reuse
	entityBatchMerge     = 0.5  // two NEW names in one retain merge at this
	entityBatchMaxNames  = 250
)

// knownEntity is an entity already in the bank, as resolution sees it.
type knownEntity struct {
	Name     string
	lower    string
	tri      map[string]struct{}
	LastSeen time.Time
	// Cooc is the lowercase names this entity has appeared alongside.
	Cooc     map[string]bool
	Mentions int
}

// Mention is one entity name on one fact, waiting to be resolved.
type Mention struct {
	Text string
	// Nearby is the other entity names on the same fact.
	Nearby []string
	Event  time.Time
	// Literal names are reused only on an exact case-insensitive match and
	// never fuzzily merged — what a caller passes with resolve=false.
	Literal bool
}

// resolver resolves mentions against a bank's existing entities.
type resolver struct {
	known  []knownEntity
	byLow  map[string]int
	byTri  map[string][]int
	degree map[string]int // lowercase name → distinct partners
}

func newResolver(known []knownEntity) *resolver {
	r := &resolver{known: known, byLow: map[string]int{}, byTri: map[string][]int{}, degree: map[string]int{}}
	for i := range r.known {
		k := &r.known[i]
		k.lower = strings.ToLower(k.Name)
		k.tri = trigrams(k.Name)
		if _, dup := r.byLow[k.lower]; !dup {
			r.byLow[k.lower] = i
		}
		for t := range k.tri {
			r.byTri[t] = append(r.byTri[t], i)
		}
		r.degree[k.lower] = len(k.Cooc)
	}
	return r
}

// candidates are the known entities whose trigram overlap clears the
// prefilter, found through the trigram index rather than a scan.
func (r *resolver) candidates(tri map[string]struct{}) []int {
	shared := map[int]int{}
	for t := range tri {
		for _, i := range r.byTri[t] {
			shared[i]++
		}
	}
	var out []int
	for i, n := range shared {
		sim := float64(n) / float64(len(tri)+len(r.known[i].tri)-n)
		if sim >= entityCandidateFloor {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// score is how strongly a mention should reuse a known entity.
func (r *resolver) score(m Mention, low string, tri map[string]struct{}, k *knownEntity) float64 {
	trg := jaccard(tri, k.tri)
	if trg < entityMergeFloor || !tokensCompatible(low, k.lower) {
		return 0
	}
	if trg >= 1 {
		return 1 // the same words, differing only in case or punctuation
	}
	s := 0.5 * seqRatio(low, k.lower)
	var nearby []string
	for _, n := range m.Nearby {
		if nl := strings.ToLower(n); nl != low {
			nearby = append(nearby, nl)
		}
	}
	if len(nearby) > 0 {
		sum := 0.0
		for _, n := range nearby {
			if k.Cooc[n] {
				sum += 1 / math.Sqrt(float64(max(r.degree[n], 1)))
			}
		}
		s += 0.3 * sum / float64(len(nearby))
	}
	if !k.LastSeen.IsZero() && !m.Event.IsZero() {
		days := math.Abs(m.Event.Sub(k.LastSeen).Hours()) / 24
		if days < 7 {
			s += 0.2 * (1 - days/7)
		}
	}
	return math.Round(s*1e6) / 1e6
}

// Resolve returns the canonical name for each mention, in order.
//
// Known entities are reused when the combined score clears entityAccept.
// Names that are new to the bank are clustered among themselves, so one
// retain that says "Dr. Smith" and "Dr Smith" creates one entity, and a
// cluster is named by its most-mentioned spelling, then the shortest, then the
// first alphabetically — a deterministic choice, so the same input always
// yields the same file.
func (r *resolver) Resolve(ms []Mention) []string {
	out := make([]string, len(ms))
	var pending []int
	for i, m := range ms {
		name := NormalizeEntity(m.Text)
		if name == "" {
			continue
		}
		low := strings.ToLower(name)
		if idx, ok := r.byLow[low]; ok {
			out[i] = r.known[idx].Name
			continue
		}
		if m.Literal {
			out[i] = name // created as written, never merged
			continue
		}
		tri := trigrams(name)
		best, bestScore := -1, 0.0
		for _, c := range r.candidates(tri) {
			if s := r.score(m, low, tri, &r.known[c]); s > bestScore {
				best, bestScore = c, s
			}
		}
		if best >= 0 && bestScore >= entityAccept {
			out[i] = r.known[best].Name
			continue
		}
		out[i] = name
		pending = append(pending, i)
	}
	r.clusterNew(out, pending)
	return out
}

func (r *resolver) clusterNew(out []string, idx []int) {
	if len(idx) == 0 {
		return
	}
	// Distinct new names with their mention counts.
	type nameInfo struct {
		name  string
		count int
		tri   map[string]struct{}
	}
	var names []nameInfo
	pos := map[string]int{}
	for _, i := range idx {
		low := strings.ToLower(out[i])
		if p, ok := pos[low]; ok {
			names[p].count++
			continue
		}
		pos[low] = len(names)
		names = append(names, nameInfo{name: out[i], count: 1, tri: trigrams(out[i])})
	}
	parent := make([]int, len(names))
	for i := range parent {
		parent[i] = i
	}
	find := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	if len(names) <= entityBatchMaxNames {
		for a := 0; a < len(names); a++ {
			for b := a + 1; b < len(names); b++ {
				if jaccard(names[a].tri, names[b].tri) >= entityBatchMerge &&
					tokensCompatible(names[a].name, names[b].name) {
					parent[find(a)] = find(b)
				}
			}
		}
	}
	canon := map[int]int{}
	for i := range names {
		root := find(i)
		c, ok := canon[root]
		if !ok || better(names[i].name, names[i].count, names[c].name, names[c].count) {
			canon[root] = i
		}
	}
	for _, i := range idx {
		low := strings.ToLower(out[i])
		out[i] = names[canon[find(pos[low])]].name
	}
}

func better(a string, ac int, b string, bc int) bool {
	if ac != bc {
		return ac > bc
	}
	if la, lb := utf8.RuneCountInString(a), utf8.RuneCountInString(b); la != lb {
		return la < lb
	}
	return a < b
}

// EntityID is an entity's id: derived from its canonical name so that a
// rebuild from the files arrives at the same ids without remembering any.
func EntityID(bankID, canonical string) string {
	return "e" + shortHash(bankID+"\x00"+strings.ToLower(canonical), 15)
}

// properNameRE finds the capitalised runs a rule-based extractor treats as
// entities when no model is available: "Alice", "New York", "Acme Corp".
var properNameRE = regexp.MustCompile(`\b[A-Z][\p{L}'’-]*(?:\s+(?:of\s+|de\s+|van\s+|von\s+)?[A-Z][\p{L}'’-]*)*`)

var notNames = map[string]bool{
	"I": true, "I'm": true, "I've": true, "I'd": true, "I'll": true, "The": true, "A": true, "An": true,
	"This": true, "That": true, "These": true, "Those": true, "It": true, "We": true, "You": true,
	"He": true, "She": true, "They": true, "My": true, "Our": true, "Your": true, "His": true, "Her": true,
	"Their": true, "If": true, "When": true, "Then": true, "But": true, "And": true, "Or": true, "So": true,
	"Yes": true, "No": true, "Ok": true, "OK": true, "Hi": true, "Hello": true, "Thanks": true, "Oh": true,
	"Well": true, "Also": true, "Just": true, "What": true, "Why": true, "How": true, "Where": true,
	"Who": true, "Is": true, "Are": true, "Was": true, "Do": true, "Did": true, "Can": true, "Let": true,
	"There": true, "Here": true, "Sure": true, "Great": true, "Wow": true, "Maybe": true, "Not": true,
	"In": true, "On": true, "At": true, "For": true, "With": true, "After": true, "Before": true,
	"Speaker": true, "User": true, "Assistant": true, "Yesterday": true, "Today": true,
	"Tomorrow": true, "Tonight": true, "Last": true, "Next": true, "Recently": true,
}

// ruleEntities is the no-model entity extractor: capitalised runs that are
// not sentence-initial function words, plus month and weekday names dropped.
func ruleEntities(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range properNameRE.FindAllString(text, -1) {
		m = strings.TrimRight(m, "'’-")
		parts := strings.Fields(m)
		for len(parts) > 0 && notNames[parts[0]] {
			parts = parts[1:]
		}
		for len(parts) > 0 && notNames[parts[len(parts)-1]] {
			parts = parts[:len(parts)-1]
		}
		if len(parts) == 0 {
			continue
		}
		name := strings.Join(parts, " ")
		low := strings.ToLower(name)
		if monthNames[low] != 0 || isWeekday(low) || seen[low] || len(name) < 2 {
			continue
		}
		seen[low] = true
		out = append(out, name)
	}
	return out
}

// entityAliases folds a lone first name into a full name: when a bank knows
// "Dana" and exactly one longer name that starts with that word ("Dana Kim"),
// the two are one person and "Dana" is an alias. Two full names sharing the
// first word ("Dana Kim", "Dana Lee") leave "Dana" alone — guessing which one
// is meant would weld two people together.
//
// It is a pure function of the set of names (in first-seen order, which is
// file order), so a rebuild from the files folds exactly the same names and
// nothing about the fold is stored. The result maps the lowercase alias to the
// full name as first spelled.
func entityAliases(names []string) map[string]string {
	full := map[string]string{} // lowercase full name → first spelling
	byFirst := map[string][]string{}
	single := map[string]bool{}
	for _, n := range names {
		low := strings.ToLower(n)
		ws := words(n)
		switch {
		case len(ws) == 1:
			if len([]rune(ws[0])) >= 2 && !strings.ContainsAny(ws[0], "0123456789") && ws[0] == strings.TrimSpace(low) {
				single[low] = true
			}
		case len(ws) >= 2 && len(ws) <= 4:
			if _, ok := full[low]; ok {
				continue
			}
			full[low] = n
			byFirst[ws[0]] = append(byFirst[ws[0]], low)
		}
	}
	out := map[string]string{}
	for s := range single {
		if cands := byFirst[s]; len(cands) == 1 {
			out[s] = full[cands[0]]
		}
	}
	return out
}
