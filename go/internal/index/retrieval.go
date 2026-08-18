package index

import (
	"database/sql"
	"math"
	"regexp"
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

// cachedRow is one chunk, reduced to what ranking actually reads. Note the
// absence of the chunk TEXT: scoring never looks at it, only the handful of
// rows that survive into the response do, and those are cheap to fetch by
// primary key at the end. Keeping bodies here would have been the single
// largest allocation in the process for no benefit.
type cachedRow struct {
	note    int32 // index into corpusCache.notes
	ci      int32 // chunk_idx within the note
	total   int32 // token count, for BM25 length normalization
	private bool
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

	// Corpus statistics, kept for both visibilities so a query never has to
	// sweep the corpus just to learn its size. Accumulated in row order, which
	// is the order the original summed them in.
	nAll, nPublic     int
	lenAll, lenPublic float64
}

func (c *corpusCache) count(includePrivate bool) int {
	if includePrivate {
		return c.nAll
	}
	return c.nPublic
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
	ix.cacheMu.RUnlock()
	if c != nil && c.rev == rev {
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
		"SELECT note, chunk, chunk_idx, embedding, private FROM vectors " +
			"ORDER BY note, chunk_idx")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	c := &corpusCache{rev: rev, postings: make(map[string][]posting)}
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
		var blob []byte
		// The note path repeats for every chunk of a note. Scanning it as raw
		// bytes and only materializing a string when the note is NEW turns
		// one allocation per chunk into one per note; the map lookup on
		// string(note) is compiled without a copy.
		var note sql.RawBytes
		if err := rows.Scan(&note, &chunk, &ci, &blob, &private); err != nil {
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
		c.rows = append(c.rows, cachedRow{
			note: ni, ci: int32(ci), total: total, private: isPrivate,
		})
		c.nAll++
		c.lenAll += float64(total)
		if !isPrivate {
			c.nPublic++
			c.lenPublic += float64(total)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Postings are appended in row order, so each list is already ascending by
	// row; accumulation order over a row's terms is fixed by the sorted term
	// loop in Retrieve, not by this.
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

// Retrieve ranks chunks against a query. Private chunks are excluded unless
// includePrivate is set — the default has to be exclusion, since this feeds
// surfaces that are not necessarily authenticated.
func (ix *Index) Retrieve(query string, k int, includePrivate bool) ([]Hit, error) {
	c, err := ix.corpusCacheFor()
	if err != nil {
		return nil, err
	}
	n := c.count(includePrivate)
	if n == 0 || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	qv := ix.Emb.Embed([]string{query})[0]
	qterms := queryTerms(query)

	nChunks := float64(n)
	avglen := c.avgLen(includePrivate)

	// visible reports whether a row participates. Every statistic below —
	// document frequency, corpus size, average length — is computed over the
	// visible subset only, because BM25 IDF depends on corpus composition: a
	// private note must not shift the scores of public results, or the private
	// corpus would be observable through the ranking of the public one.
	visible := func(r *cachedRow) bool { return includePrivate || !r.private }

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
	cands := make([]candidate, 0, 256)
	for i := range c.rows {
		if !visible(&c.rows[i]) {
			continue
		}
		cos := c.cosine(i, qv, qnorm)
		// A NaN would make the comparator non-transitive and hand pdqsort an
		// ordering it is entitled to resolve arbitrarily. Cosine's zero-norm
		// guards mean one should not arise; clamping makes that a property of
		// the code rather than of an argument about the code.
		if math.IsNaN(cos) {
			cos = 0
		}
		if cos > 0 || lex[i] > 0 {
			cands = append(cands, candidate{row: int32(i), cos: cos, lex: lex[i]})
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
	order := make([]int32, len(cands))

	// The dense leg has to rank every candidate: cosine is nonzero almost
	// everywhere, so there is no sparsity to exploit.
	identity(order)
	slices.SortFunc(order, func(a, b int32) int {
		if c := cmpDesc(cands[a].cos, cands[b].cos); c != 0 {
			return c
		}
		return int(a - b)
	})
	for rank, idx := range order {
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
	nz := order[:0]
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

	identity(order)
	slices.SortFunc(order, func(a, b int32) int {
		if c := cmpDesc(cands[a].rrf, cands[b].rrf); c != 0 {
			return c
		}
		return int(a - b)
	})

	return ix.finalize(c, cands, order, k)
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
		if len(out) < 3 {
			rows, err := ix.DB.Query(
				"SELECT chunk_idx, chunk FROM vectors WHERE note=? "+
					"AND chunk_idx BETWEEN ? AND ? ORDER BY chunk_idx",
				h.Path, h.ChunkIdx-1, h.ChunkIdx+1)
			if err != nil {
				return nil, err
			}
			var parts []string
			for rows.Next() {
				var ci int
				var chunk string
				if err := rows.Scan(&ci, &chunk); err != nil {
					rows.Close()
					return nil, err
				}
				parts = append(parts, chunk)
				covered[key{r.note, int32(ci)}] = true
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
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
}
