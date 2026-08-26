package index

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Fact-level rows for agent memory.
//
// Memory notes are indexed twice: once as notes, like everything else, and
// once bullet by bullet here. The second pass is what lets recall rank a
// single fact, reconciliation find the belief a new fact contradicts, and a
// filter address one agent's memory from one session — none of which a
// note-shaped row can express, because a memory note accumulates dozens of
// unrelated facts and ranks as one blurred document.
//
// These rows are derived, like every other table in this package: drop them
// and a reindex rebuilds them from the markdown.

// MemoryPrefix is the vault namespace agent memory lives under.
const MemoryPrefix = memory.Dir + "/"

// IsMemoryPath reports whether a note holds agent memory.
func IsMemoryPath(rel string) bool { return strings.HasPrefix(rel, MemoryPrefix) }

// MemoryHit is one entry with the scores that ranked it.
type MemoryHit struct {
	memory.Entry
	Note  string
	Score float64

	// The component scores, kept so /api/memory/explain can show why a fact
	// was recalled. A ranking nobody can inspect is one nobody can fix.
	Semantic float64
	Keyword  float64
	Entity   float64
	Recency  float64
	Useful   float64
}

// DefaultScanLimit bounds how many stored facts one recall may score.
//
// Chosen to sit far above any personal vault — this project's own has two — so
// that in practice the bound never binds and ranking is unchanged. It exists
// for the tail: without it a recall is O(every fact ever recorded), which is
// fine at a hundred and 762ms at fifty thousand.
const DefaultScanLimit = 20000

// MemoryQuery selects and ranks entries.
type MemoryQuery struct {
	// ScanLimit caps how many rows ranking will score. Zero means
	// DefaultScanLimit.
	ScanLimit int

	Filter Filter

	// Structured narrowing. Empty means "any".
	Note     string
	Agent    string
	Task     string
	Session  string
	Category string
	ID       string

	// SessionSet selects facts with NO session when Session is empty. Without
	// it, "" means "any session" — which is right for a filter and wrong for a
	// reconciliation scope, where it would let a sessionless write supersede
	// every session's facts.
	SessionSet bool

	// Query ranks the survivors. Empty returns them newest first.
	Query string

	// QueryVector ranks by a vector the caller already computed, for a
	// framework that owns its embedding step. It must be in THIS server's
	// embedding space — the caller gets it from /api/embed — because a cosine
	// between vectors from two different models is a number with no meaning.
	// With no query text there are no terms and no entities, so semantic is
	// the only content signal available.
	QueryVector []float32

	IncludeSuperseded bool
	IncludeExpired    bool

	// AsOf answers "what did this agent believe THEN": only facts that were
	// written by that instant and had not yet been replaced or expired. It
	// overrides IncludeSuperseded and IncludeExpired, which are about what to
	// show now rather than about which "now" to ask about.
	//
	// Nothing but keeping superseded facts in the file makes this answerable,
	// which is the argument for keeping them: a store that deletes what it
	// replaces cannot reconstruct a belief it no longer holds.
	AsOf time.Time

	Now   time.Time
	Limit int
}

// Ranking weights. Semantic carries the most because it is the only signal
// that survives paraphrase; keyword is next because an exact term the user
// typed is usually the point; entity is a boost rather than a base score
// because a fact merely mentioning a name is not necessarily about it; and
// recency is small on purpose — it breaks ties between equally relevant facts
// without letting a new irrelevant one outrank an old exact match.
const (
	wSemantic = 0.42
	wKeyword  = 0.28
	wEntity   = 0.18
	wRecency  = 0.04
	// wUseful is small and saturating (see memory.Entry.Usefulness): feedback
	// reorders facts that are already close, and cannot bury one that is the
	// only answer to a question. It is also the one signal a person can drive
	// directly, which is a reason to keep its authority low.
	wUseful = 0.08

	// recencyHalfLife is how long a fact takes to lose half its recency
	// component. Ninety days: long enough that last quarter's facts still
	// compete, short enough that "what am I working on" surfaces this week's.
	recencyHalfLife = 90 * 24 * time.Hour
)

