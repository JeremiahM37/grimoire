package api

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/adherence"
	"github.com/JeremiahM37/grimoire/go/internal/decide"
)

// gateBudget is the hard total time the gate may add to a context request.
const gateBudget = 400 * time.Millisecond

// settingFloat reads a numeric setting within (lo, hi], else the default.
func (s *Server) settingFloat(key string, def, lo, hi float64) float64 {
	if v, err := strconv.ParseFloat(s.setting(key), 64); err == nil && v > lo && v <= hi {
		return v
	}
	return def
}

// The stricter wording ("p1" in benchmarks/memory_use/round2): AUC 0.80 against
// 0.745 for the old one-line question on 180 labelled pairs. It costs a few
// more tokens to read, which the 400 ms budget absorbs (p50 277 ms, p95 374 ms
// on Jev with three in parallel). The yes-probabilities it gives are low, so
// the matching default threshold is 0.16, not 0.5.
var gateQuestion = map[string]decide.Question{"applies": {
	Type: decide.Noul,
	Instructions: "Is this stored memory directly about the task the user is asking for right now, " +
		"so that the agent would act differently without it? Sharing a topic or a project name is not enough.",
	Criteria: map[string]string{
		"true":  "the memory states a rule, fact or preference that governs this exact request",
		"false": "the memory is only on the same topic, or the request does not touch what it says",
	},
}}

// gateClient builds the gate's decision client, or nil when off.
func (s *Server) gateClient() *decide.Client {
	url := strings.TrimSpace(s.setting("context_gate_url"))
	if url == "" {
		return nil
	}
	key := strings.TrimSpace(s.setting("decision_api_key"))
	if key == "" && s.Secrets != nil {
		if v, err := s.Secrets.Get("decision-api-key"); err == nil {
			key = v
		}
	}
	return &decide.Client{BaseURL: url, Model: strings.TrimSpace(s.setting("context_gate_model")),
		APIKey: key, HTTP: &http.Client{Timeout: gateBudget}}
}

// gateBand parses "lo,hi" (or "lo-hi").
func gateBand(v string) (float64, float64) {
	lo, hi := 0.5, 0.75
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '-' || r == ' ' })
	if len(parts) == 2 {
		a, e1 := strconv.ParseFloat(parts[0], 64)
		b, e2 := strconv.ParseFloat(parts[1], 64)
		if e1 == nil && e2 == nil && a >= 0 && b <= 1 && a < b {
			lo, hi = a, b
		}
	}
	return lo, hi
}

// applyGate asks the decision model, for candidates whose relevance is inside
// the band and would otherwise be injected, whether the memory applies to the
// request. Above the band injects, below never does. The calls run in
// parallel under a hard 400 ms total budget; a candidate with no answer by
// then keeps its score-rule fate. Gate latency is recorded.
func (s *Server) applyGate(ctx context.Context, query string, items []contextItem, minRel float64) []contextItem {
	client := s.gateClient()
	if client == nil {
		return items
	}
	lo, hi := gateBand(s.setting("context_gate_band"))
	if lo < minRel {
		lo = minRel
	}
	var idx []int
	for i, it := range items {
		if it.score >= lo && it.score < hi {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return items
	}
	sort.Slice(idx, func(a, b int) bool { return items[idx[a]].score > items[idx[b]].score })
	if max := int(s.settingFloat("context_gate_max", 3, 0, 10)); len(idx) > max {
		idx = idx[:max]
	}
	keep := s.settingFloat("context_gate_threshold", 0.16, 0, 1)
	started := time.Now()
	gctx, cancel := context.WithTimeout(ctx, gateBudget)
	defer cancel()
	verdict := make([]float64, len(idx))
	for i := range verdict {
		verdict[i] = -1
	}
	var wg sync.WaitGroup
	for n, i := range idx {
		wg.Add(1)
		go func(n int, it contextItem) {
			defer wg.Done()
			state := "Memory: " + clipText(strings.Join(strings.Fields(it.Text), " "), 280) +
				"\nRequest: " + clipText(query, 240)
			res, err := client.Ask(gctx, state, gateQuestion)
			if a, ok := res.Answers["applies"]; err == nil && ok && a.Type == decide.Noul {
				verdict[n] = a.Noul
			}
		}(n, items[i])
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	timedOut := false
	select {
	case <-done:
	case <-gctx.Done():
		timedOut = true
	}
	call := adherence.GateCall{MS: time.Since(started).Milliseconds(), Candidates: len(idx), TimedOut: timedOut}
	// verdict is read only after done or deadline; a late goroutine may still
	// write its slot, so copy under the race by reading a snapshot.
	if timedOut {
		<-done // goroutines return promptly once the context is cancelled
	}
	for n, i := range idx {
		switch v := verdict[n]; {
		case v < 0:
		case v >= keep:
			call.Kept++
		default:
			items[i].score = 0
			call.Dropped++
		}
	}
	if st := s.adh(); st != nil {
		st.LogGate(call, time.Now())
	}
	return items
}
