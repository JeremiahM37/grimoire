package index

import (
	"math"
	"os"
	"sort"
	"strconv"
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

	// ImportanceFactor is the multiplier the fact's importance applied to its
	// score: exactly 1 for an unrated agent fact, so an unrated store is
	// unchanged. (The declared value is Entry.Importance.)
	ImportanceFactor float64
	// Reuse is the bonus earned by being used (see reuseBonus), and Uses and
	// LastUsed are the derived counts behind it. They live in the index only:
	// a reindex resets them, and the markdown never carries them.
	Reuse    float64
	Uses     int
	LastUsed string
}

// DefaultScanLimit bounds how many stored facts one recall may score. It is
// read once from GRIMOIRE_SCAN_LIMIT so a test can put a small corpus over the
// bound; the default is far above any personal vault.
//
// Below the bound a recall scores the newest facts and the result is unchanged
// by the bound. Above it, a recall with text or a vector draws candidates from
// the whole store (memory_candidates.go), so the bound no longer decides
// whether an old fact can be found.
var DefaultScanLimit = scanLimitFromEnv()

func scanLimitFromEnv() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("GRIMOIRE_SCAN_LIMIT"))); err == nil && n > 0 {
		return n
	}
	return 20000
}

// MemoryQuery selects and ranks entries.
type MemoryQuery struct {
	// ScanLimit caps how many rows ranking will score. Zero means
	// DefaultScanLimit.
	ScanLimit int

	// CandidatePool, when positive, overrides the per-arm pool that candidate
	// generation takes above the bound. Zero means scanLimit/10 with a floor.
	CandidatePool int

	Filter Filter

	// Structured narrowing. Empty means "any".
	Note     string
	Paths    []string
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
	Query        string
	LexicalOnly  bool
	AcceptedOnly bool

	// Mode narrows by what the fact is ABOUT: factual drops the personal
	// categories, personal keeps only them, and the zero value keeps all. It is
	// applied in SQL, before the scan bound, like the validity filters.
	Mode memory.RecallMode
	// Basis, when non-empty, keeps only facts whose derived basis is listed.
	Basis []memory.Basis

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

	// ValidAt answers "what was true at this instant in the world": a fact
	// with valid_from <= t and (no valid_to or t < valid_to). Facts with no
	// validity are always valid. It composes with AsOf: "what did we believe on
	// A about what was true on B" is AsOf=A, ValidAt=B.
	ValidAt time.Time
	// ValidSince and ValidUntil select facts whose validity overlaps the closed
	// range [ValidSince, ValidUntil]. Either may be zero, which is open.
	ValidSince time.Time
	ValidUntil time.Time

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

	// wReuse caps the bonus a fact earns from being used (reuseBonus). It is
	// below wUseful's total, so use can reorder facts that are already close
	// but never lifts an irrelevant fact over a relevant one.
	wReuse = 0.05
	// reuseScale is how many uses reach about two thirds of the bonus. Use is
	// saturating for the same reason feedback is: the tenth use says less than
	// the first.
	reuseScale = 3.0

	// recencyHalfLife is how long a fact takes to lose half its recency
	// component. Ninety days: long enough that last quarter's facts still
	// compete, short enough that "what am I working on" surfaces this week's.
	recencyHalfLife = 90 * 24 * time.Hour
)

// importanceFactor scales a fact's whole score by how much it matters. It is
// 1 at the neutral rank (3), so an unrated fact is never reweighted, and it is
// bounded to 0.8..1.2 so importance reorders facts without being able to bury
// or lift one by more than a fifth.
func importanceFactor(effective int) float64 {
	return 1 + 0.1*float64(effective-3)
}

// halfLifeFor stretches the recency half-life for facts that matter and shrinks
// it for facts that do not: an important decision should still be recalled
// after a quarter, trivia should yield to this week's facts sooner.
func halfLifeFor(effective int) time.Duration {
	switch {
	case effective >= 4:
		return 2 * recencyHalfLife
	case effective <= 2:
		return recencyHalfLife / 2
	default:
		return recencyHalfLife
	}
}

