package index

import (
	"encoding/json"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Learned ranking weights (experiment "ips", docs/lab/ips.md).
//
// The offline learner (internal/rankfit) fits weights on logged exposures. It
// never deploys them. Under the ips flag only, rankMemory uses a weight vector
// stored in the index meta under rankWeightsKey, if one is present and well
// formed. With the flag off, or with no stored vector, the hand-set constants
// are used and ranking is unchanged.

const (
	ipsFlag        = "ips"
	rankWeightsKey = "memory_rank_weights"
	// RankDims is the number of ranking features. The order is fixed:
	// semantic, keyword, entity, recency, useful, link, authority.
	RankDims = 7
)

// RankFeatures returns the feature vector the learner fits and the exposure log
// records, in the order RankDims names.
func RankFeatures(h MemoryHit) []float64 {
	auth := 0.0
	if h.Authority() == memory.AuthorityHuman {
		auth = 1
	}
	return []float64{h.Semantic, h.Keyword, h.Entity, h.Recency, h.Useful, h.Link, auth}
}

// learnedRankWeights returns the stored weight vector when the ips experiment is
// on and one is present. Nil means use the hand-set constants.
func (ix *Index) learnedRankWeights() *[RankDims]float64 {
	if !exp.On(ipsFlag) {
		return nil
	}
	raw, err := ix.meta(rankWeightsKey)
	if err != nil || raw == "" {
		return nil
	}
	var v struct {
		Weights []float64 `json:"weights"`
	}
	if json.Unmarshal([]byte(raw), &v) != nil || len(v.Weights) != RankDims {
		return nil
	}
	var out [RankDims]float64
	copy(out[:], v.Weights)
	return &out
}

// learnedBase scores a hit with a stored weight vector in place of the
// hand-set components. The phrase term is kept as it is.
func learnedBase(w *[RankDims]float64, h MemoryHit, phrase float64) float64 {
	f := RankFeatures(h)
	s := 0.0
	for i := range f {
		s += w[i] * f[i]
	}
	return s + wPhrase*phrase
}

// HandRankWeights returns the hand-set constants on the feature order of
// RankFeatures, for comparison with a learned vector. Phrase is not included:
// it is not a learned feature.
func HandRankWeights() [RankDims]float64 {
	return [RankDims]float64{wSemantic, wKeyword, wEntity, wRecency, wUseful, 0, 0}
}
