package index

import (
	"database/sql"
	"errors"
	"math"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Hybrid retrieval: reciprocal-rank fusion of embedding cosine with
// IDF-weighted BM25. Port of the exact full-fusion path in server/index.py —
// the one the LoCoMo and LongMemEval numbers were measured on.
//
// The ranking is deliberately unchanged from that port; retrieval_parity_test
// pins its output to a digest. What changed is the cost of producing it. The
// original loaded every chunk out of SQLite, decoded every embedding blob and
// re-tokenized every chunk body on EVERY query, so a search allocated roughly
// 9.4 KB per stored chunk and ran in time linear in the corpus with a very
// large constant: 596 ms and 469 MB of garbage at 50k chunks. None of that
// work depends on the query, so all of it belongs in a cache.

var wordRE = regexp.MustCompile(`[a-z0-9]+`)

// BM25 tuning, matching the Python constants.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
	rrfK   = 60.0
)

// Hit is one retrieved chunk. ChunkIdx is internal: it drives neighbour
// merging but is not part of the response, matching the Python payload.
type Hit struct {
	Path     string  `json:"path"`
	Title    string  `json:"title"`
	Chunk    string  `json:"chunk"`
	Score    float64 `json:"score"`
	ChunkIdx int     `json:"-"`
}

// ---------------------------------------------------------------- the cache

// noteMeta is per-note identity, stored once rather than once per chunk.
// Chunks-per-note is around 8 in a real vault, so interning the path and title
// here is most of the cache's non-vector footprint.
type noteMeta struct {
	path  string
	title string
}

// The ordering view: row positions in corpus order — by note path, then chunk
// index — which is the order a freshly built cache already holds them in.
//
// Ranking breaks every tie by candidate position, so that order is part of the
// output, not an implementation detail. Once the cache can be patched in place
// a new note's rows land at the end of the arena rather than in their sorted
// place, so the arena stops being the corpus order. sorted keeps it, and
// Retrieve walks that instead. A cache built by a rebuild leaves it nil, where
// the identity order is the corpus order and costs nothing.

// order returns row positions in corpus order. Read-only: it is called with
// the cache read lock held, concurrently, so it cannot materialize anything.
func (c *corpusCache) order() []int32 {
	if c.sorted != nil {
		return c.sorted
	}
	return c.identity
}

// beforeRow reports whether row a sorts before row b in corpus order.
func (c *corpusCache) beforeRow(a, b int32) bool {
	ra, rb := &c.rows[a], &c.rows[b]
	pa, pb := c.notes[ra.note].path, c.notes[rb.note].path
	if pa != pb {
		return pa < pb
	}
	return ra.ci < rb.ci
}

// cachedRow is one chunk, reduced to what ranking actually reads. Note the
// absence of the chunk TEXT: scoring never looks at it, only the handful of
// rows that survive into the response do, and those are cheap to fetch by
// primary key at the end. Keeping bodies here would have been the single
// largest allocation in the process for no benefit.
type cachedRow struct {
	note    int32 // index into corpusCache.notes
	ci      int32 // chunk_idx within the note
	total   int32 // token count, for BM25 length normalization
	chars   int32 // chunk length, so a removal can undo its corpus-size total
	space   int32 // index into corpusCache.spaces
	private bool
	// dead marks a row whose note has been rewritten or deleted since the
	// cache was built. Rows are tombstoned rather than removed because the
	// postings lists address rows by position; compaction is a rebuild.
	dead bool
}

// posting is one (term, chunk) occurrence with its term frequency.
type posting struct {
	row int32
	tf  int32
}

// corpusCache is the query-independent half of retrieval, materialized once
// per index revision.
//
// Two structural choices carry the speedup:
//
//   - vectors live in ONE flat float32 arena instead of 50k separately
//     allocated slices, so the cosine sweep is a linear walk over contiguous
//     memory instead of a pointer chase, and the GC has one object to track
//     rather than one per chunk;
//   - the lexical leg reads an inverted index (term -> postings) instead of
//     asking every chunk "do you contain this term?". BM25 only ever scores
//     chunks that actually contain a query term, so the work becomes
//     proportional to matches rather than to corpus size.
//
// Norms are precomputed, which is safe to the bit: Cosine accumulates them in
// index order and so does the builder, and float64 addition over the same
// values in the same order gives the same result.
type corpusCache struct {
	rev int64

	notes []noteMeta
	rows  []cachedRow
	vecs  []float32 // row i occupies vecs[i*dim : (i+1)*dim]
	dim   int
	norms []float64 // ||v|| per row, pre-zero-guard

	postings map[string][]posting

	// Where each note's rows live, so a write can find and tombstone them
	// without scanning the corpus.
	byNote  map[string][]int32
	noteIdx map[string]int32
	// dead rows accumulate with churn; past a threshold the cache is dropped
	// and rebuilt compactly rather than carrying them forever.
	deadRows int

	// spaces interned to int32, so a row carries an index rather than a string
	// and a visibility check is an array lookup.
	spaces   []string
	spaceIdx map[string]int32

	// sorted is the corpus-order view of rows, materialized only once the
	// arena stops being in corpus order — which happens the first time a note
	// is patched in. identity is the 0..n-1 view a freshly built cache uses.
	sorted   []int32
	identity []int32

	// Corpus statistics, kept for both visibilities so a query never has to
	// sweep the corpus just to learn its size. Accumulated in row order, which
	// is the order the original summed them in.
	nAll, nPublic     int
	lenAll, lenPublic float64

	// character totals, for callers deciding whether the whole corpus fits a
	// budget rather than which parts of it to rank
	charsAll, charsPublic int64
}

