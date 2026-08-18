package index

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// Hybrid retrieval: reciprocal-rank fusion of embedding cosine with
// IDF-weighted BM25. Port of the exact full-fusion path in server/index.py —
// the one the LoCoMo and LongMemEval numbers were measured on.

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

type corpusRow struct {
	note   string
	title  string
	ci     int
	chunk  string
	vector []float32
	counts map[string]int
	total  int
}

// Retrieve ranks chunks against a query. Private chunks are excluded unless
// includePrivate is set — the default has to be exclusion, since this feeds
// surfaces that are not necessarily authenticated.
func (ix *Index) Retrieve(query string, k int, includePrivate bool) ([]Hit, error) {
	rows, err := ix.corpus(includePrivate)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	qv := ix.Emb.Embed([]string{query})[0]
	qterms := queryTerms(query)

	nChunks := float64(len(rows))
	var totalLen float64
	for _, r := range rows {
		totalLen += float64(r.total)
	}
	avglen := totalLen / nChunks
	if avglen == 0 {
		avglen = 1
	}
	df := map[string]int{}
	for _, r := range rows {
		for _, t := range qterms {
			if r.counts[t] > 0 {
				df[t]++
			}
		}
	}

	type scored struct {
		hit Hit
		cos float64
		lex float64
		rrf float64
	}
	var cands []*scored
	for _, r := range rows {
		cos := Cosine(r.vector, qv)
		lex := bm25(qterms, r, df, nChunks, avglen)
		if cos > 0 || lex > 0 {
			cands = append(cands, &scored{
				hit: Hit{Path: r.note, Title: r.title, ChunkIdx: r.ci, Chunk: r.chunk},
				cos: cos, lex: lex,
			})
		}
	}
	// reciprocal-rank fusion: each ranking contributes 1/(60+rank)
	for _, key := range []string{"cos", "lex"} {
		ordered := make([]*scored, len(cands))
		copy(ordered, cands)
		sort.SliceStable(ordered, func(i, j int) bool {
			if key == "cos" {
				return ordered[i].cos > ordered[j].cos
			}
			return ordered[i].lex > ordered[j].lex
		})
		for rank, s := range ordered {
			s.rrf += 1.0 / (rrfK + float64(rank))
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].rrf > cands[j].rrf })

	ranked := make([]Hit, 0, len(cands))
	for _, s := range cands {
		s.hit.Score = math.Round(s.rrf*10000) / 10000
		ranked = append(ranked, s.hit)
	}
	return ix.finalize(ranked, k)
}

// finalize turns a ranked chunk list into the returned passages.
//
// Two stages that materially change what an agent sees:
//
//   - per-note de-duplication, so one long note cannot occupy every slot: the
//     best chunk from each note comes first, then the remainder fills in;
//   - small-to-big — the top 3 hits are returned WITH their neighbouring chunks
//     merged in, because a chunk boundary often cuts the sentence that answers
//     the question. Merged neighbours are marked covered so they cannot also be
//     returned on their own.
func (ix *Index) finalize(ranked []Hit, k int) ([]Hit, error) {
	seen := map[string]bool{}
	var primary, extra []Hit
	for _, h := range ranked {
		if seen[h.Path] {
			extra = append(extra, h)
		} else {
			primary = append(primary, h)
		}
		seen[h.Path] = true
	}
	ordered := append(primary, extra...)

	type key struct {
		path string
		ci   int
	}
	covered := map[key]bool{}
	out := []Hit{}
	for _, h := range ordered {
		if k > 0 && len(out) >= k {
			break
		}
		if covered[key{h.Path, h.ChunkIdx}] {
			continue
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
				covered[key{h.Path, ci}] = true
			}
			rows.Close()
			if len(parts) > 0 {
				h.Chunk = strings.Join(parts, "\n")
			}
		} else {
			covered[key{h.Path, h.ChunkIdx}] = true
		}
		out = append(out, h)
	}
	return out, nil
}

// bm25 scores one chunk: IDF-weighted with term-frequency saturation and length
// normalization, so a term mentioned three times beats one mentioned once, but
// thirty mentions don't drown everything else.
func bm25(terms []string, r *corpusRow, df map[string]int, nChunks, avglen float64) float64 {
	norm := bm25K1 * (1 - bm25B + bm25B*float64(r.total)/avglen)
	score := 0.0
	for _, t := range terms {
		tf := float64(r.counts[t])
		if tf == 0 || df[t] == 0 {
			continue
		}
		score += math.Log(nChunks/float64(df[t])) * tf * (bm25K1 + 1) / (tf + norm)
	}
	return score
}

// corpus loads the vector store joined to note titles, tokenizing each chunk
// once per call.
func (ix *Index) corpus(includePrivate bool) ([]*corpusRow, error) {
	q := "SELECT v.note, v.chunk, v.chunk_idx, v.embedding, n.title " +
		"FROM vectors v JOIN notes n ON n.path=v.note"
	if !includePrivate {
		q += " WHERE v.private=0"
	}
	// Deterministic corpus order: RRF turns each chunk's RANK into its score and
	// many chunks tie on the lexical leg, so row order decides how ties break.
	// Unspecified order would make results shift after an unrelated rebuild.
	q += " ORDER BY v.note, v.chunk_idx"
	rows, err := ix.DB.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*corpusRow
	for rows.Next() {
		var note, chunk, title string
		var ci int
		var blob []byte
		if err := rows.Scan(&note, &chunk, &ci, &blob, &title); err != nil {
			return nil, err
		}
		counts, total := termCounts(chunk)
		out = append(out, &corpusRow{
			note: note, title: title, ci: ci, chunk: chunk,
			vector: Unpack(blob), counts: counts, total: total,
		})
	}
	return out, rows.Err()
}

func termCounts(s string) (map[string]int, int) {
	counts := map[string]int{}
	total := 0
	for _, t := range wordRE.FindAllString(strings.ToLower(s), -1) {
		counts[t]++
		total++
	}
	return counts, total
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
