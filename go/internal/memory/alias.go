package memory

import (
	"regexp"
	"strings"
	"time"
)

// Alias linking: resolving a short form to the full entity it stands for.
//
// A fact that says "Priya prefers tabs" or "auth-gateway is on port 8443" does
// not name the entity the way a query does ("Priya N. Schmidt", "auth-gateway-03").
// Nothing in that fact's text links the two, so ranking cannot recover it. The
// link has to come from somewhere else in the vault: another fact, in the same
// session or note, that names the entity in full. That evidence is what this
// file turns into a link.
//
// Everything here is deterministic and model-free, and a pure function of the
// facts it is given. The index derives its tables from it and never writes the
// result back to the markdown. An alias that cannot be tied to exactly one
// entity is not linked at all: a wrong link is worse than a missed one, because
// it puts an unrelated fact in front of a question about someone else.

// Scope names the evidence a link came from, strongest first.
const (
	ScopeSession   = "session"
	ScopeNote      = "note"
	ScopeComention = "comention"
	ScopeUnique    = "unique"
)

// scopeConf is the confidence of each scope. Session evidence is the author
// naming the entity in the same conversation; a unique alias is only a guess
// that nothing else contradicts.
var scopeConf = map[string]float64{
	ScopeSession:   1.0,
	ScopeNote:      0.8,
	ScopeComention: 0.7,
	ScopeUnique:    0.5,
}

// LinkCeiling caps what a link can add to a query's entity score. A literal
// full-name match scores 1, so any link stays strictly below it: a fact that
// says the name outright always outranks one reached through an alias.
const LinkCeiling = 0.95

// NoteWindow bounds the note scope in time, in both directions. Facts in one
// note written weeks apart are not evidence about each other.
const NoteWindow = 30 * 24 * time.Hour

// MaxAliasKeysPerFact bounds how many short forms one fact is searched for.
// It keeps a pathological fact from producing an unbounded number of link rows.
const MaxAliasKeysPerFact = 64

// Mention is a full entity name found in one fact.
type Mention struct {
	Canon string   // "priya n schmidt", "pgbouncer-523", "db01.prod"
	Kind  string   // "person" or "service"
	Keys  []string // the short forms this name establishes
}

var (
	// First name, middle initial (dot optional), last name. The initial is what
	// separates two people who share a first and last name, so it is part of
	// the canonical name, and the first+last pair without it is an alias.
	personFullRE = regexp.MustCompile(`\b(\p{Lu}\p{Ll}+)\s+(\p{Lu})\.?\s+(\p{Lu}\p{Ll}+)\b`)
	// A first and last name with no initial. Never a canonical name by itself;
	// it is only a short form to be resolved.
	personPairRE = regexp.MustCompile(`\b(\p{Lu}\p{Ll}+)\s+(\p{Lu}\p{Ll}+)\b`)
	// A hyphenated service with a numeric suffix: "pgbouncer-523", "auth-gateway-03".
	serviceRE = regexp.MustCompile(`\b([a-z][a-z0-9]*(?:-[a-z][a-z0-9]*)*)-(\d{2,4})\b`)
	// A host name: "db01.prod". The letters-then-digits stem is the short form.
	hostRE = regexp.MustCompile(`\b([a-z]+\d+)\.([a-z][a-z0-9-]*)\b`)
	// A lowercase word, with its hyphenated form kept whole.
	aliasWordRE = regexp.MustCompile(`[\p{L}\p{N}]+(?:-[\p{L}\p{N}]+)*`)
)

// FullMentions returns the full entity names in text, each once, in order of
// first appearance.
func FullMentions(text string) []Mention {
	var out []Mention
	seen := map[string]bool{}
	add := func(m Mention) {
		if !seen[m.Canon] {
			seen[m.Canon] = true
			out = append(out, m)
		}
	}
	for _, m := range personFullRE.FindAllStringSubmatch(text, -1) {
		first := strings.ToLower(m[1])
		if isSentenceStart(first) || stopwords[first] {
			continue
		}
		init := strings.ToLower(m[2])
		last := strings.ToLower(m[3])
		add(Mention{
			Canon: first + " " + init + " " + last,
			Kind:  "person",
			Keys:  []string{first, first + " " + last},
		})
	}
	lower := strings.ToLower(text)
	for _, m := range serviceRE.FindAllStringSubmatch(lower, -1) {
		keys := []string{m[1]}
		if i := strings.IndexByte(m[1], '-'); i > 0 {
			keys = append(keys, m[1][:i])
		}
		add(Mention{Canon: m[0], Kind: "service", Keys: keys})
	}
	for _, m := range hostRE.FindAllStringSubmatch(lower, -1) {
		add(Mention{Canon: m[0], Kind: "service", Keys: []string{m[1]}})
	}
	return out
}

// QueryCanons is the canonical names a query names in full. A query that says
// "Priya Schmidt" with no initial names no canonical entity and gets no link
// boost; it keeps its ordinary lexical behaviour.
func QueryCanons(text string) []string {
	ms := FullMentions(text)
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Canon)
	}
	return out
}

// AliasCandidates returns the short forms a fact might use to refer to an
// entity: each lowercase word, each part of a hyphenated word, and each
// two-word capitalised pair (a first and last name without an initial). These
// are the keys searched for in the alias table; a key is only linked if the
// table has evidence for it. Capped at MaxAliasKeysPerFact, in text order.
func AliasCandidates(text string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(k string) {
		if len(k) < 3 || seen[k] || stopwords[k] {
			return
		}
		seen[k] = true
		out = append(out, k)
	}
	lower := strings.ToLower(text)
	for _, w := range aliasWordRE.FindAllString(lower, -1) {
		add(w)
		if strings.Contains(w, "-") {
			for _, part := range strings.Split(w, "-") {
				add(part)
			}
		}
	}
	for _, m := range personPairRE.FindAllStringSubmatch(text, -1) {
		first := strings.ToLower(m[1])
		if isSentenceStart(first) {
			continue
		}
		add(first + " " + strings.ToLower(m[2]))
	}
	if len(out) > MaxAliasKeysPerFact {
		out = out[:MaxAliasKeysPerFact]
	}
	return out
}