// spaceID interns a space name, adding it if new.
func (c *corpusCache) spaceID(name string) int32 {
	if name == "" {
		name = "commons"
	}
	if id, ok := c.spaceIdx[name]; ok {
		return id
	}
	id := int32(len(c.spaces))
	c.spaces = append(c.spaces, name)
	if c.spaceIdx == nil {
		c.spaceIdx = map[string]int32{}
	}
	c.spaceIdx[name] = id
	return id
}

// allowedSpaces turns a filter's space names into a lookup by interned id.
// A nil filter set means every space, which is what a single-user deployment
// and an administrator both get.
func (c *corpusCache) allowedSpaces(names map[string]bool) []bool {
	if names == nil {
		return nil
	}
	out := make([]bool, len(c.spaces))
	for i, s := range c.spaces {
		out[i] = names[s]
	}
	return out
}

func (c *corpusCache) count(includePrivate bool) int {
	if includePrivate {
		return c.nAll
	}
	return c.nPublic
}

// countFor and lenFor compute corpus statistics over the visible subset.
//
// With no space filter these are the totals maintained on every write, which
// is why the unrestricted path costs nothing. With one they are a scan — the
// price of BM25 being scored against the caller's corpus rather than the whole
// one. It is O(rows) of int32 reads against a query that already walks every
// row for cosine.
func (c *corpusCache) countFor(includePrivate bool, allowed []bool) int {
	if allowed == nil {
		return c.count(includePrivate)
	}
	n := 0
	for i := range c.rows {
		r := &c.rows[i]
		if r.dead || (!includePrivate && r.private) {
			continue
		}
		if int(r.space) < len(allowed) && allowed[r.space] {
			n++
		}
	}
	return n
}

func (c *corpusCache) avgLenFor(includePrivate bool, allowed []bool) float64 {
	if allowed == nil {
		return c.avgLen(includePrivate)
	}
	total, n := 0.0, 0
	for i := range c.rows {
		r := &c.rows[i]
		if r.dead || (!includePrivate && r.private) {
			continue
		}
		if int(r.space) < len(allowed) && allowed[r.space] {
			total += float64(r.total)
			n++
		}
	}
	if n == 0 || total == 0 {
		return 1
	}
	return total / float64(n)
}

// avgLen mirrors the original's guard: an all-empty corpus would divide by
// zero, and a zero average would then make every BM25 length norm infinite.
func (c *corpusCache) avgLen(includePrivate bool) float64 {
	n := c.count(includePrivate)
	if n == 0 {
		return 1
	}
	total := c.lenPublic
	if includePrivate {
		total = c.lenAll
	}
	avg := total / float64(n)
	if avg == 0 {
		avg = 1
	}
	return avg
}

// cosine scores row i against a query vector, reproducing index.Cosine exactly
// — including its two guards: a dimension mismatch scores 0, and a zero-length
// vector is treated as unit length so an empty note cannot produce NaN and
// poison the ranking.
func (c *corpusCache) cosine(i int, qv []float32, qnorm float64) float64 {
	if c.dim != len(qv) || c.dim == 0 {
		return 0
	}
	v := c.vecs[i*c.dim : (i+1)*c.dim]
	var dot float64
	for j, x := range v {
		dot += float64(x) * float64(qv[j])
	}
	na := c.norms[i]
	if na == 0 {
		na = 1
	}
	if qnorm == 0 {
		qnorm = 1
	}
	return dot / (na * qnorm)
}