// reuseBonus is the bounded ranking bonus for a fact that has been used. Zero
// for a fact never used, so the bonus cannot change an unused store.
func reuseBonus(uses int) float64 {
	if uses <= 0 {
		return 0
	}
	return wReuse * (1 - math.Exp(-float64(uses)/reuseScale))
}

// writeMemoryRows re-derives one note's entries. The caller holds the write
// lock; rows for the note are deleted first, so this is idempotent and a
// reindex converges.
func (ix *Index) writeMemoryRows(note *vault.Note) error {
	// Use counts are derived index state, but a note is rewritten every time
	// one of its facts changes. Carrying them across that rewrite is what makes
	// them mean anything; a full reindex is the one thing allowed to reset them.
	uses, err := ix.memoryUses(note.Path)
	if err != nil {
		return err
	}
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
				"embedding,space,acl,private,origin,human,challenges,"+
				"fresh,chk,verified,nchange,nverify,since,shape,vol,prate,valid_from,valid_to,"+
				"importance,hand,evidence,image,capb)"+
				" VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			e.ID, note.Path, e.Text, e.Agent, e.Task, e.Session, e.Stamp, e.Category,
			e.Expires, immutable, e.SupersededBy, e.SupersededAt, e.Helpful,
			e.Unhelpful, e.Line, blob, space, acl, private, e.Origin, human, e.Challenges,
			e.Fresh, e.Check, e.Verified, e.Changes, e.Verifies, e.Since, e.Shape(), e.Vol, e.PriorRate,
			canonicalValidity(e.ValidFrom), canonicalValidity(e.ValidTo),
			e.Importance, boolInt(e.HumanAuthored()), e.Evidence, e.Image, e.CaptionBasis,
		); err != nil {
			return err
		}
		if n, ok := uses[e.ID]; ok {
			if err := ix.DB.Exec(
				"UPDATE memory_entries SET uses=?, last_used=? WHERE note=? AND id=?",
				n.uses, n.lastUsed, note.Path, e.ID); err != nil {
				return err
			}
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

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// memoryUse is one fact's derived usage.
type memoryUse struct {
	uses     int
	lastUsed string
}

// memoryUses reads the usage counts of one note's facts, so a rewrite can put
// them back. Only facts that were ever used are returned.
func (ix *Index) memoryUses(rel string) (map[string]memoryUse, error) {
	rows, err := ix.DB.Query("SELECT id,uses,last_used FROM memory_entries WHERE note=? AND uses>0", rel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]memoryUse{}
	for rows.Next() {
		var id string
		var u memoryUse
		if err := rows.Scan(&id, &u.uses, &u.lastUsed); err != nil {
			return nil, err
		}
		out[id] = u
	}
	return out, rows.Err()
}

// RecordUse counts one use of a fact: it was recalled and then reported
// helpful. The count is index state only (see writeMemoryRows).
func (ix *Index) RecordUse(rel, id string, at time.Time) error {
	return ix.DB.Exec(
		"UPDATE memory_entries SET uses=uses+1, last_used=? WHERE note=? AND id=?",
		at.UTC().Format(time.RFC3339), rel, id)
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
	where, args := q.sqlWhere()
	limit := q.scanLimit()

	// Above the bound, a query with text or a vector cannot be answered from
	// the newest facts alone: the answer may be an old one. Those queries take
	// the candidate path (memory_candidates.go). Everything else — a store
	// under the bound, or a query that is only a filter — keeps the window,
	// which is the same answer it always gave.
	if q.hasContent() {
		over, err := ix.overScanLimit(where, args, limit)
		if err != nil {
			return nil, err
		}
		if over {
			return ix.memoryEntriesCandidates(q, where, args, limit)
		}
	}

	// The window: the newest facts that pass every check, up to the bound. It
	// streams in recency order and stops once the bound is full, so a Go-side
	// check (expiry, a reader list the SQL cannot express exactly) removes
	// rows without shrinking the answer below the bound. Ranking scores at most
	// limit rows, which is what the bound promises.
	//
	// Below the bound every row is accepted or rejected, the stream ends
	// first, and the result is byte-identical to the unbounded query; that is
	// what memory_scan_test.go pins against a golden file.
	sql := "SELECT " + memoryColumns + " FROM memory_entries WHERE " + where +
		" ORDER BY stamp DESC, id DESC"
	rows, err := ix.DB.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cands []memoryRow
	for rows.Next() && len(cands) < limit {
		r, err := scanMemoryRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		if q.accept(r) {
			cands = append(cands, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ix.rankMemory(cands, q), nil
}

// sqlWhere is every predicate that can be decided in SQL, joined with AND. It
// runs before the scan bound, so the bound counts only rows that can answer
// the question. Each predicate is either exact or a superset of the Go check
// in accept, which then decides; a superset is safe because accept still runs
// on every row that comes back.
func (q MemoryQuery) sqlWhere() (string, []any) {
	where := []string{"1=1"}
	var args []any
	if q.Note != "" {
		where = append(where, "note=?")
		args = append(args, q.Note)
	}
	if len(q.Paths) > 0 {
		clause, values := MemoryPathClause("note", q.Paths)
		where = append(where, clause)
		args = append(args, values...)
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
	switch q.Mode {
	case memory.RecallFactual, memory.RecallPersonal:
		personal := memory.PersonalCategories()
		marks := make([]string, len(personal))
		for i, c := range personal {
			marks[i] = "?"
			args = append(args, c)
		}
		op := " NOT IN "
		if q.Mode == memory.RecallPersonal {
			op = " IN "
		}
		where = append(where, "lower(category)"+op+"("+strings.Join(marks, ",")+")")
	}
	if !q.IncludeSuperseded && q.AsOf.IsZero() {
		where = append(where, "superseded_by=''")
	}
	if q.AcceptedOnly {
		where = append(where, "challenges=''")
	}
	// Validity is compared in SQL, before the scan bound, so a historical
	// question is not answered from only the newest facts. Canonical strings
	// are fixed-width UTC, so text comparison is time comparison. Empty
	// means unbounded on that side.
	//
	// With AsOf set, the SQL is a superset of the belief-dependent rule (see
	// asOfWhere) and accept applies the exact one.
	if q.AsOf.IsZero() && !q.ValidAt.IsZero() {
		at := memory.FormatValidity(q.ValidAt)
		where = append(where, "(valid_from='' OR valid_from<=?) AND (valid_to='' OR valid_to>?)")
		args = append(args, at, at)
	}
	if q.AsOf.IsZero() && !q.ValidSince.IsZero() {
		// Overlap: the fact must end after the range starts...
		where = append(where, "(valid_to='' OR valid_to>?)")
		args = append(args, memory.FormatValidity(q.ValidSince))
	}
	if q.AsOf.IsZero() && !q.ValidUntil.IsZero() {
		// ...and start no later than the range ends.
		where = append(where, "(valid_from='' OR valid_from<=?)")
		args = append(args, memory.FormatValidity(q.ValidUntil))
	}
	if !q.AsOf.IsZero() {
		w, a := q.asOfWhere()
		where = append(where, w...)
		args = append(args, a...)
	}
	if !q.Filter.IncludePrivate {
		where = append(where, "private=0")
	}
	// An image with no caption is stored so the bytes are not lost, but its
	// placeholder text is not a fact and must never be recalled as one.
	where = append(where, "capb<>'none'")
	w, a := q.Filter.sqlVisibility()
	where = append(where, w...)
	args = append(args, a...)
	return strings.Join(where, " AND "), args
}

// asOfWhere is the belief check at q.AsOf and the validity bounds as they were
// known at that instant, expressed in SQL exactly rather than as a superset.
// It must be exact: the window takes LIMIT rows before accept runs, so a
// predicate that admits extra rows would leave the window short of the bound
// and change which facts it holds.
//
// Stamps have minute precision and are local wall-clock strings in a fixed
// width, so comparing a stamp with stampBound(AsOf) is the same comparison as
// comparing the instants: a stamp of 10:30 is not after 10:30:45, and 10:31 is.
func (q MemoryQuery) asOfWhere() ([]string, []any) {
	at := stampBound(q.AsOf)
	where := []string{
		// Written by then (BelievedAt: written.After(t) excludes).
		"(stamp='' OR stamp<=?)",
		// Not replaced by then. A replacement after AsOf keeps the fact; an
		// empty or unparsable one does not (BelievedAt returns false).
		"(superseded_by='' OR superseded_at>?)",
	}
	args := []any{at, at}
	if !q.ValidAt.IsZero() {
		v := memory.FormatValidity(q.ValidAt)
		where = append(where,
			"(valid_from='' OR valid_from<=?)",
			// valid_to does not apply when the supersession that closes it
			// happened after AsOf (validTo ignores it then). That is the
			// third arm below; otherwise the bound must lie after the instant.
			"(valid_to='' OR valid_to>? OR (superseded_by<>'' AND superseded_at>?))")
		args = append(args, v, v, at)
	}
	if !q.ValidUntil.IsZero() {
		where = append(where, "(valid_from='' OR valid_from<=?)")
		args = append(args, memory.FormatValidity(q.ValidUntil))
	}
	if !q.ValidSince.IsZero() {
		v := memory.FormatValidity(q.ValidSince)
		where = append(where,
			"(valid_to='' OR valid_to>? OR (superseded_by<>'' AND superseded_at>?))")
		args = append(args, v, at)
	}
	return where, args
}

// stampBound renders an instant in the form bullet stamps are stored in.
func stampBound(t time.Time) string {
	return t.In(time.Local).Format(memory.StampFormat)
}

// scanLimit is the bound on rows ranking may score.
func (q MemoryQuery) scanLimit() int {
	if q.ScanLimit > 0 {
		return q.ScanLimit
	}
	return DefaultScanLimit
}

// hasContent reports whether the query asks for something a candidate can
// match: words, or a vector. A pure filter has neither, and is answered by the
// window, because a filter alone has no reason to prefer an old fact.
func (q MemoryQuery) hasContent() bool {
	return strings.TrimSpace(q.Query) != "" || len(q.QueryVector) > 0
}

// accept applies the checks that cannot be decided in SQL, and the same
// space and reader-list rules that sqlVisibility pushes down. It is the
// authoritative check: whatever SQL let through is decided here.
func (q MemoryQuery) accept(r memoryRow) bool {
	if !q.allows(r) {
		return false
	}
	if !q.AsOf.IsZero() {
		if !r.hit.BelievedAt(q.AsOf) {
			return false
		}
		if !q.ValidAt.IsZero() && !r.hit.ValidAtAsOf(q.ValidAt, q.AsOf) {
			return false
		}
		if (!q.ValidSince.IsZero() || !q.ValidUntil.IsZero()) &&
			!r.hit.ValidDuringAsOf(q.ValidSince, q.ValidUntil, q.AsOf) {
			return false
		}
		return true
	}
	return q.IncludeExpired || !r.hit.ExpiredAt(q.Now)
}

// overScanLimit reports whether more than limit rows pass the SQL predicates.
// It counts at most limit+1 rows, so the probe costs no more than the window.
func (ix *Index) overScanLimit(where string, args []any, limit int) (bool, error) {
	n, err := ix.DB.Count(
		"SELECT COUNT(*) FROM (SELECT 1 FROM memory_entries WHERE "+where+" LIMIT ?)",
		append(args, limit+1)...)
	if err != nil {
		return false, err
	}
	return n > limit, nil
}

// memoryColumns is the select list every memoryRow is scanned from. It must
// match scanMemoryRow's order.
const memoryColumns = "id,note,text,agent,task,session,stamp,category,expires,immutable," +
	"superseded_by,superseded_at,helpful,unhelpful,line,embedding,space,acl," +
	"private,origin,human,challenges,fresh,chk,verified,nchange,nverify,since,vol,prate," +
	"valid_from,valid_to,importance,hand,uses,last_used,evidence,image,capb"

// scanMemoryRow reads one row selected with memoryColumns. It takes the Scan
// method rather than the rows, so both the ranked query and the prune query
// can share it.
func scanMemoryRow(scan func(...any) error) (memoryRow, error) {
	var (
		r         memoryRow
		immutable int
		human     int
		private   int
		hand      int
		blob      []byte
		evidence  string
		image     string
		capb      string
	)
	if err := scan(&r.hit.ID, &r.hit.Note, &r.hit.Text, &r.hit.Agent,
		&r.hit.Task, &r.hit.Session, &r.hit.Stamp, &r.hit.Category,
		&r.hit.Expires, &immutable, &r.hit.SupersededBy, &r.hit.SupersededAt,
		&r.hit.Helpful, &r.hit.Unhelpful, &r.hit.Line, &blob, &r.sp, &r.acl,
		&private, &r.hit.Origin, &human, &r.hit.Challenges,
		&r.hit.Fresh, &r.hit.Check, &r.hit.Verified, &r.hit.Changes,
		&r.hit.Verifies, &r.hit.Since, &r.hit.Vol, &r.hit.PriorRate,
		&r.hit.ValidFrom, &r.hit.ValidTo,
		&r.hit.Importance, &hand, &r.hit.Uses, &r.hit.LastUsed, &evidence,
		&image, &capb); err != nil {
		return r, err
	}
	r.hit.Evidence = evidence
	r.hit.Image = image
	r.hit.CaptionBasis = capb
	r.hit.Immutable = immutable == 1
	r.hit.Human = human == 1
	r.hit.HandWritten = hand == 1
	r.priv = private == 1
	r.vec = Unpack(blob)
	return r, nil
}

// canonicalValidity is the form a validity bound takes in the index: canonical
// RFC3339 UTC, or empty for unbounded and for a bound nothing could parse. The
// index is derived, so this never rejects a write; the API validates input.
func canonicalValidity(s string) string {
	if t, ok := memory.ParseValidity(s); ok {
		return memory.FormatValidity(t)
	}
	return ""
}

func MemoryPathClause(column string, paths []string) (string, []any) {
	clauses := make([]string, 0, len(paths))
	var args []any
	for _, path := range paths {
		if strings.HasSuffix(path, "/") {
			clauses = append(clauses, "substr("+column+",1,length(?))=?")
			args = append(args, path, path)
		} else {
			clauses = append(clauses, column+"=?")
			args = append(args, path)
		}
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args
}

// allows applies the space and reader-list checks to one row.
func (q MemoryQuery) allows(r memoryRow) bool {
	if q.Filter.Spaces != nil && !q.Filter.Spaces[r.sp] {
		return false
	}
	if !q.Filter.IgnoreACLs && !aclAllows(r.acl, q.Filter.User) {
		return false
	}
	if len(q.Basis) > 0 && !hasBasis(q.Basis, r.hit.Basis()) {
		return false
	}
	return true
}

func hasBasis(want []memory.Basis, b memory.Basis) bool {
	for _, w := range want {
		if w == b {
			return true
		}
	}
	return false
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
	if len(qVec) == 0 && !q.LexicalOnly {
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
		eff := h.EffectiveImportance()
		h.Semantic = clamp01(cosineNorm(qVec, qNorm, c.vec))
		h.Keyword = keywordScore(qTokens, c.hit.Text, idf)
		h.Entity = memory.EntityOverlap(qEntities, ents[entKey{c.hit.Note, c.hit.ID}])
		h.Recency = recencyScoreWith(c.hit.Stamp, q.Now, halfLifeFor(eff))
		h.Useful = c.hit.Usefulness()
		h.ImportanceFactor = importanceFactor(eff)
		h.Reuse = reuseBonus(h.Uses)
		base := wSemantic*h.Semantic + wKeyword*h.Keyword +
			wEntity*h.Entity + wRecency*h.Recency + wUseful*h.Useful
		// An unrated agent fact has factor 1 and no reuse, so its score is the
		// pre-importance score bit for bit (x*1 and x+0 are exact).
		h.Score = base*h.ImportanceFactor + h.Reuse
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
	return recencyScoreWith(stamp, now, recencyHalfLife)
}

// recencyScoreWith is recencyScore with the half-life chosen by the caller.
func recencyScoreWith(stamp string, now time.Time, halfLife time.Duration) float64 {
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
	return math.Exp2(-float64(age) / float64(halfLife))
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

// FreshnessEvidence totals, per shape class, how many times current facts
// have been found changed and how many observed days of history they cover
// (first version to last confirmation; see memory.Entry.ObservedDays). It is what
// memory.LearnPriors turns into the store's own change rates, and it is one
// aggregate query rather than a walk of every note.
func (ix *Index) FreshnessEvidence() (changes, exposureDays [2]float64, err error) {
	rows, err := ix.DB.Query(
		"SELECT shape, COALESCE(SUM(nchange),0), " +
			"COALESCE(SUM(MAX(julianday(CASE WHEN verified<>'' THEN verified ELSE stamp END) - " +
			"julianday(CASE WHEN since<>'' THEN since ELSE stamp END), 0)),0) " +
			"FROM memory_entries WHERE superseded_by='' AND stamp<>'' GROUP BY shape")
	if err != nil {
		return changes, exposureDays, err
	}
	defer rows.Close()
	for rows.Next() {
		var shape int
		var c, d float64
		if err := rows.Scan(&shape, &c, &d); err != nil {
			return changes, exposureDays, err
		}
		if shape >= 0 && shape < len(changes) {
			changes[shape] += c
			exposureDays[shape] += d
		}
	}
	return changes, exposureDays, rows.Err()
}

// PruneCandidates lists the facts eviction may retract: agent-written, not
// immutable, not human-authored, not challenged, never voted helpful or used,
// explicitly rated at or below `below`, and last written before `cutoff`.
//
// Each of those is a condition, not a preference, and the query is the only
// place they are stated. Anything that fails one is never a candidate — in
// particular an unrated fact (importance 0) is not, because nobody has said it
// is low value; it only ranks as one. Oldest first, so a capped run retracts
// the stalest facts before anything newer.
func (ix *Index) PruneCandidates(below int, cutoff time.Time, limit int) ([]MemoryHit, error) {
	if below < memory.MinImportance {
		below = memory.MinImportance
	}
	if below > memory.DefaultImportance {
		// A rating of 4 or 5 is a person's or an operator's judgement that the
		// fact matters, and neither is ever a candidate.
		below = memory.DefaultImportance
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := ix.DB.Query(
		"SELECT "+memoryColumns+" FROM memory_entries WHERE"+
			" superseded_by='' AND immutable=0 AND human=0 AND hand=0 AND challenges=''"+
			" AND helpful=0 AND uses=0 AND last_used='' AND agent<>'' AND stamp<>''"+
			" AND stamp<? AND importance BETWEEN ? AND ?"+
			" ORDER BY stamp ASC, id ASC LIMIT ?",
		cutoff.In(time.Local).Format(memory.StampFormat), memory.MinImportance, below, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryHit
	for rows.Next() {
		r, err := scanMemoryRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r.hit)
	}
	return out, rows.Err()
}
