package index

// Clone: write-time alias texts for memory facts (experiment flag "clone").
//
// A fact is indexed once, as written. When the flag is on, the index also
// stores ALIAS TEXTS for it: sentences that restate the same belief in other
// words, so a query worded differently from the fact still finds it. Aliases
// are embedded and fed to the keyword score, but they are never shown, never
// written to the markdown, and never attached to any fact other than the one
// they were derived from. They live in memory_clone_alias, a table the index
// owns and rebuilds from the vault.
//
// Three sources, each behind its own flag:
//
//   - clone        (E5, templates): a coreference substitution when a fact
//     opens with a pronoun, and two fixed restatements per predicate in a small
//     lexicon: "<subject> <noun>: <value>" and a wh-question.
//   - clone-llm    paraphrases read from GRIMOIRE_CLONE_LLM_FILE, generated
//     offline by a local model; each must pass a grounding check (no capitalised
//     mid-sentence word and no number absent from the fact).
//   - clone-n7     (clonal selection): only facts that were recalled (named in
//     GRIMOIRE_CLONE_SELECTION) get candidates, and at the end of a full
//     reindex an alias is kept only if it raises its own fact's selection query
//     and lowers no other selection query's gold rank. At most cloneCapFact
//     aliases are kept per fact.
//
// Attribution: a candidate is stored under (note, id) of its own fact, and the
// ranking reads it only for that key. Two different entities with the same
// written name are one subject to the text-based identity used everywhere else
// in the index; the coreference step below therefore abstains rather than guess
// when the antecedent is ambiguous.

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

const (
	cloneFlag    = "clone"
	cloneLLMFlag = "clone-llm"
	cloneN7Flag  = "clone-n7"
	cloneLLMEnv  = "GRIMOIRE_CLONE_LLM_FILE"
	cloneSelEnv  = "GRIMOIRE_CLONE_SELECTION"
	// cloneCapFact bounds how many aliases one fact keeps under clone-n7. Each
	// kept alias costs one embedding row and one more score per recall, so the
	// cap bounds index growth to a constant per recalled fact.
	cloneCapFact = 3
)

const cloneSchema = `CREATE TABLE IF NOT EXISTS memory_clone_alias(
  note TEXT NOT NULL, id TEXT NOT NULL, alias TEXT NOT NULL, kind TEXT NOT NULL,
  kept INTEGER NOT NULL DEFAULT 0, vec BLOB,
  PRIMARY KEY(note, id, alias))`

func cloneOn() bool { return exp.On(cloneFlag) }

// cloneFields is the state the clone path keeps on the Index. It is written
// only during a write or a reindex (under the write lock) and read by ranking
// under cloneMu.
type cloneFields struct {
	cloneMu     sync.Mutex
	cacheValid  bool
	cloneCache  map[entKey][]cloneAlias
	cloneActive map[string]bool
	cloneLLM    map[string][]string
	// counters for the log line at the end of a reindex
	cStats cloneStats
}

type cloneStats struct {
	facts, candidates, abstain, llmRejected int
}

// cloneAlias is one stored alias with its embedding.
type cloneAlias struct {
	text string
	vec  []float32
}

// cloneCand is one candidate before embedding.
type cloneCand struct{ text, kind string }

// cloneSlot splits a fact into subject, predicate and value. The predicate
// list is the lexicon's whole vocabulary; a fact with another predicate gets no
// template aliases (coverage is reported, not hidden).
var cloneSlot = regexp.MustCompile(
	`^(.+?) (owns|prefers|works in|listens on port|runs on|is pinned to version|targets release|lives in) (.+)$`)

// clonePronoun matches a fact that opens with a pronoun or a generic noun
// standing for the subject of the previous fact.
var clonePronoun = regexp.MustCompile(`^(It|They|He|She|This|The service|The host|The project) (.+)$`)

