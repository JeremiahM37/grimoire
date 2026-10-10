package index

import (
	"container/heap"
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Candidate generation for recall above the scan bound.
//
// The window scored the newest N facts, which is correct only while the whole
// store fits in it. Past the bound, a recall takes its candidates from four
// arms over the same filtered set, unions them, fetches the full rows for the
// union, applies accept, and ranks with rankMemory, unchanged:
//
//   - newest:   the newest pool facts, so recent material always takes part;
//   - lexical:  the pool facts with the best FTS5 bm25 for the query words;
//   - entity:   the newest pool facts that share a stored entity with the query;
//   - semantic: the pool facts with the highest cosine to the query vector,
//     found by a first pass that decodes only the embedding column.
//
// pool is scanLimit/10, with a floor, so the candidate set is bounded at four
// pools regardless of corpus size: 8,000 rows at the default bound.

// minPool keeps tiny test bounds from producing empty arms.
const minPool = 10

// fetchChunk bounds the IN list of the final row fetch, under SQLite's
// variable limit on old builds.
const fetchChunk = 500

func (q MemoryQuery) poolSize(limit int) int {
	if p := limit / 10; p > minPool {
		return p
	}
	return minPool
}

// sqlVisibility is the space and reader-list rule in SQL: the same decision as
// allows, pushed down so that the scan bound counts only rows this caller can
// read. allows stays the authority; this is an exact image of it, and the
// leak probe in internal/api exercises both together.
func (f Filter) sqlVisibility() ([]string, []any) {
	var where []string
	var args []any
	if f.Spaces != nil {
		names := make([]string, 0, len(f.Spaces))
		for s, ok := range f.Spaces {
			if ok {
				names = append(names, s)
			}
		}
		if len(names) == 0 {
			where = append(where, "0")
		} else {
			sort.Strings(names)
			marks := make([]string, len(names))
			for i, s := range names {
				marks[i] = "?"
				args = append(args, s)
			}
			where = append(where, "space IN ("+strings.Join(marks, ",")+")")
		}
	}
	if !f.IgnoreACLs {
		if f.User == "" {
			// aclAllows with no user admits only notes without a reader list.
			where = append(where, "acl=''")
		} else {
			where = append(where, "(acl='' OR instr(acl,?)>0)")
			args = append(args, ","+f.User+",")
		}
	}
	return where, args
}

// ftsMatch turns query terms into an FTS5 expression. Each term is quoted so
// that punctuation in a term cannot be read as FTS syntax; terms are OR-ed,
// because bm25 already rewards a fact that matches several of them.
func ftsMatch(terms []string) string {
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " OR ")
}

// memoryEntriesCandidates is the over-bound path of MemoryEntries.
func (ix *Index) memoryEntriesCandidates(q MemoryQuery, where string, args []any, limit int) ([]MemoryHit, error) {
	pool := q.poolSize(limit)
	set := map[int64]bool{}
	take := func(query string, qargs ...any) error {
		rows, err := ix.DB.Query(query, qargs...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var rid int64
			if err := rows.Scan(&rid); err != nil {
				return err
			}
			set[rid] = true
		}
		return rows.Err()
	}
	with := func(extra ...any) []any {
		out := make([]any, 0, len(args)+len(extra))
		out = append(out, args...)
		return append(out, extra...)
	}

	// newest
	if err := take("SELECT rowid FROM memory_entries WHERE "+where+
		" ORDER BY stamp DESC, id DESC LIMIT ?", with(pool)...); err != nil {
		return nil, err
	}

	// lexical: FTS5 top by bm25. The join applies the same predicates as the
	// window, so the top-N is taken among rows the caller may read.
	if terms := memory.Tokens(q.Query); len(terms) > 0 {
		if err := take("SELECT memory_entries.rowid FROM memory_fts"+
			" JOIN memory_entries ON memory_entries.rowid = memory_fts.rowid"+
			" WHERE memory_fts MATCH ? AND "+where+
			" ORDER BY bm25(memory_fts) LIMIT ?",
			with2(ftsMatch(terms), args, pool)...); err != nil {
			return nil, err
		}
	}

	// entity: rows sharing a stored entity with the query, newest first.
	if ents := memory.Entities(q.Query); len(ents) > 0 {
		marks := make([]string, len(ents))
		eargs := make([]any, len(ents))
		for i, e := range ents {
			marks[i] = "?"
			eargs[i] = e
		}
		if err := take("SELECT rowid FROM memory_entries WHERE "+where+
			" AND (note,id) IN (SELECT note,id FROM memory_entities WHERE entity IN ("+
			strings.Join(marks, ",")+"))"+
			" ORDER BY stamp DESC, id DESC LIMIT ?",
			with(append(eargs, pool)...)...); err != nil {
			return nil, err
		}
	}

	// semantic: brute-force cosine, keeping the best pool. Only the embedding
	// column is decoded here; full rows are fetched for the survivors below.
	qVec := q.QueryVector
	if len(qVec) == 0 && !q.LexicalOnly && strings.TrimSpace(q.Query) != "" {
		qVec = firstVec(ix.Emb.Embed([]string{q.Query}))
	}
	if len(qVec) > 0 {
		top, err := ix.topByVector(qVec, where, args, pool)
		if err != nil {
			return nil, err
		}
		for _, rid := range top {
			set[rid] = true
		}
	}

	rids := make([]int64, 0, len(set))
	for rid := range set {
		rids = append(rids, rid)
	}
	sort.Slice(rids, func(i, j int) bool { return rids[i] < rids[j] })

	cands, err := ix.fetchMemoryRows(rids, q)
	if err != nil {
		return nil, err
	}
	// Order the pool by recency before ranking. rankMemory breaks score ties
	// by stamp and id, but the order of rows it receives still decides IDF
	// ties and stable-sort output, and rowid order is an accident of writes.
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i].hit, cands[j].hit
		if a.Stamp != b.Stamp {
			return a.Stamp > b.Stamp
		}
		if a.ID != b.ID {
			return a.ID > b.ID
		}
		return a.Note < b.Note
	})
	q.QueryVector = qVec
	return ix.rankMemory(cands, q), nil
}

