// Package fingerprint finds the distinctive content of a memory (a path, a
// host, a port, a flag, an env var, a version, a rare identifier) so that
// "did the agent use this memory" can be answered without the agent writing a
// tag. An agent that was shown "snapshot with ~/tailscale-helpers/snapshot.sh"
// and then runs that script has used the memory; the script's name is the
// evidence.
//
// Two halves share one normalisation spec (Spec, version SpecVersion):
//
//   - the server picks up to MaxPerMemory fingerprints per memory, scored by
//     rarity across the whole memory store and never one that is already in
//     the situation that triggered the injection, and sends only salted
//     hashes of them;
//   - the client hook runs Candidates over what the agent did and said,
//     hashes each candidate the same way, and reports how many fingerprints
//     matched. It never sends text. clients/hooks/grimoire_outcome.py
//     implements Candidates in Python; testdata/vectors.json is run by both
//     test suites, so the two cannot drift.
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"regexp"
	"sort"
	"strings"
)

// SpecVersion changes whenever Candidates or Hash would give a different
// answer, so an old hook and a new server notice the mismatch.
const SpecVersion = 1

// MaxPerMemory is the most fingerprints kept for one memory.
const MaxPerMemory = 6

// HashHex is the length of a fingerprint hash in hex characters (40 bits).
const HashHex = 10

// Spec is the normalisation spec the server sends beside the hashes. The
// client implements exactly this.
const Spec = "v1: lowercase ASCII letters only; runs of [a-z0-9_./:~@%+#$=-]; " +
	"strip leading [:=+#%@] and trailing [.:=-/+#%@]; keep runs of 3..160 chars; " +
	"also emit: the run without a leading $; pieces split on '='; for a URL its host and host:port; " +
	"for a path every component of 3+ chars; for a:b pieces of 3+ chars. " +
	"hash = sha256(salt + NUL + token) hex[:10]"

var (
	runRE   = regexp.MustCompile(`[A-Za-z0-9_./:~@%+#$=\-]+`)
	codeRE  = regexp.MustCompile("`([^`\\n]{1,200})`")
	leadCut = ":=+#%@"
	tailCut = ".:=-/+#%@"
)

// asciiLower lowercases A-Z only, so Go and the Python port agree on every
// input (full Unicode lowering differs between them).
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func clean(run string) string {
	run = strings.TrimLeft(run, leadCut)
	return strings.TrimRight(run, tailCut)
}

// expand lists a run and the pieces worth matching separately, in a fixed
// order. The run keeps whatever case it came in, so the server can classify
// it; Candidates lowers the run first.
func expand(run string) []string {
	if len(run) < 3 || len(run) > 160 {
		return nil
	}
	out := []string{run}
	add := func(p string) {
		if p = clean(p); len(p) >= 3 {
			out = append(out, p)
		}
	}
	if strings.HasPrefix(run, "$") {
		add(run[1:])
	}
	for _, p := range strings.Split(run, "=") {
		if p != run {
			add(p)
		}
	}
	if i := strings.Index(run, "://"); i >= 0 {
		rest := run[i+3:]
		auth, path, _ := strings.Cut(rest, "/")
		if at := strings.LastIndex(auth, "@"); at >= 0 {
			auth = auth[at+1:]
		}
		add(auth)
		if host, _, ok := strings.Cut(auth, ":"); ok {
			add(host)
		}
		for _, p := range strings.Split(path, "/") {
			add(p)
		}
		return dedupe(out)
	}
	if strings.Contains(run, "/") {
		for _, p := range strings.Split(run, "/") {
			add(p)
		}
	} else if strings.Contains(run, ":") {
		for _, p := range strings.Split(run, ":") {
			add(p)
		}
	}
	return dedupe(out)
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// maxCandidates bounds the work on a very long text.
const maxCandidates = 4000

// Candidates is the normalisation spec: every token in text that could be a
// fingerprint, lowercase and distinct, in order of first appearance.
func Candidates(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, run := range runRE.FindAllString(text, -1) {
		for _, c := range expand(clean(asciiLower(run))) {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
				if len(out) >= maxCandidates {
					return out
				}
			}
		}
	}
	return out
}

