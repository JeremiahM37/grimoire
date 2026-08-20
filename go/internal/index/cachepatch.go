package index

import (
	"database/sql"
	"math"
	"slices"
	"sort"
	"strings"
)

// Keeping the retrieval cache current without rebuilding it.
//
// The cache is the query-independent half of retrieval: every chunk's vector in
// one flat arena, an inverted index over their terms, and the corpus statistics
// BM25 needs. It was rebuilt from SQLite whenever the index revision changed —
// which is to say, on the first query after ANY write. Measured on synthetic
// vaults, that rebuild costs 45ms at 1,000 notes, 464ms at 10,000 and 2.2s at
// 50,000, and it is paid by whoever queries next. A vault with people or
// connectors writing into it continuously is therefore permanently rebuilding,
// and the cost grows with the corpus rather than with the change.
//
// Nothing about a one-note write justifies that. Patching replaces just that
// note's rows: the old ones are tombstoned and the new ones appended, with the
// corpus statistics adjusted by the difference.
//
// Two things make it safe to do in place:
//
//   - Rows are tombstoned, never removed. The postings lists address rows by
//     position, so removing one would shift every position after it. Scans skip
//     dead rows, and once enough accumulate the cache is dropped and rebuilt
//     compactly — churn is bounded, not unbounded.
//   - Ranking breaks ties by content (note key, chunk index) rather than by
//     position in the arena. Otherwise a patched cache and a rebuilt one would
//     order equal-scoring chunks differently, and the same corpus would answer
//     differently depending on how it got there.

// maxDeadFraction is how much tombstoned space the cache tolerates before a
// rebuild is cheaper than carrying it.
const maxDeadFraction = 0.35

// patchNote brings the live cache in line with one note's rows in SQLite.
//
// Called after the note's rows have been written or deleted, with the index
// write lock held. A bulk operation (rebuild, sync) drops the cache instead:
// patching ten thousand notes one at a time would cost more than one rebuild.
func (ix *Index) patchNote(path string) {
	ix.cacheMu.Lock()
	defer ix.cacheMu.Unlock()
	c := ix.cache
	if c == nil || ix.bulk {
		return
	}

	c.tombstone(path)
	if err := ix.appendNoteRows(c, path); err != nil {
		// A patch that cannot read the new rows must not leave the cache
		// holding a half-updated corpus: drop it and let the next query
		// rebuild from the source of truth.
		ix.cache = nil
		return
	}
	if len(c.rows) > 0 && float64(c.deadRows)/float64(len(c.rows)) > maxDeadFraction {
		ix.cache = nil // rebuild compactly on the next query
		return
	}
	c.rev = ix.Rev()
}

// beginBulk suppresses per-note patching and drops the cache; endBulk restores
// patching. A rebuild or a sync writes thousands of notes, and the next query
// rebuilding once is cheaper than patching each of them.
func (ix *Index) beginBulk() {
	ix.cacheMu.Lock()
	ix.bulk = true
	ix.cache = nil
	ix.cacheMu.Unlock()
}

func (ix *Index) endBulk() {
	ix.cacheMu.Lock()
	ix.bulk = false
	ix.cacheMu.Unlock()
}

// tombstone marks a note's rows dead and subtracts them from the corpus
// statistics, which BM25 reads as corpus size and average length.
func (c *corpusCache) tombstone(path string) {
	for _, r := range c.byNote[path] {
		row := &c.rows[r]
		if row.dead {
			continue
		}
		row.dead = true
		c.deadRows++
		c.nAll--
		c.lenAll -= float64(row.total)
		c.charsAll -= int64(row.chars)
		if !row.private {
			c.nPublic--
			c.lenPublic -= float64(row.total)
			c.charsPublic -= int64(row.chars)
		}
	}
	delete(c.byNote, path)
}

// appendNoteRows reads one note's chunks from SQLite and appends them to the
// cache, mirroring what buildCache does for the whole corpus.
func (ix *Index) appendNoteRows(c *corpusCache, path string) error {
	var title string
	err := ix.DB.QueryRow("SELECT title FROM notes WHERE path=?", path).Scan(&title)
	if err == sql.ErrNoRows {
		return nil // the note was deleted; tombstoning was the whole job
	}
	if err != nil {
		return err
	}

	ni, ok := c.noteIdx[path]
	if !ok {
		ni = int32(len(c.notes))
		c.noteIdx[path] = ni
		c.notes = append(c.notes, noteMeta{path: path, title: title})
	} else {
		c.notes[ni].title = title // a rename or retitle must not keep the old one
	}

	rows, err := ix.DB.Query(
		"SELECT chunk, chunk_idx, embedding, private, space FROM vectors WHERE note=? ORDER BY chunk_idx",
		path)
	if err != nil {
		return err
	}
	defer rows.Close()

	var added []int32
	tf := make(map[string]int32)
	for rows.Next() {
		var chunk string
		var ci, private int
		var space string
		var blob []byte
		if err := rows.Scan(&chunk, &ci, &blob, &private, &space); err != nil {
			return err
		}
		if c.dim == 0 {
			c.dim = len(blob) / 4
		}
		base := len(c.vecs)
		c.vecs = slices.Grow(c.vecs, c.dim)[:base+c.dim]
		row := c.vecs[base : base+c.dim]
		clear(row)
		if c.dim > 0 && len(blob)/4 == c.dim {
			decodeInto(row, blob)
		}
		var sq float64
		for _, x := range row {
			sq += float64(x) * float64(x)
		}
		c.norms = append(c.norms, math.Sqrt(sq))

		clear(tf)
		total := int32(0)
		for _, t := range wordRE.FindAllString(strings.ToLower(chunk), -1) {
			tf[t]++
			total++
		}
		rowIdx := int32(len(c.rows))
		added = append(added, rowIdx)
		for t, n := range tf {
			c.postings[t] = append(c.postings[t], posting{row: rowIdx, tf: n})
		}
		isPrivate := private != 0
		c.byNote[path] = append(c.byNote[path], rowIdx)
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
		return err
	}
	c.insertSorted(added)
	return nil
}

// insertSorted places a note's newly appended rows into the corpus-order view.
//
// The rows themselves live at the end of the arena, because that is the only
// place they can go without moving every vector after them. Corpus order is
// part of retrieval's output — ties are broken by position — so it is kept
// here instead, as an ordering of row positions rather than of the rows.
func (c *corpusCache) insertSorted(added []int32) {
	if len(added) == 0 {
		return
	}
	if c.sorted == nil {
		c.sorted = make([]int32, len(c.rows)-len(added))
		for i := range c.sorted {
			c.sorted[i] = int32(i)
		}
	}
	first := added[0]
	// Dead rows keep their keys, so they take part in the search and the new
	// rows of a rewritten note land next to the ones they replace. Only live
	// rows are ever read back out.
	pos := sort.Search(len(c.sorted), func(i int) bool {
		return !c.beforeRow(c.sorted[i], first)
	})
	c.sorted = slices.Insert(c.sorted, pos, added...)
}
