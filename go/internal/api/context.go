package api

import (
	"net/http"
	"os"
	"strconv"

	"github.com/JeremiahM37/grimoire/go/internal/index"
)

// Choosing between retrieving and just reading everything.
//
// Retrieval exists to decide what to leave out. When nothing has to be left
// out — when the whole corpus fits the budget the caller has anyway — deciding
// can only lose information, and the benchmarks say so out loud: on LoCoMo,
// whose conversations fit a reader's window, handing over the whole transcript
// beats retrieving from it by 5.5 points (p = 0.0015, n = 500). On
// LongMemEval's ~118k-token haystacks the same comparison inverts and
// retrieval wins by 8.5. The difference is not the method, it is whether the
// corpus fits.
//
// So this is a size check, not a position on "RAG versus long context".

// DefaultContextBudget is the character budget under which the whole corpus is
// preferred to a ranking of it.
//
// 100k characters is roughly 25k tokens, which is the size at which full
// context is MEASURED to win (LoCoMo's conversations average ~24k tokens).
// The crossover above that is not measured — LongMemEval's haystacks, where
// retrieval wins decisively, are nearly five times larger — so the default
// sits at the top of the evidence rather than at the top of the guess.
// Raise it with GRIMOIRE_CONTEXT_BUDGET if your vault is bigger and you would
// rather spend tokens than lose recall.
const DefaultContextBudget = 100_000

func contextBudget() int {
	if v := os.Getenv("GRIMOIRE_CONTEXT_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return DefaultContextBudget
}

// bestContext returns the passages to answer from, and which strategy produced
// them: "full" when the entire corpus fit the budget, "retrieved" otherwise.
func (s *Server) bestContext(q string, k int, includePrivate bool, budget int) ([]index.Hit, string, error) {
	if budget > 0 {
		_, _, chars, err := s.Index.CorpusStats(includePrivate)
		if err != nil {
			return nil, "", err
		}
		if chars > 0 && chars <= int64(budget) {
			hits, err := s.Index.WholeCorpus(includePrivate)
			if err != nil {
				return nil, "", err
			}
			return hits, "full", nil
		}
	}
	hits, err := s.Index.Retrieve(q, k, includePrivate)
	return hits, "retrieved", err
}

// contextEndpoint answers "what should an agent read to answer this?" — the
// question /api/retrieve only answers under the assumption that ranking is
// wanted at all.
func (s *Server) contextEndpoint(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	k := 8
	if v := r.URL.Query().Get("k"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			k = n
		}
	}
	budget := contextBudget()
	if v := r.URL.Query().Get("budget"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			budget = n
		}
	}
	includePrivate := truthy(r.URL.Query().Get("include_private"))

	hits, mode, err := s.bestContext(q, k, includePrivate, budget)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hits == nil {
		hits = []index.Hit{}
	}
	chars := 0
	for _, h := range hits {
		chars += len(h.Chunk)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode": mode, "budget": budget, "chars": chars,
		"passages": hits,
	})
}