// Set is Candidates as a set.
func Set(text string) map[string]bool {
	out := map[string]bool{}
	for _, c := range Candidates(text) {
		out[c] = true
	}
	return out
}

// Hash is the salted, truncated hash the client compares against.
func Hash(salt, token string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + token))
	return hex.EncodeToString(sum[:])[:HashHex]
}

// DF is document frequency over a memory store.
type DF struct {
	N     int
	Count map[string]int
}

// BuildDF counts, for every candidate token, how many of the texts contain it.
func BuildDF(texts []string) DF {
	df := DF{Count: map[string]int{}}
	for _, t := range texts {
		df.N++
		for c := range Set(t) {
			df.Count[c]++
		}
	}
	return df
}

// Fingerprint is one distinctive token of a memory.
type Fingerprint struct {
	Token string  `json:"token"`
	Kind  string  `json:"kind"`
	Score float64 `json:"score"`
	DF    int     `json:"df"`
}

var (
	ipRE      = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}(:\d+)?$`)
	flagRE    = regexp.MustCompile(`^--[A-Za-z][A-Za-z0-9-]{2,}$`)
	envRE     = regexp.MustCompile(`^\$?[A-Z][A-Z0-9]*_[A-Z0-9_]+$`)
	version3  = regexp.MustCompile(`^v?\d+(\.\d+){2,}$`)
	version2  = regexp.MustCompile(`^v\d+(\.\d+)+$|^\d+\.\d+$`)
	portRE    = regexp.MustCompile(`:\d{3,5}$`)
	camelRE   = regexp.MustCompile(`[a-z][A-Z]`)
	mixRE     = regexp.MustCompile(`[A-Za-z]\d|\d[A-Za-z]`)
	extRE     = regexp.MustCompile(`^[A-Za-z0-9_-]{2,}\.[A-Za-z][A-Za-z0-9]{0,6}$`)
	hyphenRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]+(-[A-Za-z0-9]+)+$`)
	pureDigit = regexp.MustCompile(`^\d+$`)
)

// kindWeight is how much evidence a kind of token is, before rarity.
var kindWeight = map[string]float64{
	"url": 1.2, "ip": 1.2, "path": 1.0, "env": 1.1, "flag": 1.0, "ident": 1.0,
	"port": 0.9, "version": 0.8, "code": 0.55, "compound": 0.7,
}

// generic words that are never evidence, whatever their rarity here.
var generic = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`home admin root usr bin etc var tmp opt mnt lib src dev proc run srv
		local share users user projects project docs doc true false null none yes localhost http https www com org net
		readme license todo note notes memory session agent feedback
		return returns import export class classes function functions method methods value values result results
		error errors input output data file files path paths name names type types list lists item items text string
		storage server client service services command commands option options config default defaults set get add
		remove delete create update read write open close start stop run runs running build builds test tests
		role important always never must should avoid check checks keep make made need needs only also then than
		when where which while with without work works used using user users time times line lines code main
		before after first last next previous latest current only one two three all any each every both other
		sonnet haiku opus claude model models prompt prompts task tasks tool tools rule rules`) {
		generic[w] = true
	}
}

// classify says what kind of evidence run (in original case) is, or "".
func classify(run string) string {
	switch {
	case strings.Contains(run, "://"):
		return "url"
	case ipRE.MatchString(run):
		return "ip"
	case flagRE.MatchString(run):
		return "flag"
	case envRE.MatchString(run):
		return "env"
	case version3.MatchString(run) || version2.MatchString(run):
		return "version"
	case strings.Contains(run, "/"):
		if strings.HasPrefix(run, "/") || strings.HasPrefix(run, "~") || strings.HasPrefix(run, ".") ||
			strings.Count(run, "/") >= 2 || strings.Contains(run[strings.LastIndex(run, "/"):], ".") {
			return "path"
		}
		return ""
	case portRE.MatchString(run):
		return "port"
	case strings.Contains(run, "_") || camelRE.MatchString(run) ||
		(len(run) >= 5 && mixRE.MatchString(run) && !pureDigit.MatchString(run)) ||
		extRE.MatchString(run):
		return "ident"
	case len(run) >= 7 && hyphenRE.MatchString(run):
		// "copy-paste" and "read-only" are English; "tailscale-helpers" is a
		// name. Only rarity tells them apart, so they weigh less.
		return "compound"
	}
	return ""
}

