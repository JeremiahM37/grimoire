package embed

import (
	"crypto/md5" //nolint:gosec // a hash bucket, not a security primitive
	"math"
	"math/big"
	"regexp"
	"strings"
)

// Dim is the vector width every backend produces.
const Dim = 256

var tokenRE = regexp.MustCompile(`[a-z0-9]+`)

// Hash is the zero-dependency embedding floor: a signed hashing trick over
// lowercase alphanumeric tokens. Not semantic, but always available, so
// indexing never breaks because a model or an AI service is missing.
type Hash struct{}

// Signature identifies the backend for the index's re-embed check.
func (Hash) Signature() string { return "hash:v1" }

// Dim reports the vector width.
func (Hash) Dim() int { return Dim }

// Embed encodes a batch of texts.
func (h Hash) Embed(texts []string) [][]float32 {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = h.embedOne(t)
	}
	return out
}

func (Hash) embedOne(text string) []float32 {
	vec := make([]float32, Dim)
	for _, tok := range tokenRE.FindAllString(strings.ToLower(text), -1) {
		sum := md5.Sum([]byte(tok)) //nolint:gosec // bucketing only
		// Python reads the full 128-bit digest as one integer, so the bucket
		// and sign come from the WHOLE hash — taking a machine-word slice of it
		// would silently produce a different, equally plausible vector.
		h := new(big.Int).SetBytes(sum[:])
		idx := new(big.Int).Mod(h, big.NewInt(Dim)).Int64()
		sign := float32(-1)
		if new(big.Int).Rsh(h, 8).Bit(0) == 1 {
			sign = 1
		}
		vec[idx] += sign
	}
	var sq float64
	for _, v := range vec {
		sq += float64(v) * float64(v)
	}
	norm := math.Sqrt(sq)
	if norm == 0 {
		norm = 1 // Python's `or 1.0`: an empty text stays all-zero
	}
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}