// with2 builds the argument list of the lexical arm, whose MATCH expression
// comes before the shared predicates.
func with2(match string, args []any, pool int) []any {
	out := make([]any, 0, len(args)+2)
	out = append(out, match)
	out = append(out, args...)
	return append(out, pool)
}

// scoredRow is one heap entry in the semantic arm.
type scoredRow struct {
	score float64
	rid   int64
}

// minHeap keeps the best k entries; the root is the weakest kept.
type minHeap []scoredRow

func (h minHeap) Len() int { return len(h) }
func (h minHeap) Less(i, j int) bool {
	if h[i].score != h[j].score {
		return h[i].score < h[j].score
	}
	return h[i].rid > h[j].rid
}
func (h minHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x interface{}) { *h = append(*h, x.(scoredRow)) }
func (h *minHeap) Pop() interface{} {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// topByVector returns the rowids of the k rows whose stored embedding is
// closest to qVec among the rows the predicates admit.
func (ix *Index) topByVector(qVec []float32, where string, args []any, k int) ([]int64, error) {
	rows, err := ix.DB.Query("SELECT rowid, embedding FROM memory_entries WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	qNorm := norm(qVec)
	h := &minHeap{}
	for rows.Next() {
		var rid int64
		var blob []byte
		if err := rows.Scan(&rid, &blob); err != nil {
			return nil, err
		}
		if len(blob) == 0 {
			continue
		}
		s := cosineNorm(qVec, qNorm, Unpack(blob))
		if h.Len() < k {
			heap.Push(h, scoredRow{s, rid})
		} else if top := (*h)[0]; s > top.score || (s == top.score && rid < top.rid) {
			(*h)[0] = scoredRow{s, rid}
			heap.Fix(h, 0)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]int64, 0, h.Len())
	for _, e := range *h {
		out = append(out, e.rid)
	}
	return out, nil
}

// fetchMemoryRows loads full rows for the given rowids and keeps those that
// pass accept.
func (ix *Index) fetchMemoryRows(rids []int64, q MemoryQuery) ([]memoryRow, error) {
	var out []memoryRow
	for start := 0; start < len(rids); start += fetchChunk {
		end := start + fetchChunk
		if end > len(rids) {
			end = len(rids)
		}
		chunk := rids[start:end]
		marks := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, rid := range chunk {
			marks[i] = "?"
			args[i] = rid
		}
		rows, err := ix.DB.Query("SELECT rowid, "+memoryColumns+
			" FROM memory_entries WHERE rowid IN ("+strings.Join(marks, ",")+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var rid int64
			r, err := scanMemoryRow(func(dest ...any) error {
				return rows.Scan(append([]any{&rid}, dest...)...)
			})
			if err != nil {
				rows.Close()
				return nil, err
			}
			if q.accept(r) {
				out = append(out, r)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}