// corpusCacheFor returns the cache for the current index revision, rebuilding
// it if the index has been written since it was built.
//
// Double-checked under an RWMutex: searches are concurrent and read-mostly, so
// the common path takes the read lock and returns immediately. The revision is
// re-read under the write lock because two searches can arrive together after
// an edit and only one of them should pay for the rebuild.
func (ix *Index) corpusCacheFor() (*corpusCache, error) {
	rev := ix.Rev()
	ix.cacheMu.RLock()
	c := ix.cache
	// c.rev is read under the lock: a patch updates it in place, so reading it
	// afterwards races with the write that made the cache current.
	current := c != nil && c.rev == rev
	ix.cacheMu.RUnlock()
	if current {
		return c, nil
	}

	ix.cacheMu.Lock()
	defer ix.cacheMu.Unlock()
	if ix.cache != nil && ix.cache.rev == ix.Rev() {
		return ix.cache, nil
	}
	rev = ix.Rev() // read AFTER the scan starts below would race; see buildCache
	built, err := ix.buildCache(rev)
	if err != nil {
		return nil, err
	}
	ix.cache = built
	return built, nil
}

// errCacheContended means a write landed between every attempt to read a
// current cache. Reported rather than served from a stale one: eight
// consecutive losses is a symptom worth seeing, not something to paper over.
var errCacheContended = errors.New("retrieval cache could not be read: writes are landing faster than it can be built")

// withCache runs fn against a current cache, holding the READ lock for the
// whole of it.
//
// Holding it for the duration is what the in-place patching costs: a writer
// mutates the arena, the postings and the corpus statistics, and a reader
// midway through a ranking must not see half of that. Readers still run
// concurrently with each other — the lock is only exclusive while a write
// patches — and a patch touches one note's rows rather than rebuilding
// everything, so the window it holds is short.
func (ix *Index) withCache(fn func(*corpusCache) error) error {
	for attempt := 0; attempt < 8; attempt++ {
		if _, err := ix.corpusCacheFor(); err != nil {
			return err
		}
		ix.cacheMu.RLock()
		c := ix.cache
		if c == nil || c.rev != ix.Rev() {
			// A write landed between building and reading. Rebuild and retry
			// rather than answer from a cache that is already stale.
			ix.cacheMu.RUnlock()
			continue
		}
		err := fn(c)
		ix.cacheMu.RUnlock()
		return err
	}
	return errCacheContended
}

