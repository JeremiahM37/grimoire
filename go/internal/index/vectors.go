package index

import (
	"encoding/binary"
	"math"
)

// Pack serializes a vector as little-endian float32, the layout Python's
// struct.pack("<Nf") produces — the two implementations read each other's
// stored embeddings, so the byte order is part of the on-disk format.
func Pack(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(x))
	}
	return out
}

// Unpack reverses Pack.
func Unpack(blob []byte) []float32 {
	out := make([]float32, len(blob)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return out
}

// decodeInto is Unpack without the allocation, for callers that already own
// the destination — the retrieval cache decodes every stored vector into one
// contiguous arena, and a per-row temporary there is pure garbage.
// len(dst) must be len(blob)/4.
func decodeInto(dst []float32, blob []byte) {
	for i := range dst {
		dst[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
}

// Cosine is the cosine similarity of two vectors. Mismatched lengths score 0,
// and a zero vector is treated as unit-length so an empty note can't produce
// NaN and poison a ranking.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	na, nb = math.Sqrt(na), math.Sqrt(nb)
	if na == 0 {
		na = 1
	}
	if nb == 0 {
		nb = 1
	}
	return dot / (na * nb)
}