// writeMemoryRows re-derives one note's entries. The caller holds the write
// lock; rows for the note are deleted first, so this is idempotent and a
// reindex converges.
func (ix *Index) writeMemoryRows(note *vault.Note) error {
	if err := ix.deleteMemoryRows(note.Path); err != nil {
		return err
	}
	if !IsMemoryPath(note.Path) || note.Encrypted {
		// Never mine facts out of ciphertext: the whole point of an encrypted
		// note is that the index cannot read it.
		return nil
	}
	entries := memory.Parse(note.Body)
	if len(entries) == 0 {
		return nil
	}
	private := 0
	if note.Private {
		private = 1
	}
	space := ix.spaceOf(note.Path)
	acl := EncodeACL(splitList(note.Frontmatter.StringVal("readers")))

	texts := make([]string, len(entries))
	for i, e := range entries {
		texts[i] = e.Text
	}
	vecs := ix.Emb.Embed(texts)

	for i, e := range entries {
		human := 0
		if e.Human {
			human = 1
		}
		immutable := 0
		if e.Immutable {
			immutable = 1
		}
		var blob []byte
		if i < len(vecs) {
			blob = Pack(vecs[i])
		}
		// OR IGNORE, because two byte-identical bullets in one note derive the
		// same id and the primary key would reject the second — failing the
		// whole write, which is how a note holding a duplicate (exactly what
		// consolidation exists to clean up) became unindexable. Colliding ids
		// mean the same stamp, agent and text: they ARE one fact, so collapsing
		// them is the right answer rather than a workaround.
		if err := ix.DB.Exec(
			"INSERT OR IGNORE INTO memory_entries(id,note,text,agent,task,session,stamp,category,"+
				"expires,immutable,superseded_by,superseded_at,helpful,unhelpful,line,"+
				"embedding,space,acl,private,origin,human,challenges)"+
				" VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			e.ID, note.Path, e.Text, e.Agent, e.Task, e.Session, e.Stamp, e.Category,
			e.Expires, immutable, e.SupersededBy, e.SupersededAt, e.Helpful,
			e.Unhelpful, e.Line, blob, space, acl, private, e.Origin, human, e.Challenges,
		); err != nil {
			return err
		}
		for _, ent := range memory.Entities(e.Text) {
			if err := ix.DB.Exec(
				"INSERT OR IGNORE INTO memory_entities(note,id,entity) VALUES(?,?,?)",
				note.Path, e.ID, ent); err != nil {
				return err
			}
		}
	}
	return nil
}

func (ix *Index) deleteMemoryRows(rel string) error {
	if err := ix.DB.Exec("DELETE FROM memory_entries WHERE note=?", rel); err != nil {
		return err
	}
	return ix.DB.Exec("DELETE FROM memory_entities WHERE note=?", rel)
}

// memoryRow is a hit before ranking.
type memoryRow struct {
	hit  MemoryHit
	vec  []float32
	acl  string
	priv bool
	sp   string
}

