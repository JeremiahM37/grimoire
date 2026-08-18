package index

import (
	"math"
	"math/rand"
	"testing"
)

// Pack/Unpack define the on-disk vector format, which the Python
// implementation also reads and writes, so these are format tests rather than
// round-trip conveniences.

func TestPackUnpackRoundTripsExactly(t *testing.T) {
	// Bit-exactness matters more than approximate equality: a vector is
	// compared, not displayed, and a one-ULP drift on load would move
	// rankings. The awkward values are included deliberately.
	vals := []float32{
		0, 1, -1, 0.5, -0.5,
		float32(math.SmallestNonzeroFloat32),
		float32(math.MaxFloat32),
		float32(math.Inf(1)), float32(math.Inf(-1)),
		float32(math.Copysign(0, -1)), // negative zero
		0.1, 0.2, 0.3, 1e-30, 1e30,
	}
	got := Unpack(Pack(vals))
	if len(got) != len(vals) {
		t.Fatalf("length %d != %d", len(got), len(vals))
	}
	for i := range vals {
		if math.Float32bits(got[i]) != math.Float32bits(vals[i]) {
			t.Errorf("index %d: %v (bits %#x) != %v (bits %#x)",
				i, got[i], math.Float32bits(got[i]), vals[i], math.Float32bits(vals[i]))
		}
	}
	if len(Unpack(Pack(nil))) != 0 {
		t.Error("empty vector did not round-trip to empty")
	}
}

// decodeInto is the allocation-free form used to fill the retrieval cache's
// arena. It has to agree with Unpack to the bit, because the two describe the
// same stored bytes and only one of them is exercised by the search path.
func TestDecodeIntoMatchesUnpack(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	for trial := 0; trial < 200; trial++ {
		n := 1 + r.Intn(300)
		vec := make([]float32, n)
		for i := range vec {
			vec[i] = float32(r.NormFloat64())
		}
		blob := Pack(vec)

		want := Unpack(blob)
		got := make([]float32, n)
		decodeInto(got, blob)
		for i := range want {
			if math.Float32bits(got[i]) != math.Float32bits(want[i]) {
				t.Fatalf("trial %d index %d: decodeInto %v != Unpack %v",
					trial, i, got[i], want[i])
			}
		}
	}
}

func TestCosineGuards(t *testing.T) {
	a := []float32{1, 0, 0}
	if got := Cosine(a, []float32{1, 0}); got != 0 {
		t.Errorf("mismatched lengths = %v, want 0", got)
	}
	if got := Cosine(a, a); math.Abs(got-1) > 1e-12 {
		t.Errorf("identical vectors = %v, want 1", got)
	}
	if got := Cosine(a, []float32{0, 1, 0}); got != 0 {
		t.Errorf("orthogonal = %v, want 0", got)
	}
	if got := Cosine(a, []float32{-1, 0, 0}); math.Abs(got+1) > 1e-12 {
		t.Errorf("opposed = %v, want -1", got)
	}
	// An empty note embeds to an all-zero vector. Dividing by its zero norm
	// would produce NaN, and a NaN in the ranking poisons every comparison it
	// takes part in, so the zero guard is a correctness property.
	zero := []float32{0, 0, 0}
	if got := Cosine(zero, a); math.IsNaN(got) {
		t.Error("zero vector produced NaN")
	}
	if got := Cosine(zero, zero); math.IsNaN(got) {
		t.Error("two zero vectors produced NaN")
	}
}

// The retrieval cache precomputes each row's norm and reuses it across
// queries, which is only safe if it produces bit-identical scores to computing
// the norm inline. That claim is what licenses the optimization, so it is
// asserted rather than argued.
func TestCachedCosineIsBitIdenticalToCosine(t *testing.T) {
	ix := parityCorpus(t)
	c, err := ix.corpusCacheFor()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.rows) == 0 {
		t.Fatal("empty cache; the test would prove nothing")
	}

	r := rand.New(rand.NewSource(5))
	for trial := 0; trial < 20; trial++ {
		qv := make([]float32, c.dim)
		for i := range qv {
			qv[i] = float32(r.NormFloat64())
		}
		var qsq float64
		for _, x := range qv {
			qsq += float64(x) * float64(x)
		}
		qnorm := math.Sqrt(qsq)

		for i := range c.rows {
			row := c.vecs[i*c.dim : (i+1)*c.dim]
			want := Cosine(row, qv)
			got := c.cosine(i, qv, qnorm)
			if math.Float64bits(got) != math.Float64bits(want) {
				t.Fatalf("trial %d row %d: cached cosine %.17g != Cosine %.17g",
					trial, i, got, want)
			}
		}
	}
}

// queryTerms is what makes BM25 reproducible; its contract is dedup plus a
// fixed order that depends only on the query text.
func TestQueryTermsAreDedupedAndSorted(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"beta alpha", []string{"alpha", "beta"}},
		{"Beta ALPHA beta", []string{"alpha", "beta"}},
		{"alpha alpha alpha", []string{"alpha"}},
		{"", nil},
		{"   ", nil},
		{"punctuation! and, symbols?", []string{"and", "punctuation", "symbols"}},
		{"mixed123 456", []string{"456", "mixed123"}},
	}
	for _, c := range cases {
		got := queryTerms(c.in)
		if len(got) != len(c.want) {
			t.Errorf("queryTerms(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("queryTerms(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
	// Order must be a function of the terms, not of how they were written.
	a, b := queryTerms("zeta alpha mu"), queryTerms("mu zeta alpha")
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("term order depends on input order: %v vs %v", a, b)
		}
	}
}

// Corpus statistics have to survive degenerate corpora: an all-empty vault
// would divide by zero, and a zero average length makes every BM25 length
// normalization infinite.
func TestAvgLenGuardsDegenerateCorpora(t *testing.T) {
	c := &corpusCache{}
	if got := c.avgLen(true); got != 1 {
		t.Errorf("empty corpus avgLen = %v, want 1", got)
	}
	c = &corpusCache{nAll: 3, nPublic: 3, lenAll: 0, lenPublic: 0}
	if got := c.avgLen(false); got != 1 {
		t.Errorf("zero-length corpus avgLen = %v, want 1", got)
	}
	c = &corpusCache{nAll: 4, nPublic: 2, lenAll: 40, lenPublic: 10}
	if got := c.avgLen(true); got != 10 {
		t.Errorf("avgLen(includePrivate) = %v, want 10", got)
	}
	if got := c.avgLen(false); got != 5 {
		t.Errorf("avgLen(public) = %v, want 5", got)
	}
}

// An empty query and an empty corpus are both ordinary states — a fresh vault,
// or a cleared search box — and neither may error or panic.
func TestRetrieveHandlesEmptyInputs(t *testing.T) {
	ix := parityCorpus(t)
	for _, q := range []string{"", "   ", "\t\n"} {
		hits, err := ix.Retrieve(q, 8, true)
		if err != nil {
			t.Errorf("Retrieve(%q) errored: %v", q, err)
		}
		if len(hits) != 0 {
			t.Errorf("Retrieve(%q) returned %d hits, want none", q, len(hits))
		}
	}
	// A query whose terms appear nowhere still runs the dense leg, so it may
	// return results; it must simply not fail.
	if _, err := ix.Retrieve("zzzznomatchzzzz", 8, true); err != nil {
		t.Errorf("no-match query errored: %v", err)
	}
}