// StampTime reads a bullet timestamp for the linker's time comparisons.
func StampTime(s string) (time.Time, bool) {
	return parseStamp(s)
}

// Evidence is one full-name mention that establishes an alias: the fact it is
// in, and what that fact names.
type Evidence struct {
	Alias   string
	Canon   string // the entity the alias resolves to
	Note    string
	ID      string
	Session string
	Stamp   time.Time
	Canons  []string // every full-name canonical in the same fact
}

// FactRef is a fact that mentions an alias and is being resolved.
type FactRef struct {
	Note    string
	ID      string
	Session string
	Stamp   time.Time
	Canons  []string // the full-name canonicals this fact itself names
}

// Link is a resolved alias.
type Link struct {
	Alias  string
	Entity string
	Scope  string
	Conf   float64
}

// Outcome says what Resolve decided.
type Outcome int

const (
	// NoEvidence: nothing in any scope names a candidate entity.
	NoEvidence Outcome = iota
	// Linked: the best scope with candidates has exactly one.
	Linked
	// Ambiguous: the best scope has more than one candidate. Nothing is linked.
	Ambiguous
)

// AliasResolver answers, for one alias, which entity a fact that uses it is
// about. It is built once from every piece of evidence for the alias, so that
// resolving each fact is a map lookup and not a scan of the evidence.
type AliasResolver struct {
	alias     string
	bySession map[string]map[string]bool
	byNote    map[string][]Evidence
	// co maps another full entity X to each entity C that is named alongside X
	// in some fact, with the earliest such fact's stamp.
	co map[string]map[string]time.Time
	// unique maps each candidate entity to the earliest stamp of its evidence.
	unique map[string]time.Time
}

// NewAliasResolver indexes the evidence for alias. Evidence for other aliases
// is ignored, so a caller may pass a superset.
func NewAliasResolver(alias string, ev []Evidence) *AliasResolver {
	r := &AliasResolver{
		alias:     alias,
		bySession: map[string]map[string]bool{},
		byNote:    map[string][]Evidence{},
		co:        map[string]map[string]time.Time{},
		unique:    map[string]time.Time{},
	}
	for _, e := range ev {
		if e.Alias != alias {
			continue
		}
		if e.Session != "" {
			if r.bySession[e.Session] == nil {
				r.bySession[e.Session] = map[string]bool{}
			}
			r.bySession[e.Session][e.Canon] = true
		}
		r.byNote[e.Note] = append(r.byNote[e.Note], e)
		if t, ok := r.unique[e.Canon]; !ok || e.Stamp.Before(t) {
			r.unique[e.Canon] = e.Stamp
		}
		for _, x := range e.Canons {
			if x == e.Canon {
				continue
			}
			if r.co[x] == nil {
				r.co[x] = map[string]time.Time{}
			}
			if t, ok := r.co[x][e.Canon]; !ok || e.Stamp.Before(t) {
				r.co[x][e.Canon] = e.Stamp
			}
		}
	}
	return r
}

// Resolve decides which entity f's use of the alias refers to.
//
// The scopes are tried strongest first, and the first scope with any candidate
// decides. It does not fall through to a weaker scope when the stronger one is
// ambiguous: two people in the same session who share a first name make that
// session's "Priya" unresolvable, even if the note around it names only one.
//
// Time limits. Session evidence counts at any time, because a conversation is
// one unit however its facts are stamped. Note evidence counts within
// NoteWindow either side. Co-mention and unique evidence count only if it is
// no later than f itself. That last rule is the guard against retroactive
// relinking: a full name written later, in another session, must not change
// what an earlier fact meant.
func (r *AliasResolver) Resolve(f FactRef) (Link, Outcome) {
	if f.Session != "" {
		if set := r.bySession[f.Session]; len(set) > 0 {
			return r.pick(set, ScopeSession)
		}
	}
	set := map[string]bool{}
	for _, e := range r.byNote[f.Note] {
		if nearStamp(e.Stamp, f.Stamp) {
			set[e.Canon] = true
		}
	}
	if len(set) > 0 {
		return r.pick(set, ScopeNote)
	}
	set = map[string]bool{}
	for _, x := range f.Canons {
		for c, t := range r.co[x] {
			if !t.After(f.Stamp) && c != x {
				set[c] = true
			}
		}
	}
	if len(set) > 0 {
		return r.pick(set, ScopeComention)
	}
	for c, t := range r.unique {
		if !t.After(f.Stamp) {
			set[c] = true
		}
	}
	if len(set) > 0 {
		return r.pick(set, ScopeUnique)
	}
	return Link{}, NoEvidence
}

func (r *AliasResolver) pick(set map[string]bool, scope string) (Link, Outcome) {
	if len(set) != 1 {
		return Link{}, Ambiguous
	}
	for c := range set {
		return Link{Alias: r.alias, Entity: c, Scope: scope, Conf: scopeConf[scope]}, Linked
	}
	return Link{}, NoEvidence
}

func nearStamp(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d <= NoteWindow
}

// LinkScore is what a link adds to a query's entity score.
func LinkScore(conf float64) float64 {
	return conf * LinkCeiling
}