// MemoryEntries selects the entries a principal may read and ranks them.
//
// Access is applied in SQL and again in Go rather than to the output: the
// keyword component scores against the statistics of the entries the caller
// can see, so a fact in another space cannot change how their own facts rank.
// Same reasoning as RetrieveFor; see the comment there.
func (ix *Index) MemoryEntries(q MemoryQuery) ([]MemoryHit, error) {
	if q.Now.IsZero() {
		q.Now = time.Now()
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	where := []string{"1=1"}
	var args []any
	if q.Note != "" {
		where = append(where, "note=?")
		args = append(args, q.Note)
	}
	if q.ID != "" {
		where = append(where, "id=?")
		args = append(args, q.ID)
	}
	if q.Agent != "" {
		where = append(where, "agent=?")
		args = append(args, q.Agent)
	}
	if q.Task != "" {
		where = append(where, "task=?")
		args = append(args, q.Task)
	}
	if q.Session != "" || q.SessionSet {
		where = append(where, "session=?")
		args = append(args, q.Session)
	}
	if q.Category != "" {
		where = append(where, "category=?")
		args = append(args, q.Category)
	}
	if !q.IncludeSuperseded && q.AsOf.IsZero() {
		where = append(where, "superseded_by=''")
	}
	if !q.Filter.IncludePrivate {
		where = append(where, "private=0")
	}
	// Bound the scan. Ranking scores every row this returns, so an unbounded
	// query makes a recall cost O(every fact ever recorded): measured at 762ms
	// over 50k entries, and a benchmark like BEAM runs at 1M tokens and up.
	//
	// The bound is deliberately generous and ordered by recency, so for any
	// vault below it the result is byte-identical to the unbounded query and no
	// ranking behaviour changes at all. Above it, the newest scanLimit facts are
	// scored — which is the right tail to keep, because superseded facts are
	// already excluded and what remains is a current belief set.
	sql := "SELECT id,note,text,agent,task,session,stamp,category,expires,immutable," +
		"superseded_by,superseded_at,helpful,unhelpful,line,embedding,space,acl," +
		"private,origin,human,challenges FROM memory_entries WHERE " +
		strings.Join(where, " AND ")
	limit := q.ScanLimit
	if limit <= 0 {
		limit = DefaultScanLimit
	}
	sql += " ORDER BY stamp DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := ix.DB.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cands []memoryRow
	for rows.Next() {
		var (
			r         memoryRow
			immutable int
			human     int
			private   int
			blob      []byte
		)
		if err := rows.Scan(&r.hit.ID, &r.hit.Note, &r.hit.Text, &r.hit.Agent,
			&r.hit.Task, &r.hit.Session, &r.hit.Stamp, &r.hit.Category,
			&r.hit.Expires, &immutable, &r.hit.SupersededBy, &r.hit.SupersededAt,
			&r.hit.Helpful, &r.hit.Unhelpful, &r.hit.Line, &blob, &r.sp, &r.acl,
			&private, &r.hit.Origin, &human, &r.hit.Challenges); err != nil {
			return nil, err
		}
		r.hit.Immutable = immutable == 1
		r.hit.Human = human == 1
		r.priv = private == 1
		r.vec = Unpack(blob)
		if !q.allows(r) {
			continue
		}
		if !q.AsOf.IsZero() {
			if !r.hit.BelievedAt(q.AsOf) {
				continue
			}
		} else if !q.IncludeExpired && r.hit.ExpiredAt(q.Now) {
			continue
		}
		cands = append(cands, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ix.rankMemory(cands, q), nil
}

// allows applies the space and reader-list checks to one row.
func (q MemoryQuery) allows(r memoryRow) bool {
	if q.Filter.Spaces != nil && !q.Filter.Spaces[r.sp] {
		return false
	}
	if !q.Filter.IgnoreACLs && !aclAllows(r.acl, q.Filter.User) {
		return false
	}
	return true
}

// entKey identifies one entry's stored entity list. Note and id together,
// because memory_entries is keyed that way — an id alone repeats across notes.
type entKey struct{ note, id string }

// candidateEntities returns each candidate's entity list, read from the table
// that indexing already wrote rather than re-extracted from its text.
//
// Ranking used to call memory.Entities on every candidate on every query, which
// is the same deterministic extraction the indexer had already done and stored
// in memory_entities — a table that was written on every note write and never
// once read. Measured over 500 candidates the recompute costs 7.06ms and 36,001
// allocations against 0.027ms and zero for a precomputed set, so it was roughly
// 250x the cost of the ranking arithmetic it was feeding.
//
// Rows missing from the table fall back to extraction. That matters for
// correctness, not just caution: an index built before entities were stored, or
// mid-reindex, would otherwise silently score every entity overlap as zero and
// quietly change what retrieval returns.
func (ix *Index) candidateEntities(cands []memoryRow, qEntities []string) map[entKey][]string {
	out := make(map[entKey][]string, len(cands))
	if len(cands) == 0 {
		return out
	}
	// One query for the whole candidate set. Per-candidate queries would trade
	// a CPU cost for a round-trip count, which is the worse of the two.
	notes := make(map[string]bool, len(cands))
	for _, c := range cands {
		notes[c.hit.Note] = true
	}
	placeholders := make([]string, 0, len(notes))
	args := make([]any, 0, len(notes))
	for n := range notes {
		placeholders = append(placeholders, "?")
		args = append(args, n)
	}
	rows, err := ix.DB.Query(
		"SELECT note,id,entity FROM memory_entities WHERE note IN ("+
			strings.Join(placeholders, ",")+")", args...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var k entKey
			var ent string
			if err := rows.Scan(&k.note, &k.id, &ent); err != nil {
				break
			}
			out[k] = append(out[k], ent)
		}
	}
	for _, c := range cands {
		k := entKey{c.hit.Note, c.hit.ID}
		if _, ok := out[k]; !ok {
			out[k] = memory.Entities(c.hit.Text)
		}
	}
	return out
}

func (ix *Index) rankMemory(cands []memoryRow, q MemoryQuery) []MemoryHit {
	if strings.TrimSpace(q.Query) == "" && len(q.QueryVector) == 0 {
		out := make([]MemoryHit, 0, len(cands))
		for _, c := range cands {
			out = append(out, c.hit)
		}
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Stamp != out[j].Stamp {
				return out[i].Stamp > out[j].Stamp
			}
			return out[i].ID < out[j].ID
		})
		return truncate(out, q.Limit)
	}

	qVec := q.QueryVector
	if len(qVec) == 0 {
		qVec = firstVec(ix.Emb.Embed([]string{q.Query}))
	}
	qNorm := norm(qVec)
	qEntities := memory.Entities(q.Query)
	idf := entryIDF(cands)
	qTokens := memory.Tokens(q.Query)
	ents := ix.candidateEntities(cands, qEntities)

	out := make([]MemoryHit, 0, len(cands))
	for _, c := range cands {
		h := c.hit
		h.Semantic = clamp01(cosineNorm(qVec, qNorm, c.vec))
		h.Keyword = keywordScore(qTokens, c.hit.Text, idf)
		h.Entity = memory.EntityOverlap(qEntities, ents[entKey{c.hit.Note, c.hit.ID}])
		h.Recency = recencyScore(c.hit.Stamp, q.Now)
		h.Useful = c.hit.Usefulness()
		h.Score = wSemantic*h.Semantic + wKeyword*h.Keyword +
			wEntity*h.Entity + wRecency*h.Recency + wUseful*h.Useful
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Stamp != out[j].Stamp {
			return out[i].Stamp > out[j].Stamp
		}
		return out[i].ID < out[j].ID
	})
	return truncate(out, q.Limit)
}

