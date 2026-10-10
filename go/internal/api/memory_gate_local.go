package api

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/adherence"
	"github.com/JeremiahM37/grimoire/go/internal/rerank"
)

// The local gate answers "does this memory apply to this request" with a
// cross-encoder fine-tuned on Jev's strict-question verdicts
// (benchmarks/memory_use/round2/gatemodel_*.py), run in pure Go inside the
// server: no extra service, a few tens of milliseconds for three candidates.
//
// It is selected by context_gate_local (a model directory, or a name under
// <vault>/.grimoire/models) and used when context_gate_url is empty; a remote
// decision server, if configured, takes precedence. The score is the model's
// raw logit; the shipped threshold is fitted on the round-2 benchmark
// (docs/MEMORY_ADHERENCE.md).

const defaultGateLocalThreshold = -0.3

type gateLocalState struct {
	mu    sync.Mutex
	key   string
	model *rerank.Local
}

// gateLocalModel returns the in-process gate model, building it on first use
// and rebuilding it if the setting changes. Nil when not configured or when
// the model files are not on disk (the gate then does nothing, as if off).
func (s *Server) gateLocalModel() *rerank.Local {
	name := s.setting("context_gate_local")
	if name == "" || s.Vault == nil {
		return nil
	}
	st := &s.gateLocal
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.key == name {
		return st.model
	}
	st.key, st.model = name, nil
	cache := filepath.Join(s.Vault.Root, ".grimoire", "models")
	dir := rerank.FindModel(name, cache)
	if dir == "" {
		return nil
	}
	m := rerank.NewLocal(rerank.LocalConfig{Model: dir, CacheDir: cache, MaxLen: 192})
	st.model = m
	// Load now, off the request path: a first request must not spend its
	// whole 400 ms budget reading weights.
	go m.Score(context.Background(), "warm", []string{"warm"}) //nolint:errcheck
	return m
}

// applyLocalGate scores the selected candidates in one batched forward pass
// and zeroes the ones below the threshold. On error or timeout the score rule
// stands.
func (s *Server) applyLocalGate(ctx context.Context, query string, items []contextItem, idx []int, started time.Time) {
	m := s.gateLocalModel()
	thr := defaultGateLocalThreshold
	if v, err := strconv.ParseFloat(s.setting("context_gate_local_threshold"), 64); err == nil {
		thr = v
	}
	docs := make([]string, len(idx))
	for n, i := range idx {
		docs[n] = clipText(strings.Join(strings.Fields(items[i].Text), " "), 280)
	}
	type result struct {
		sc  []float32
		err error
	}
	ch := make(chan result, 1)
	q := clipText(strings.Join(strings.Fields(query), " "), 240)
	go func() {
		sc, err := m.Score(ctx, q, docs)
		ch <- result{sc, err}
	}()
	call := adherence.GateCall{Candidates: len(idx)}
	select {
	case r := <-ch:
		if r.err == nil && len(r.sc) == len(idx) {
			for n, i := range idx {
				if float64(r.sc[n]) >= thr {
					call.Kept++
				} else {
					items[i].score = 0
					call.Dropped++
				}
			}
		}
	case <-ctx.Done():
		call.TimedOut = true
	}
	call.MS = time.Since(started).Milliseconds()
	if st := s.adh(); st != nil {
		st.LogGate(call, time.Now())
	}
}
