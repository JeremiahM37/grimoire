// Package rerank scores (query, passage) pairs so retrieval can reorder its
// candidates by how well each one answers the query.
//
// First-stage retrieval (BM25 + embeddings) scores the query and each passage
// independently, so it cannot see that a passage answers the question rather
// than merely sharing its words. A reranker reads the two together. Three
// backends satisfy the same interface:
//
//   - Local: a BERT cross-encoder run in pure Go, so the binary stays a single
//     static file with no cgo and no inference server to install.
//   - Remote: any service speaking the common /rerank HTTP shape.
//   - Adapt: a seam for anything else that can score pairs, such as the LLM
//     listwise ranker in the ai package.
//
// Scores are only comparable within one call: a cross-encoder emits raw
// logits, a remote service a relevance in [0,1], and an LLM adapter whatever
// it derives from its ranking. Callers should sort by them, not threshold.
package rerank

import (
	"context"
	"math"
	"sort"
)

// Reranker scores each document against the query. The result has one score
// per document, in the documents' order; higher is more relevant.
type Reranker interface {
	Score(ctx context.Context, query string, docs []string) ([]float32, error)
	Name() string
}

// ScoreFunc is the shape of a reranker's Score method, for adapting a plain
// function (an LLM call, a test double) without declaring a type.
type ScoreFunc func(ctx context.Context, query string, docs []string) ([]float32, error)

// Adapt turns a function into a Reranker.
func Adapt(name string, f ScoreFunc) Reranker { return funcReranker{name: name, f: f} }

type funcReranker struct {
	name string
	f    ScoreFunc
}

func (r funcReranker) Name() string { return r.name }

func (r funcReranker) Score(ctx context.Context, query string, docs []string) ([]float32, error) {
	return r.f(ctx, query, docs)
}

// ScoresFromOrder converts a ranking — document indices, best first, as a
// listwise LLM ranker returns — into scores for n documents. Ranked documents
// score n, n-1, …; documents the ranking omits score below all of them and
// keep their original relative order, so a partial ranking degrades instead of
// dropping candidates. Out-of-range and repeated indices are ignored.
func ScoresFromOrder(order []int, n int) []float32 {
	out := make([]float32, n)
	seen := make([]bool, n)
	rank := 0
	for _, i := range order {
		if i < 0 || i >= n || seen[i] {
			continue
		}
		seen[i] = true
		out[i] = float32(2*n - rank)
		rank++
	}
	for i := range out {
		if !seen[i] {
			out[i] = float32(n - i) // below every ranked score, order kept
		}
	}
	return out
}

// Order returns document indices sorted by descending score. Ties, and NaN
// scores (sorted last), keep their input order, so an all-equal result leaves
// the first-stage ranking untouched.
func Order(scores []float32) []int {
	idx := make([]int, len(scores))
	for i := range idx {
		idx[i] = i
	}
	key := func(i int) float64 {
		s := float64(scores[idx[i]])
		if math.IsNaN(s) {
			return math.Inf(-1)
		}
		return s
	}
	sort.SliceStable(idx, func(a, b int) bool { return key(a) > key(b) })
	return idx
}