// buildCache materializes the cache from SQLite.
//
// The revision is captured by the CALLER before the scan, not after: if a
// write lands mid-scan, stamping the cache with the post-scan revision would
// mark a half-old snapshot as current and it would never be rebuilt. Stamping
// the pre-scan revision means the worst case is one redundant rebuild.
//
// Rows are ordered by (note, chunk_idx) because RRF scores chunks by RANK and
// many chunks tie on the lexical leg, so scan order decides how ties break.
// Unspecified order would make results shift after an unrelated rebuild.
func (ix *Index) buildCache(rev int64) (*corpusCache, error) {
	// Size the arena up front. Growing it by append means repeatedly copying
	// the whole vector block — at 200k chunks that is a 200 MB memmove done
	// over and over as it doubles. One COUNT is cheaper than all of it.
	n, err := ix.DB.Count("SELECT count(*) FROM vectors")
	if err != nil {
		return nil, err
	}

	// Titles are read once per NOTE rather than once per chunk. Joining them
	// onto the chunk scan made SQLite materialize, and the driver allocate, one
	// title string for every chunk — tens of copies of the same string per
	// note, on a scan whose cost is dominated by per-row allocation.
	titles, err := ix.noteTitles()
	if err != nil {
		return nil, err
	}

	rows, err := ix.DB.Query(
		"SELECT note, chunk, chunk_idx, embedding, private, space FROM vectors " +
			"ORDER BY note, chunk_idx")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	c := &corpusCache{rev: rev, postings: make(map[string][]posting),
		byNote: make(map[string][]int32), noteIdx: make(map[string]int32),
		spaceIdx: make(map[string]int32)}
	if n > 0 {
		c.rows = make([]cachedRow, 0, n)
		c.norms = make([]float64, 0, n)
	}
	noteIdx := make(map[string]int32)
	// Term frequencies are gathered per chunk into a reused map; a fresh map
	// per chunk was a large share of the original's allocation count.
	tf := make(map[string]int32)

	for rows.Next() {
		var chunk string
		var ci int
		var private int
		var space string
		var blob []byte
		// The note path repeats for every chunk of a note. Scanning it as raw
		// bytes and only materializing a string when the note is NEW turns
		// one allocation per chunk into one per note; the map lookup on
		// string(note) is compiled without a copy.
		var note sql.RawBytes
		if err := rows.Scan(&note, &chunk, &ci, &blob, &private, &space); err != nil {
			return nil, err
		}

		ni, ok := noteIdx[string(note)]
		if !ok {
			path := string(note)
			// The scan this replaced was an INNER JOIN against notes, so a
			// chunk whose note row is gone was excluded. That exclusion is
			// load-bearing, not incidental: a rebuild that clears notes but
			// leaves vectors behind has happened here before, and without this
			// check those orphans would surface as untitled search results
			// still carrying the deleted note's text.
			title, exists := titles[path]
			if !exists {
				continue
			}
			ni = int32(len(c.notes))
			noteIdx[path] = ni
			c.noteIdx[path] = ni
			c.notes = append(c.notes, noteMeta{path: path, title: title})
		}

		if c.dim == 0 {
			c.dim = len(blob) / 4
			c.vecs = make([]float32, 0, c.dim*max(n, 1))
		}
		// Decode straight into the arena. Unpacking into a temporary slice and
		// copying it in allocated two vectors per row and threw both away —
		// at 256 dimensions that was 2 KB of garbage per chunk, the single
		// largest source of allocation in a rebuild.
		base := len(c.vecs)
		// Grow (a no-op when the arena was pre-sized correctly) and reslice,
		// rather than appending a freshly made slice — that would allocate the
		// very per-row temporary this is here to avoid.
		c.vecs = slices.Grow(c.vecs, c.dim)[:base+c.dim]
		row := c.vecs[base : base+c.dim]
		clear(row)
		// A row whose width disagrees with the arena cannot be stored inline.
		// Cosine already scores dimension mismatches 0, so a mismatched row is
		// left zeroed: a re-embed with a different model is a rebuild, not
		// something to score against.
		if len(blob)/4 == c.dim {
			decodeInto(row, blob)
		}

		var sq float64
		for _, x := range row {
			sq += float64(x) * float64(x)
		}
		c.norms = append(c.norms, math.Sqrt(sq))

		for k := range tf {
			delete(tf, k)
		}
		total := int32(0)
		for _, t := range wordRE.FindAllString(strings.ToLower(chunk), -1) {
			tf[t]++
			total++
		}
		rowIdx := int32(len(c.rows))
		for t, n := range tf {
			c.postings[t] = append(c.postings[t], posting{row: rowIdx, tf: n})
		}

		isPrivate := private != 0
		c.byNote[c.notes[ni].path] = append(c.byNote[c.notes[ni].path], rowIdx)
		c.rows = append(c.rows, cachedRow{
			note: ni, ci: int32(ci), total: total,
			chars: int32(len(chunk)), space: c.spaceID(space), private: isPrivate,
		})
		c.nAll++
		c.lenAll += float64(total)
		c.charsAll += int64(len(chunk))
		if !isPrivate {
			c.nPublic++
			c.lenPublic += float64(total)
			c.charsPublic += int64(len(chunk))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Postings are appended in row order, so each list is already ascending by
	// row; accumulation order over a row's terms is fixed by the sorted term
	// loop in Retrieve, not by this.
	c.identity = make([]int32, len(c.rows))
	for i := range c.identity {
		c.identity[i] = int32(i)
	}
	return c, nil
}

// noteTitles maps note path to title. Titles are only ever needed for the few
// notes that reach a response, but the map is small (one entry per note) and
// building it once is far cheaper than joining it onto every chunk.
func (ix *Index) noteTitles() (map[string]string, error) {
	rows, err := ix.DB.Query("SELECT path, title FROM notes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, title string
		if err := rows.Scan(&path, &title); err != nil {
			return nil, err
		}
		out[path] = title
	}
	return out, rows.Err()
}

// CorpusStats reports the size of the retrievable corpus: how many chunks and
// notes it holds and how many characters of chunk text that is. Cheap — the
// character total is accumulated when the cache is built.
func (ix *Index) CorpusStats(includePrivate bool) (chunks, notes int, chars int64, err error) {
	return ix.CorpusStatsFor(Everything(includePrivate))
}

// CorpusStatsFor is CorpusStats over what a principal may read, so the
// corpus-fits decision is made against the corpus that caller would receive.
func (ix *Index) CorpusStatsFor(f Filter) (chunks, notes int, chars int64, err error) {
	err = ix.withCache(func(c *corpusCache) error {
		chunks, notes, chars = c.stats(f.IncludePrivate, c.allowedSpaces(f.Spaces))
		return nil
	})
	return chunks, notes, chars, err
}

func (c *corpusCache) stats(includePrivate bool, allowed []bool) (chunks, notes int, chars int64) {
	seen := make(map[int32]bool, len(c.notes))
	for i := range c.rows {
		// Tombstoned rows belong to notes that have since been rewritten or
		// deleted; counting them would report a corpus larger than the one
		// that can be retrieved, and the corpus-fits decision reads this.
		r := &c.rows[i]
		if r.dead || (!includePrivate && r.private) {
			continue
		}
		if allowed != nil && !(int(r.space) < len(allowed) && allowed[r.space]) {
			continue
		}
		chunks++
		chars += int64(r.chars)
		seen[r.note] = true
	}
	if allowed == nil {
		if includePrivate {
			return chunks, len(seen), c.charsAll
		}
		return chunks, len(seen), c.charsPublic
	}
	return chunks, len(seen), chars
}

// WholeCorpus returns every retrievable chunk in document order.
//
// For a corpus small enough to fit a caller's budget this is a better context
// than any ranking of it: retrieval exists to choose what to leave out, and
// when nothing has to be left out, choosing can only lose information. The
// benchmarks show exactly that — on LoCoMo, whose conversations fit a reader's
// window, handing over the whole transcript beats retrieving from it by 5.5
// points.
func (ix *Index) WholeCorpus(includePrivate bool) ([]Hit, error) {
	return ix.WholeCorpusFor(Everything(includePrivate))
}

// WholeCorpusFor is WholeCorpus restricted to what a principal may read.
func (ix *Index) WholeCorpusFor(f Filter) ([]Hit, error) {
	q := "SELECT v.note, n.title, v.chunk FROM vectors v JOIN notes n ON n.path=v.note"
	where := []string{}
	args := []any{}
	if !f.IncludePrivate {
		where = append(where, "v.private=0")
	}
	if f.Spaces != nil {
		// An empty allow-list must return nothing rather than everything, so
		// the clause is built even when there is nothing to allow.
		names := make([]string, 0, len(f.Spaces))
		for s, ok := range f.Spaces {
			if ok {
				names = append(names, s)
			}
		}
		if len(names) == 0 {
			return nil, nil
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
		where = append(where, "v.space IN ("+ph+")")
		for _, n := range names {
			args = append(args, n)
		}
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY v.note, v.chunk_idx"
	rows, err := ix.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Path, &h.Title, &h.Chunk); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// InvalidateCache drops the retrieval cache. Writes bump the revision, which
// is normally enough; this exists for callers that mutate the index tables
// out of band.
func (ix *Index) InvalidateCache() {
	ix.cacheMu.Lock()
	ix.cache = nil
	ix.cacheMu.Unlock()
}

// CacheStats reports the resident cost of the retrieval cache, so an operator
// can see it rather than infer it. Vectors dominate: one float32 per dimension
// per chunk, 1 KB per chunk at the 256-wide default.
func (ix *Index) CacheStats() (chunks, notes, terms int, vectorBytes int64) {
	ix.cacheMu.RLock()
	defer ix.cacheMu.RUnlock()
	if ix.cache == nil {
		return 0, 0, 0, 0
	}
	return len(ix.cache.rows), len(ix.cache.notes), len(ix.cache.postings),
		int64(len(ix.cache.vecs)) * 4
}

// ------------------------------------------------------------------ ranking

// Filter is what a caller is allowed to see.
//
// Spaces nil means every space: a deployment with no accounts, or an
// administrator. A non-nil set is an allow-list, and an EMPTY non-nil set
// means nothing is visible — which is what an unauthenticated caller gets on a
// multi-user deployment, and must not be confused with "unset".
type Filter struct {
	IncludePrivate bool
	Spaces         map[string]bool
}

// Everything is the filter a single-user deployment retrieves with.
func Everything(includePrivate bool) Filter {
	return Filter{IncludePrivate: includePrivate}
}

// Retrieve ranks chunks against a query. Private chunks are excluded unless
// includePrivate is set — the default has to be exclusion, since this feeds
// surfaces that are not necessarily authenticated.
func (ix *Index) Retrieve(query string, k int, includePrivate bool) ([]Hit, error) {
	return ix.RetrieveFor(query, k, Everything(includePrivate))
}

// RetrieveFor is Retrieve restricted to what a principal may read.
//
// The restriction is applied INSIDE ranking rather than to its output. BM25
// scores against corpus statistics — document frequency, corpus size, average
// document length — so a filter applied afterwards would score every visible
// chunk against a corpus that includes notes the caller cannot see. The
// ranking would then depend on their contents, which is a slow leak of exactly
// the thing a space is for. The private-note filter has always worked this way;
// spaces reuse the mechanism.
func (ix *Index) RetrieveFor(query string, k int, f Filter) ([]Hit, error) {
	var hits []Hit
	err := ix.withCache(func(c *corpusCache) error {
		var err error
		hits, err = ix.rank(c, query, k, f)
		return err
	})
	return hits, err
}

// rank is Retrieve's body, run with the cache read lock held.
func (ix *Index) rank(c *corpusCache, query string, k int, f Filter) ([]Hit, error) {
	includePrivate := f.IncludePrivate
	allowed := c.allowedSpaces(f.Spaces)
	n := c.countFor(includePrivate, allowed)
	if n == 0 || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	qv := ix.Emb.Embed([]string{query})[0]
	qterms := queryTerms(query)

	nChunks := float64(n)
	avglen := c.avgLenFor(includePrivate, allowed)

	// visible reports whether a row participates. Every statistic below —
	// document frequency, corpus size, average length — is computed over the
	// visible subset only, because BM25 IDF depends on corpus composition: a
	// private note must not shift the scores of public results, or the private
	// corpus would be observable through the ranking of the public one.
	visible := func(r *cachedRow) bool {
		if r.dead || (!includePrivate && r.private) {
			return false
		}
		return allowed == nil || (int(r.space) < len(allowed) && allowed[r.space])
	}

	// --- lexical leg: BM25 over the inverted index ---
	lex := make([]float64, len(c.rows))
	for _, t := range qterms {
		plist := c.postings[t]
		if len(plist) == 0 {
			continue
		}
		df := 0
		for i := range plist {
			if visible(&c.rows[plist[i].row]) {
				df++
			}
		}
		if df == 0 {
			continue
		}
		idf := math.Log(nChunks / float64(df))
		for i := range plist {
			p := plist[i]
			r := &c.rows[p.row]
			if !visible(r) {
				continue
			}
			tf := float64(p.tf)
			norm := bm25K1 * (1 - bm25B + bm25B*float64(r.total)/avglen)
			lex[p.row] += idf * tf * (bm25K1 + 1) / (tf + norm)
		}
	}

	// --- dense leg: cosine over the flat arena ---
	var qsq float64
	for _, x := range qv {
		qsq += float64(x) * float64(x)
	}
	qnorm := math.Sqrt(qsq)

	// candidate is a value type in one contiguous slice: the original allocated
	// a *scored per candidate, which at 50k chunks was 50k pointer-chased
	// objects per query.
	// Iterated in CORPUS order — by note path, then chunk index — not in the
	// order rows happen to sit in the arena. For a freshly built cache those
	// are the same thing; for one that has been patched in place they are not,
	// and every tie below is broken by candidate position. Going through the
	// ordering view is what makes a patched cache rank identically to a
	// rebuilt one, which is the property the golden digest pins.
	order := c.order()
	cosines := c.cosines(order, visible, qv, qnorm)
	cands := make([]candidate, 0, 256)
	for pos, i := range order {
		if !visible(&c.rows[i]) {
			continue
		}
		cos := cosines[pos]
		if cos > 0 || lex[i] > 0 {
			cands = append(cands, candidate{row: i, cos: cos, lex: lex[i]})
		}
	}
	if len(cands) == 0 {
		return nil, nil
	}

	// --- reciprocal-rank fusion: each ranking contributes 1/(60+rank) ---
	//
	// Both legs are ranked from the SAME starting permutation. A stable sort's
	// tie handling is defined by its input order, so ranking the second leg
	// from the first leg's output would let the dense ordering decide how
	// lexical ties break — a different ranking, arrived at silently.
	perm := make([]int32, len(cands))

	// The dense leg has to rank every candidate: cosine is nonzero almost
	// everywhere, so there is no sparsity to exploit.
	identity(perm)
	slices.SortFunc(perm, func(a, b int32) int {
		if c := cmpDesc(cands[a].cos, cands[b].cos); c != 0 {
			return c
		}
		return int(a - b)
	})
	for rank, idx := range perm {
		cands[idx].rrf += 1.0 / (rrfK + float64(rank))
	}

	// The lexical leg is sparse, and that is worth exploiting exactly rather
	// than approximately. BM25 is zero for any chunk containing none of the
	// query terms, and it can never be negative: its IDF is log(N/df) with
	// df <= N. So every zero-scoring chunk ties at the bottom, and a stable
	// descending sort would order that entire tail by index — which is the
	// order they are already in. Only the chunks an inverted-index lookup
	// actually touched need sorting, turning an N-log-N sort of the whole
	// corpus into an m-log-m sort of the matches.
	nz := perm[:0]
	for i := range cands {
		if cands[i].lex > 0 {
			nz = append(nz, int32(i))
		}
	}
	slices.SortFunc(nz, func(a, b int32) int {
		if c := cmpDesc(cands[a].lex, cands[b].lex); c != 0 {
			return c
		}
		return int(a - b)
	})
	rank := 0
	for _, idx := range nz {
		cands[idx].rrf += 1.0 / (rrfK + float64(rank))
		rank++
	}
	for i := range cands {
		if cands[i].lex <= 0 {
			cands[i].rrf += 1.0 / (rrfK + float64(rank))
			rank++
		}
	}

	identity(perm)
	slices.SortFunc(perm, func(a, b int32) int {
		if c := cmpDesc(cands[a].rrf, cands[b].rrf); c != 0 {
			return c
		}
		return int(a - b)
	})

	return ix.finalize(c, cands, perm, k)
}

// identity resets a permutation buffer to 0..n-1.
func identity(order []int32) {
	for i := range order {
		order[i] = int32(i)
	}
}

// candidate is one chunk in contention, carrying both legs' raw scores and the
// fused result.
type candidate struct {
	row      int32
	cos, lex float64
	rrf      float64
}

// mergeTopHits is how many returned hits are expanded into a query-focused
// excerpt of their note; mergeChunksPerHit bounds how many of that note's
// chunks the excerpt may draw on.
//
// Three chunks mirrors the lexical leg, whose excerpt budget is 2400
// characters against an 800-character chunk target — so both halves of a
// search return about the same amount of any one note, which is the property
// that made that leg carry most of the pipeline's coverage.
const (
	mergeTopHits      = 3
	mergeChunksPerHit = 3
)

// cmpDesc orders descending, reproducing the `a > b` less-function the ranking
// was defined with: everything that is not strictly greater compares equal.
//
// Every caller breaks the remaining ties by ascending index, and that tiebreak
// is load-bearing. A stable descending sort leaves equal keys in their
// original index order, so spelling that out makes the comparison a total
// order — and once no two distinct elements compare equal, an UNSTABLE sort
// has only one valid answer: the one the stable sort produced. That is what
// allows pdqsort here in place of a stable merge, without moving the ranking.
//
// The comparators are written out at each call site rather than routed through
// a key function. An indirect call per comparison, twice per comparison, cost
// more than the sort algorithm saved.
func cmpDesc(a, b float64) int {
	switch {
	case a > b:
		return -1
	case b > a:
		return 1
	}
	return 0
}

// finalize turns the fused ranking into the returned passages.
//
// Three stages that materially change what an agent sees:
//
//   - per-note de-duplication, so one long note cannot occupy every slot: the
//     best chunk from each note comes first, then the remainder fills in;
//   - small-to-big — the top 3 hits are returned WITH their neighbouring chunks
//     merged in, because a chunk boundary often cuts the sentence that answers
//     the question. Merged neighbours are marked covered so they cannot also be
//     returned on their own;
//   - chunk bodies are read here and only here. Ranking works on token counts
//     and vectors, so the text of the thousands of chunks that lost is never
//     fetched — only the handful being returned.
func (ix *Index) finalize(c *corpusCache, cands []candidate, order []int32, k int) ([]Hit, error) {
	// Both lists are bounded rather than corpus-sized. The response holds at
	// most k entries, and the only reason a ranked entry is ever skipped is
	// that neighbour merging already covered it — at most 3 marks for each of
	// the first three results and one for each result after, so no more than
	// 2k+9 entries of the concatenation can ever be read. Building the full
	// lists instead was the last allocation in this path that scaled with the
	// corpus: 50k Hit values, each with three string headers, materialized per
	// query so that eight of them could be returned.
	limit := 2*k + 9
	if k <= 0 { // k<=0 means "no cap on the response", so nothing can be pruned
		limit = len(order)
	}
	seen := make(map[int32]bool, limit)
	primary := make([]int32, 0, limit)
	extra := make([]int32, 0, limit)
	for _, idx := range order {
		if len(primary) >= limit && len(extra) >= limit {
			break
		}
		note := c.rows[cands[idx].row].note
		if seen[note] {
			if len(extra) < limit {
				extra = append(extra, idx)
			}
		} else if len(primary) < limit {
			primary = append(primary, idx)
		}
		seen[note] = true
	}
	ordered := append(primary, extra...)

	type key struct {
		note int32
		ci   int32
	}
	covered := map[key]bool{}
	out := []Hit{}
	for _, idx := range ordered {
		if k > 0 && len(out) >= k {
			break
		}
		cand := &cands[idx]
		r := &c.rows[cand.row]
		if covered[key{r.note, r.ci}] {
			continue
		}
		h := Hit{
			Path:     c.notes[r.note].path,
			Title:    c.notes[r.note].title,
			ChunkIdx: int(r.ci),
			Score:    math.Round(cand.rrf*10000) / 10000,
		}
		if len(out) < mergeTopHits {
			// Small-to-big, selected by RELEVANCE within the note rather than
			// by adjacency.
			//
			// The chunk leg emits one chunk per note, so a fact sitting
			// anywhere but its note's best-matching chunk is unreachable:
			// chunks-only coverage saturates at 43% on the LongMemEval dev
			// split whether k is 10, 25 or 30, because raising k only adds
			// more notes' FIRST chunks. Merging chunk_idx +/-1 helps only when
			// the fact happens to sit next door, and on that split it does
			// not — evidence ranked past 30 was a later chunk of an
			// already-seen note in 100% of cases.
			//
			// So a hit brings its note's other high-scoring chunks with it, in
			// document order with an elision marker: the same shape the
			// lexical leg's excerpt returns, which is why that leg carries most
			// of this pipeline's coverage. Crucially this keeps ONE entry per
			// note, so response breadth is unchanged (10.96 distinct sessions
			// per context, against 10.96 for adjacency merging); only the entry
			// itself gets richer.
			byIdx := map[int32]bool{r.ci: true}
			for _, other := range order {
				if len(byIdx) >= mergeChunksPerHit {
					break
				}
				o := &c.rows[cands[other].row]
				if o.note != r.note || byIdx[o.ci] {
					continue
				}
				byIdx[o.ci] = true
			}
			want := make([]int32, 0, len(byIdx)+2)
			for ci := range byIdx {
				want = append(want, ci)
			}
			// the hit's own neighbours still count: a chunk boundary often
			// cuts the sentence that answers the question
			want = append(want, r.ci-1, r.ci+1)
			slices.Sort(want)
			want = slices.Compact(want)

			parts := make([]string, 0, len(want))
			prev := int32(-99)
			for _, ci := range want {
				if ci < 0 {
					continue
				}
				var chunk string
				if err := ix.DB.QueryRow(
					"SELECT chunk FROM vectors WHERE note=? AND chunk_idx=?",
					h.Path, ci).Scan(&chunk); err != nil {
					continue // a gap in the note is not an error here
				}
				if prev >= 0 && ci != prev+1 {
					parts = append(parts, "[…]")
				}
				parts = append(parts, chunk)
				covered[key{r.note, ci}] = true
				prev = ci
			}
			if len(parts) > 0 {
				h.Chunk = strings.Join(parts, "\n")
			}
		} else {
			covered[key{r.note, r.ci}] = true
			if err := ix.DB.QueryRow(
				"SELECT chunk FROM vectors WHERE note=? AND chunk_idx=?",
				h.Path, h.ChunkIdx).Scan(&h.Chunk); err != nil {
				return nil, err
			}
		}
		out = append(out, h)
	}
	return out, nil
}

// queryTerms returns the query's distinct terms in sorted order.
//
// The order is not cosmetic. BM25 sums one contribution per term, and float64
// addition is not associative, so iterating a map here made a chunk's score
// differ in the last bits from one call to the next. RRF then turns scores
// into RANKS, which amplifies that: two chunks a single ULP apart swap places,
// every chunk below them shifts, and the fused score changes for the whole
// tail. Measured on the parity corpus before this was pinned, 8 of 80
// (query, k, privacy) combinations were unstable, one of them returning 28
// distinct result sets in 60 identical runs.
//
// Sorting costs nothing at query-term scale and makes the summation order a
// property of the query text alone.
func queryTerms(s string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, 8)
	for _, t := range wordRE.FindAllString(strings.ToLower(s), -1) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// cacheFields is embedded in Index; kept here so the cache's storage lives
// beside the code that owns it.
type cacheFields struct {
	cacheMu sync.RWMutex
	cache   *corpusCache
	// bulk suppresses per-note cache patching during a rebuild or a sync,
	// where dropping the cache once is far cheaper than patching every note.
	// Guarded by the index write lock, which every bulk operation holds.
	bulk bool
}

// cosines scores every visible row against the query, across CPU cores.
//
// This is the one term in a query that grows with the corpus: the dense leg has
// no sparsity to exploit — cosine is non-zero almost everywhere — so every
// chunk is scored on every query. At 50,000 notes that sweep is most of a
// query's 53ms, and it is embarrassingly parallel: each row reads its own slice
// of the arena and writes its own slot.
//
// Splitting it is not free at small sizes (goroutine setup outweighs the work),
// so a small corpus stays sequential and gets exactly the code it had.
//
// The result is identical either way, deliberately: the scores are written into
// a fixed-position slice and consumed in corpus order afterwards, so nothing
// about ranking — including how ties break — depends on how many cores ran it.
func (c *corpusCache) cosines(order []int32, visible func(*cachedRow) bool,
	qv []float32, qnorm float64) []float64 {
	out := make([]float64, len(order))
	score := func(lo, hi int) {
		for pos := lo; pos < hi; pos++ {
			i := order[pos]
			if !visible(&c.rows[i]) {
				continue
			}
			cos := c.cosine(int(i), qv, qnorm)
			// A NaN would make the comparator non-transitive and hand pdqsort
			// an ordering it is entitled to resolve arbitrarily. Cosine's
			// zero-norm guards mean one should not arise; clamping makes that
			// a property of the code rather than of an argument about it.
			if math.IsNaN(cos) {
				cos = 0
			}
			out[pos] = cos
		}
	}

	workers := runtime.GOMAXPROCS(0)
	const parallelFrom = 4096
	if workers <= 1 || len(order) < parallelFrom {
		score(0, len(order))
		return out
	}
	if workers > 8 {
		// Past a handful of cores this is memory-bandwidth bound, and more
		// goroutines buy scheduling rather than speed.
		workers = 8
	}
	chunk := (len(order) + workers - 1) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo := w * chunk
		if lo >= len(order) {
			break
		}
		hi := min(lo+chunk, len(order))
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			score(lo, hi)
		}(lo, hi)
	}
	wg.Wait()
	return out
}