type pending struct {
	token, kind string
	run         int
}

// eligible lists the tokens of text that may become fingerprints, each with
// its kind and the run it came from (so one run cannot fill the quota).
func eligible(text string) []pending {
	var out []pending
	run := 0
	take := func(orig string, code bool) {
		run++
		cut := clean(orig)
		for _, piece := range expand(cut) {
			kind := classify(piece)
			if kind == "" && code {
				kind = "code"
			}
			if kind == "" {
				continue
			}
			out = append(out, pending{asciiLower(piece), kind, run})
		}
	}
	for _, m := range codeRE.FindAllStringSubmatch(text, -1) {
		for _, r := range runRE.FindAllString(m[1], -1) {
			take(r, true)
		}
	}
	for _, r := range runRE.FindAllString(codeRE.ReplaceAllString(text, " "), -1) {
		take(r, false)
	}
	return out
}

// inSituation reports whether t, or a distinctive piece of it, was already in
// the situation: an agent that repeats a prompt's file name is not showing
// that it read the memory, whether it writes the bare name or the full path.
func inSituation(t string, situation map[string]bool) bool {
	if situation[t] {
		return true
	}
	parts := expand(t)
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts[1:] {
		if len(p) >= 4 && !generic[p] && situation[p] {
			return true
		}
	}
	return false
}

// Options tune Select.
type Options struct {
	// MinScore is the weakest fingerprint worth keeping (idf x kind weight).
	MinScore float64
	// MaxDF is the most documents a fingerprint may appear in. Zero means
	// 2% of the store, at least 2 and at most 12: a token in a hundred
	// memories is not distinctive however large the store.
	MaxDF int
	// PerRun is the most fingerprints one run of the text may contribute.
	PerRun int
}

// DefaultOptions are the shipped settings.
var DefaultOptions = Options{MinScore: 2.5, PerRun: 2}

// Select picks the distinctive tokens of a memory: rare across df, not in the
// situation (a token the prompt or command already contained proves nothing
// when it shows up in the answer), at most MaxPerMemory. A memory with none
// returns nil: unfingerprintable, and nothing is guessed.
func Select(text string, df DF, situation map[string]bool, opt Options) []Fingerprint {
	if opt.MinScore == 0 {
		opt.MinScore = DefaultOptions.MinScore
	}
	if opt.PerRun == 0 {
		opt.PerRun = DefaultOptions.PerRun
	}
	maxDF := opt.MaxDF
	if maxDF == 0 {
		maxDF = int(math.Min(12, math.Max(2, float64(df.N)/50)))
	}
	best := map[string]Fingerprint{}
	runOf := map[string]int{}
	for _, p := range eligible(text) {
		t := p.token
		if len(t) < 4 && p.kind != "port" || generic[t] || pureDigit.MatchString(t) {
			continue
		}
		if inSituation(t, situation) {
			continue
		}
		d := df.Count[t]
		if d < 1 {
			d = 1 // the memory itself
		}
		if d > maxDF {
			continue
		}
		idf := math.Log(float64(df.N+1) / float64(d+1))
		score := idf * kindWeight[p.kind]
		if score < opt.MinScore {
			continue
		}
		if old, ok := best[t]; !ok || score > old.Score {
			best[t] = Fingerprint{Token: t, Kind: p.kind, Score: score, DF: d}
			runOf[t] = p.run
		}
	}
	all := make([]Fingerprint, 0, len(best))
	for _, f := range best {
		all = append(all, f)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		// Equal evidence: the shorter token survives more spellings (the
		// basename matches ~/x/y.sh and /home/u/x/y.sh alike).
		if len(all[i].Token) != len(all[j].Token) {
			return len(all[i].Token) < len(all[j].Token)
		}
		return all[i].Token < all[j].Token
	})
	var out []Fingerprint
	perRun := map[int]int{}
	for _, f := range all {
		if perRun[runOf[f.Token]] >= opt.PerRun {
			continue
		}
		perRun[runOf[f.Token]]++
		out = append(out, f)
		if len(out) == MaxPerMemory {
			break
		}
	}
	return out
}
