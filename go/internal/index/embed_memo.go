package index

import (
	"container/list"
	"sync"
)

// queryMemo remembers the vectors of recent single-text embeddings.
//
// One context request embeds the same query three times — fact recall, note
// retrieval and cue matching each ask for it — and with a remote embedder every
// one of those is a network round trip. That was most of the hook's wall time.
// Only one-text calls are remembered: those are queries, and they repeat; a
// batch is indexing, which does not, and would only push queries out.
type queryMemo struct {
	Embedder
	mu    sync.Mutex
	order *list.List // most recent at the front; values are memoKey
	byKey map[memoKey]*list.Element
	vecs  map[memoKey][]float32
	// flight holds the queries being embedded right now, so a second caller
	// waits for the first instead of making the same round trip.
	flight map[memoKey]chan struct{}
}

type memoKey struct{ sig, text string }

// queryMemoSize is how many query vectors are kept: a few sessions' worth of
// prompts and tool calls, under a megabyte at 768 dimensions.
const queryMemoSize = 256

func memoQueries(e Embedder) Embedder {
	if _, ok := e.(*queryMemo); ok {
		return e
	}
	return &queryMemo{Embedder: e, order: list.New(),
		byKey: map[memoKey]*list.Element{}, vecs: map[memoKey][]float32{},
		flight: map[memoKey]chan struct{}{}}
}

func (m *queryMemo) Embed(texts []string) [][]float32 {
	if len(texts) != 1 {
		return m.Embedder.Embed(texts)
	}
	// The signature is part of the key so a swapped model never answers with
	// the old model's vector.
	k := memoKey{m.Embedder.Signature(), texts[0]}
	for {
		m.mu.Lock()
		if el, ok := m.byKey[k]; ok {
			m.order.MoveToFront(el)
			v := m.vecs[k]
			m.mu.Unlock()
			return [][]float32{v}
		}
		wait, busy := m.flight[k]
		if !busy {
			m.flight[k] = make(chan struct{})
			m.mu.Unlock()
			break
		}
		m.mu.Unlock()
		<-wait
		// The other caller finished: its vector is remembered now, unless it
		// failed, in which case this caller tries for itself.
		m.mu.Lock()
		_, ok := m.byKey[k]
		m.mu.Unlock()
		if !ok {
			return m.Embedder.Embed(texts)
		}
	}
	defer func() {
		m.mu.Lock()
		close(m.flight[k])
		delete(m.flight, k)
		m.mu.Unlock()
	}()

	out := m.Embedder.Embed(texts)
	// A failed or empty embedding is not remembered, so the next call retries.
	if len(out) != 1 || len(out[0]) == 0 {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byKey[k]; !ok {
		m.byKey[k] = m.order.PushFront(k)
		m.vecs[k] = out[0]
		for m.order.Len() > queryMemoSize {
			old := m.order.Remove(m.order.Back()).(memoKey)
			delete(m.byKey, old)
			delete(m.vecs, old)
		}
	}
	return out
}