// clonePred is the restatement vocabulary per predicate. The noun is used in
// the nominal form, the question is a format with the subject.
var clonePred = map[string]struct{ noun, question string }{
	"owns":                 {"holding", "Which holding does %s have?"},
	"prefers":              {"preference", "What preference does %s have?"},
	"works in":             {"timezone", "Which timezone does %s have?"},
	"listens on port":      {"port number", "What port number does %s have?"},
	"runs on":              {"host", "Which host does %s have?"},
	"is pinned to version": {"pinned version", "What pinned version does %s have?"},
	"targets release":      {"release target", "Which release target does %s have?"},
	"lives in":             {"repository location", "Which repository location does %s have?"},
}

// cloneNoteState follows the subject through one note, in write order, so a
// pronoun can be resolved to the subject of the fact before it.
type cloneNoteState struct {
	last    string              // surface of the most recent subject
	byKey   map[string][]string // short key -> full surfaces seen in this note
	abstain int
}

func newCloneNoteState() *cloneNoteState {
	return &cloneNoteState{byKey: map[string][]string{}}
}

// cloneKey is the short form a full surface is reachable by: the first word,
// cut at a hyphen or dot, so "pgbouncer-512" is reached as "pgbouncer".
func cloneKey(s string) string {
	first := strings.Fields(s)
	if len(first) == 0 {
		return ""
	}
	return strings.FieldsFunc(first[0], func(r rune) bool { return r == '-' || r == '.' })[0]
}

// cloneIsFull reports whether a surface names the entity in full: several
// words, or a service or host name with a digit, hyphen or dot.
func cloneIsFull(s string) bool {
	return len(strings.Fields(s)) >= 2 || strings.ContainsAny(s, "-.0123456789")
}

func (st *cloneNoteState) observe(subject string) {
	st.last = subject
	if !cloneIsFull(subject) {
		return
	}
	k := cloneKey(subject)
	for _, s := range st.byKey[k] {
		if s == subject {
			return
		}
	}
	st.byKey[k] = append(st.byKey[k], subject)
}

// antecedent returns the full surface a pronoun refers to. A short form that
// maps to two or more full names in the note is ambiguous, and the caller must
// abstain.
func (st *cloneNoteState) antecedent() (string, bool) {
	if st.last == "" {
		return "", false
	}
	if cloneIsFull(st.last) {
		return st.last, true
	}
	full := st.byKey[cloneKey(st.last)]
	if len(full) != 1 {
		return "", false
	}
	return full[0], true
}

// candidates returns the alias texts for one fact and advances the note state.
// It never returns the fact's own text.
func (st *cloneNoteState) candidates(text string) []cloneCand {
	text = strings.TrimSpace(text)
	var out []cloneCand
	work := text
	if m := clonePronoun.FindStringSubmatch(text); m != nil {
		ant, ok := st.antecedent()
		if !ok {
			st.abstain++
			return nil
		}
		work = ant + " " + m[2]
		out = append(out, cloneCand{work, "coref"})
	}
	if m := cloneSlot.FindStringSubmatch(work); m != nil {
		subject, verb, value := m[1], m[2], m[3]
		p := clonePred[verb]
		out = append(out,
			cloneCand{fmt.Sprintf("%s %s: %s", subject, p.noun, value), "nominal"},
			cloneCand{fmt.Sprintf(p.question, subject), "question"})
		st.observe(subject)
	}
	var kept []cloneCand
	seen := map[string]bool{text: true}
	for _, c := range out {
		if c.text == "" || seen[c.text] {
			continue
		}
		seen[c.text] = true
		kept = append(kept, c)
	}
	return kept
}

// cloneStopCaps are capitalised words that begin a question or a sentence and
// are therefore not evidence of an entity.
var cloneStopCaps = map[string]bool{
	"The": true, "A": true, "An": true, "Is": true, "Are": true, "Does": true, "Do": true,
	"What": true, "Which": true, "Who": true, "Where": true, "When": true, "How": true,
	"Why": true, "It": true, "They": true, "Tell": true, "For": true, "On": true, "In": true,
	"To": true, "Of": true, "And": true, "Or": true, "This": true, "That": true,
}

var cloneNumRE = regexp.MustCompile(`\d+`)