func truncate(hits []MemoryHit, limit int) []MemoryHit {
	if len(hits) > limit {
		return hits[:limit]
	}
	return hits
}

// entryIDF weights a term by how rare it is among the entries the caller can
// see. Without it, a word every fact contains — "user", "the server" — scores
// as highly as the one word that distinguishes them.
func entryIDF(cands []memoryRow) map[string]float64 {
	df := map[string]int{}
	for _, c := range cands {
		for t := range setOf(memory.Tokens(c.hit.Text)) {
			df[t]++
		}
	}
	n := float64(len(cands))
	idf := make(map[string]float64, len(df))
	for t, d := range df {
		idf[t] = math.Log(1 + (n-float64(d)+0.5)/(float64(d)+0.5))
	}
	return idf
}

func keywordScore(qTokens []string, text string, idf map[string]float64) float64 {
	if len(qTokens) == 0 {
		return 0
	}
	have := setOf(memory.Tokens(text))
	var got, want float64
	for _, t := range qTokens {
		w := idf[t]
		if w == 0 {
			w = 1 // a term no entry contains still counts against the query
		}
		want += w
		if have[t] {
			got += w
		}
	}
	if want == 0 {
		return 0
	}
	return got / want
}

// recencyScore decays from 1 at "now" with a fixed half-life. A fact with an
// unparseable or missing timestamp scores neutral rather than zero: an entry
// written before stamps existed should not be pushed to the bottom.
func recencyScore(stamp string, now time.Time) float64 {
	if stamp == "" {
		return 0.5
	}
	t, err := time.ParseInLocation("2006-01-02 15:04", stamp, now.Location())
	if err != nil {
		if t, err = time.ParseInLocation("2006-01-02", stamp, now.Location()); err != nil {
			return 0.5
		}
	}
	age := now.Sub(t)
	if age < 0 {
		return 1
	}
	return math.Exp2(-float64(age) / float64(recencyHalfLife))
}

func setOf(words []string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

func firstVec(vs [][]float32) []float32 {
	if len(vs) == 0 {
		return nil
	}
	return vs[0]
}

func norm(v []float32) float64 {
	var s float64
	for _, f := range v {
		s += float64(f) * float64(f)
	}
	return math.Sqrt(s)
}

func cosineNorm(a []float32, aNorm float64, b []float32) float64 {
	if aNorm == 0 || len(a) == 0 || len(b) == 0 {
		return 0
	}
	bNorm := norm(b)
	if bNorm == 0 {
		return 0
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot / (aNorm * bNorm)
}

// clamp01 keeps a cosine in range. Embedders are free to return vectors with
// negative components, and a negative semantic score would let an unrelated
// fact drag a combined score below one with no signal at all.
func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}
