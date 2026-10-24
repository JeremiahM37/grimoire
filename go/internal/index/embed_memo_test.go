package index

import "testing"

type memoCountingEmbedder struct{ calls int }

func (c *memoCountingEmbedder) Embed(texts []string) [][]float32 {
	c.calls++
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = []float32{float32(len(t)), 1}
	}
	return out
}
func (c *memoCountingEmbedder) Signature() string { return "count" }
func (c *memoCountingEmbedder) Dim() int          { return 2 }

func TestQueryMemoEmbedsARepeatedQueryOnce(t *testing.T) {
	inner := &memoCountingEmbedder{}
	m := memoQueries(inner)
	for i := 0; i < 3; i++ {
		if v := m.Embed([]string{"deploy grimoire"}); len(v) != 1 || v[0][0] != 15 {
			t.Fatalf("vector = %v", v)
		}
	}
	if inner.calls != 1 {
		t.Fatalf("inner embedder called %d times, want 1", inner.calls)
	}
	m.Embed([]string{"a", "b"}) // a batch always goes through
	m.Embed([]string{"a", "b"})
	if inner.calls != 3 {
		t.Fatalf("batches should not be memoised: %d calls", inner.calls)
	}
	if memoQueries(m) != m {
		t.Fatal("wrapping twice should return the same memo")
	}
}

func TestQueryMemoEvictsOldestPastItsSize(t *testing.T) {
	inner := &memoCountingEmbedder{}
	m := memoQueries(inner).(*queryMemo)
	for i := 0; i < queryMemoSize+10; i++ {
		m.Embed([]string{string(rune('a'+i%26)) + string(rune(i))})
	}
	if m.order.Len() != queryMemoSize || len(m.vecs) != queryMemoSize {
		t.Fatalf("memo holds %d/%d, want %d", m.order.Len(), len(m.vecs), queryMemoSize)
	}
}