// cloneGrounded reports whether a paraphrase stays within the fact: every
// number, and every capitalised word that is not sentence-initial, must appear
// in the fact. A paraphrase that is empty or longer than three times the fact
// is refused as well.
func cloneGrounded(fact, para string) bool {
	para = strings.TrimSpace(para)
	if para == "" || len(para) > 3*len(fact) || para == strings.TrimSpace(fact) {
		return false
	}
	low := strings.ToLower(fact)
	for _, n := range cloneNumRE.FindAllString(para, -1) {
		if !strings.Contains(low, n) {
			return false
		}
	}
	for i, w := range strings.FieldsFunc(para, func(r rune) bool {
		return !unicode.IsLetter(r) && r != '\''
	}) {
		if i == 0 || cloneStopCaps[w] {
			continue
		}
		r := []rune(w)
		if unicode.IsUpper(r[0]) && !strings.Contains(low, strings.ToLower(w)) {
			return false
		}
	}
	return true
}

// cloneEnsure creates the table on first use, so a flag-off index never has it.
func (ix *Index) cloneEnsure() error {
	return ix.DB.Exec(cloneSchema)
}

// cloneBeginReindex clears the clone rows and loads the inputs a reindex
// needs: the activation set for clone-n7 and the paraphrases for clone-llm.
func (ix *Index) cloneBeginReindex() error {
	ix.cloneMu.Lock()
	ix.cacheValid = false
	ix.cloneActive = nil
	ix.cloneLLM = nil
	ix.cStats = cloneStats{}
	ix.cloneMu.Unlock()
	if !cloneOn() {
		return nil
	}
	if err := ix.cloneEnsure(); err != nil {
		return err
	}
	if err := ix.DB.Exec("DELETE FROM memory_clone_alias"); err != nil {
		return err
	}
	if exp.On(cloneN7Flag) {
		active := map[string]bool{}
		qs, err := cloneReadSelection()
		if err != nil {
			return err
		}
		for _, q := range qs {
			active[q.ID] = true
		}
		ix.cloneMu.Lock()
		ix.cloneActive = active
		ix.cloneMu.Unlock()
	}
	if exp.On(cloneLLMFlag) {
		m, err := cloneReadLLM()
		if err != nil {
			return err
		}
		ix.cloneMu.Lock()
		ix.cloneLLM = m
		ix.cloneMu.Unlock()
	}
	return nil
}

// cloneWriteRows stores one note's alias candidates. It runs after the note's
// entries are written, and is a no-op with the flag off.
func (ix *Index) cloneWriteRows(rel string, entries []memory.Entry) error {
	if !cloneOn() {
		return nil
	}
	if err := ix.cloneEnsure(); err != nil {
		return err
	}
	n7 := exp.On(cloneN7Flag)
	llm := exp.On(cloneLLMFlag)
	type pending struct{ id, text, kind string }
	var rows []pending
	st := newCloneNoteState()
	ix.cloneMu.Lock()
	active, para := ix.cloneActive, ix.cloneLLM
	ix.cloneMu.Unlock()
	for _, e := range entries {
		cands := st.candidates(e.Text)
		if n7 && !active[e.ID] {
			continue
		}
		if llm {
			for _, p := range para[e.ID] {
				if cloneGrounded(e.Text, p) {
					cands = append(cands, cloneCand{strings.TrimSpace(p), "llm"})
				} else {
					ix.cloneMu.Lock()
					ix.cStats.llmRejected++
					ix.cloneMu.Unlock()
				}
			}
		}
		for _, c := range cands {
			rows = append(rows, pending{e.ID, c.text, c.kind})
		}
	}
	ix.cloneMu.Lock()
	ix.cStats.facts += len(entries)
	ix.cStats.abstain += st.abstain
	ix.cStats.candidates += len(rows)
	ix.cacheValid = false
	ix.cloneMu.Unlock()
	if len(rows) == 0 {
		return nil
	}
	texts := make([]string, len(rows))
	for i, r := range rows {
		texts[i] = r.text
	}
	vecs := ix.Emb.Embed(texts)
	kept := 1
	if n7 {
		kept = 0
	}
	for i, r := range rows {
		var blob []byte
		if i < len(vecs) {
			blob = Pack(vecs[i])
		}
		if err := ix.DB.Exec(
			"INSERT OR IGNORE INTO memory_clone_alias(note,id,alias,kind,kept,vec) VALUES(?,?,?,?,?,?)",
			rel, r.id, r.text, r.kind, kept, blob); err != nil {
			return err
		}
	}
	return nil
}

