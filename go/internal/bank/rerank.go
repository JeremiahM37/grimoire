package bank

import "context"

// Reranker scores candidate passages against a query, one score per passage,
// higher is better. Scores may be calibrated probabilities in [0,1] or raw
// logits; recall normalises either (see normaliseScores).
//
// The interface is deliberately the smallest one a cross-encoder, a remote
// /rerank endpoint or a listwise model can all satisfy, so recall does not
// care which is plugged in.
type Reranker interface {
	Score(ctx context.Context, query string, docs []string) ([]float32, error)
}

// NoRerank is the default: it declines to score, and recall keeps the fused
// rank order, mapped onto a descending score.
type NoRerank struct{}

// Score always returns nil, which recall reads as "no opinion".
func (NoRerank) Score(context.Context, string, []string) ([]float32, error) { return nil, nil }