// cloneDeleteRows removes a note's alias rows before its entries are rewritten.
func (ix *Index) cloneDeleteRows(rel string) error {
	if !cloneOn() {
		return nil
	}
	if err := ix.cloneEnsure(); err != nil {
		return err
	}
	ix.cloneMu.Lock()
	ix.cacheValid = false
	ix.cloneMu.Unlock()
	return ix.DB.Exec("DELETE FROM memory_clone_alias WHERE note=?", rel)
}

// cloneAliasesFor returns the kept aliases of the candidates, from a cache
// that every clone write invalidates. It returns nil with the flag off, so the
// ranking loop is byte-identical to the flag-off path.
func (ix *Index) cloneAliasesFor() map[entKey][]cloneAlias {
	if !cloneOn() {
		return nil
	}
	ix.cloneMu.Lock()
	defer ix.cloneMu.Unlock()
	if ix.cacheValid {
		return ix.cloneCache
	}
	out := map[entKey][]cloneAlias{}
	rows, err := ix.DB.Query("SELECT note,id,alias,vec FROM memory_clone_alias WHERE kept=1 ORDER BY rowid")
	if err != nil {
		// No table yet: nothing has been written with the flag on.
		ix.cloneCache, ix.cacheValid = out, true
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k entKey
		var text string
		var blob []byte
		if err := rows.Scan(&k.note, &k.id, &text, &blob); err != nil {
			break
		}
		out[k] = append(out[k], cloneAlias{text: text, vec: Unpack(blob)})
	}
	ix.cloneCache, ix.cacheValid = out, true
	return out
}

// applyCloneAliases raises a fact's semantic and keyword components to the
// best of its aliases. Only raises: an alias can help a fact rank, never hurt.
func applyCloneAliases(h *MemoryHit, als []cloneAlias, qVec []float32, qNorm float64,
	qTokens []string, idf map[string]float64) {
	for _, a := range als {
		if s := clamp01(cosineNorm(qVec, qNorm, a.vec)); s > h.Semantic {
			h.Semantic = s
		}
		if k := keywordScore(qTokens, a.text, idf); k > h.Keyword {
			h.Keyword = k
		}
	}
}

// cloneEndReindex runs the clonal selection once every fact is indexed, and
// logs what the flags did.
func (ix *Index) cloneEndReindex() error {
	if !cloneOn() {
		return nil
	}
	if exp.On(cloneN7Flag) {
		if err := ix.cloneSelect(); err != nil {
			return err
		}
	}
	ix.cloneMu.Lock()
	st := ix.cStats
	ix.cacheValid = false
	ix.cloneMu.Unlock()
	kept := 0
	_ = ix.DB.QueryRow("SELECT COUNT(*) FROM memory_clone_alias WHERE kept=1").Scan(&kept)
	log.Printf("clone: %d facts, %d candidates, %d kept, %d coreference abstentions, %d llm paraphrases refused",
		st.facts, st.candidates, kept, st.abstain, st.llmRejected)
	return nil
}

// cloneSelQuery is one recall the selection is judged on: the query text and
// the fact it is meant to find. The selection file is the only input; the
// index never learns what a query was for.
type cloneSelQuery struct {
	Note string `json:"note"`
	ID   string `json:"id"`
	Q    string `json:"q"`
}

func cloneReadSelection() ([]cloneSelQuery, error) {
	path := os.Getenv(cloneSelEnv)
	if path == "" {
		log.Printf("clone: %s unset, clone-n7 has no activated facts", cloneSelEnv)
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("clone selection: %w", err)
	}
	var qs []cloneSelQuery
	if err := json.Unmarshal(raw, &qs); err != nil {
		return nil, fmt.Errorf("clone selection: %w", err)
	}
	return qs, nil
}

func cloneReadLLM() (map[string][]string, error) {
	path := os.Getenv(cloneLLMEnv)
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("clone llm file: %w", err)
	}
	var m map[string][]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("clone llm file: %w", err)
	}
	return m, nil
}

// cloneBeats is the ranking order of rankMemory: score, then newer stamp, then
// smaller id.
func cloneBeats(si float64, sti, idi string, sj float64, stj, idj string) bool {
	if si != sj {
		return si > sj
	}
	if sti != stj {
		return sti > stj
	}
	return idi < idj
}

// cloneSelect is the N7 selection. Every selection query is ranked in full
// once. Each candidate alias then changes only its own fact's score, so the new
// rank of every gold fact follows from its old rank plus the one crossing the
// alias causes. This is exact for the ranking rankMemory produces, and it avoids
// re-running the queries once per trial.
func (ix *Index) cloneSelect() error {
	qs, err := cloneReadSelection()
	if err != nil || len(qs) == 0 {
		return err
	}
	type cand struct {
		row int64
		key entKey
		a   cloneAlias
	}
	crow, err := ix.DB.Query("SELECT rowid,note,id,alias,vec FROM memory_clone_alias WHERE kept=0 ORDER BY rowid")
	if err != nil {
		return err
	}
	byFact := map[entKey][]cand{}
	for crow.Next() {
		var c cand
		var blob []byte
		if err := crow.Scan(&c.row, &c.key.note, &c.key.id, &c.a.text, &blob); err != nil {
			crow.Close()
			return err
		}
		c.a.vec = Unpack(blob)
		byFact[c.key] = append(byFact[c.key], c)
	}
	crow.Close()

	// Pass 1: the fact universe. A fact no selection query can see is never a
	// candidate, so its aliases are dropped with the rest of the kept=0 rows.
	idx := map[entKey]int{}
	var keys []entKey
	for _, q := range qs {
		hits, err := ix.MemoryEntries(MemoryQuery{Query: q.Q, Limit: math.MaxInt32})
		if err != nil {
			return err
		}
		for _, h := range hits {
			k := entKey{h.Note, h.ID}
			if _, ok := idx[k]; !ok {
				idx[k] = len(keys)
				keys = append(keys, k)
			}
		}
	}
	nF := len(keys)
	stamp := make([]string, nF)
	id := make([]string, nF)
	factor := make([]float64, nF)

	// Pass 2: per-query state. Hits are not kept past their own query.
	type sel struct {
		qVec  []float32
		qNorm float64
		toks  []string
		idf   map[string]float64
		gold  int
	}
	sels := make([]sel, len(qs))
	score := make([][]float64, len(qs))
	sem := make([][]float64, len(qs))
	kw := make([][]float64, len(qs))
	for qi, q := range qs {
		hits, err := ix.MemoryEntries(MemoryQuery{Query: q.Q, Limit: math.MaxInt32})
		if err != nil {
			return err
		}
		score[qi] = make([]float64, nF)
		sem[qi] = make([]float64, nF)
		kw[qi] = make([]float64, nF)
		for fi := range score[qi] {
			score[qi][fi] = math.Inf(-1)
		}
		rows := make([]memoryRow, len(hits))
		for i, h := range hits {
			fi := idx[entKey{h.Note, h.ID}]
			score[qi][fi] = h.Score
			sem[qi][fi] = h.Semantic
			kw[qi][fi] = h.Keyword
			stamp[fi], id[fi], factor[fi] = h.Stamp, h.ID, h.ImportanceFactor
			rows[i] = memoryRow{hit: MemoryHit{Entry: memory.Entry{Text: h.Text}}}
		}
		qVec := firstVec(ix.Emb.Embed([]string{q.Q}))
		sels[qi] = sel{qVec: qVec, qNorm: norm(qVec), toks: memory.Tokens(q.Q),
			idf: entryIDF(rows), gold: -1}
		if g, ok := idx[entKey{q.Note, q.ID}]; ok && !math.IsInf(score[qi][g], -1) {
			sels[qi].gold = g
		}
	}

	// rankAt is 1 + the number of facts ranked above fact skip when it scores sv.
	rankAt := func(qi, skip int, sv float64) int {
		r := 1
		for j := 0; j < nF; j++ {
			if j == skip || math.IsInf(score[qi][j], -1) {
				continue
			}
			if cloneBeats(score[qi][j], stamp[j], id[j], sv, stamp[skip], id[skip]) {
				r++
			}
		}
		return r
	}
	rankCur := make([]int, len(qs))
	for qi := range qs {
		if g := sels[qi].gold; g >= 0 {
			rankCur[qi] = rankAt(qi, g, score[qi][g])
		}
	}

	var order []int
	for k := range byFact {
		if fi, ok := idx[k]; ok {
			order = append(order, fi)
		}
	}
	sort.Ints(order)
	cands, kept, factsWithKept := 0, 0, 0
	for _, fi := range order {
		cs := byFact[keys[fi]]
		cands += len(cs)
		accepted := 0
		for _, c := range cs {
			if accepted >= cloneCapFact {
				break
			}
			newScore := make([]float64, len(qs))
			newSem := make([]float64, len(qs))
			newKw := make([]float64, len(qs))
			newRank := make([]int, len(qs))
			ok, improved := true, false
			for qi := range qs {
				s := sels[qi]
				newScore[qi], newSem[qi], newKw[qi] = score[qi][fi], sem[qi][fi], kw[qi][fi]
				newRank[qi] = rankCur[qi]
				if s.gold < 0 || math.IsInf(score[qi][fi], -1) {
					continue
				}
				s0, k0 := sem[qi][fi], kw[qi][fi]
				s1 := math.Max(s0, clamp01(cosineNorm(s.qVec, s.qNorm, c.a.vec)))
				k1 := math.Max(k0, keywordScore(s.toks, c.a.text, s.idf))
				ns := score[qi][fi] + factor[fi]*(wSemantic*(s1-s0)+wKeyword*(k1-k0))
				newScore[qi], newSem[qi], newKw[qi] = ns, s1, k1
				var nr int
				if s.gold == fi {
					nr = rankAt(qi, fi, ns)
				} else {
					nr = rankCur[qi]
					g := s.gold
					if cloneBeats(score[qi][fi], stamp[fi], id[fi], score[qi][g], stamp[g], id[g]) {
						nr--
					}
					if cloneBeats(ns, stamp[fi], id[fi], score[qi][g], stamp[g], id[g]) {
						nr++
					}
				}
				newRank[qi] = nr
				if nr > rankCur[qi] {
					ok = false
					break
				}
				if s.gold == fi && nr < rankCur[qi] {
					improved = true
				}
			}
			if !ok || !improved {
				continue
			}
			for qi := range qs {
				score[qi][fi], sem[qi][fi], kw[qi][fi] = newScore[qi], newSem[qi], newKw[qi]
				rankCur[qi] = newRank[qi]
			}
			if err := ix.DB.Exec("UPDATE memory_clone_alias SET kept=1 WHERE rowid=?", c.row); err != nil {
				return err
			}
			accepted++
			kept++
		}
		if accepted > 0 {
			factsWithKept++
		}
	}
	// The passes above read the cache, and the kept flags just changed.
	ix.cloneMu.Lock()
	ix.cacheValid = false
	ix.cloneMu.Unlock()
	if err := ix.DB.Exec("DELETE FROM memory_clone_alias WHERE kept=0"); err != nil {
		return err
	}
	log.Printf("clone-n7: %d selection queries, %d activated facts with candidates, %d candidates, %d kept on %d facts",
		len(qs), len(order), cands, kept, factsWithKept)
	return nil
}
