// Package usage records what grimoire's own model calls cost.
//
// The scope is deliberately narrow and the boundary is the whole point.
// Grimoire is mounted BY agents; it does not sit between an agent and its
// provider, so it never sees the tokens a coding agent spends thinking. A
// dashboard here that claimed to show "your AI spend" would be inventing the
// large majority of it.
//
// What it can account for exactly is its own work: ask_notes, reranking,
// summarising, intent classification, embeddings. Those run on a key the
// operator configured against a model the operator chose, and until now cost
// nothing visible. That is a real number, it is nobody else's to report, and it
// is the one this package keeps.
package usage

import (
	"strings"
	"sync"
	"time"
)

// Call is one model invocation grimoire made.
type Call struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"at"`
	Provider Provider  `json:"provider"`
	Model    string    `json:"model"`
	// Surface is which part of grimoire made the call — ask, rerank, intent,
	// summarize, embed. Without it a bill is a single number nobody can act on;
	// with it, "reranking costs more than answering" is visible.
	Surface string `json:"surface"`
	// Agent is the MCP client that triggered the work, when one did. Empty for
	// a call the console or a background job made.
	Agent        string  `json:"agent,omitempty"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	LatencyMS    int64   `json:"latency_ms"`
	Cost         float64 `json:"cost"`
	// CostKnown is false when no price is on file for the model. The cost then
	// reads zero, and a zero presented as a total makes an unmetered provider
	// look free — so every surface reports this alongside the number.
	CostKnown bool   `json:"cost_known"`
	Error     string `json:"error,omitempty"`
}

// Store persists calls. An interface so the AI client can record without
// depending on the index, and so tests can assert without a database.
type Store interface {
	Record(Call) error
}

// Nop discards calls, for a server with no index wired.
type Nop struct{}

func (Nop) Record(Call) error { return nil }

// Recorder wraps a Store with the accounting the AI client should not have to
// repeat at every call site: timing, token extraction, pricing, and never
// letting a bookkeeping failure break the thing being booked.
type Recorder struct {
	Store Store
	mu    sync.Mutex
}

// NewRecorder returns a recorder writing to store. A nil store discards.
func NewRecorder(store Store) *Recorder {
	if store == nil {
		store = Nop{}
	}
	return &Recorder{Store: store}
}

// Observe records one completed call.
//
// Errors from the store are swallowed on purpose. Accounting must never be able
// to fail an answer: a dropped usage row is a gap in a report, while a returned
// error would be a question the user asked and did not get answered because the
// ledger was busy.
func (r *Recorder) Observe(c Call) {
	if r == nil || r.Store == nil {
		return
	}
	if c.At.IsZero() {
		c.At = time.Now().UTC()
	}
	c.Cost, c.CostKnown = Cost(c.Provider, c.Model, c.InputTokens, c.OutputTokens)
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.Store.Record(c)
}

// Time starts a timer for a call about to be made.
func (r *Recorder) Time() time.Time { return time.Now() }

// Tokens is the usage a provider reported, normalised.
//
// Every provider spells this differently — OpenAI-compatible APIs use
// prompt_tokens/completion_tokens, Anthropic input_tokens/output_tokens, Ollama
// prompt_eval_count/eval_count — so the extraction is per backend and the rest
// of the system sees one shape.
type Tokens struct {
	Input  int
	Output int
}

// FromOllama reads Ollama's counters.
func FromOllama(promptEval, eval int) Tokens { return Tokens{Input: promptEval, Output: eval} }

// FromOpenAI reads an OpenAI-compatible usage block.
func FromOpenAI(prompt, completion int) Tokens { return Tokens{Input: prompt, Output: completion} }

// FromAnthropic reads Anthropic's usage block.
func FromAnthropic(in, out int) Tokens { return Tokens{Input: in, Output: out} }

// Summary is a rollup for the dashboard.
type Summary struct {
	Since        time.Time `json:"since"`
	Calls        int       `json:"calls"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	Cost         float64   `json:"cost"`
	// Unpriced counts calls whose model has no price on file. Reported so the
	// total can be read as "at least this much" rather than as complete.
	Unpriced      int          `json:"unpriced_calls"`
	Errors        int          `json:"errors"`
	ByProvider    []GroupTotal `json:"by_provider"`
	BySurface     []GroupTotal `json:"by_surface"`
	ByModel       []GroupTotal `json:"by_model"`
	ByAgent       []GroupTotal `json:"by_agent"`
	PricesUpdated string       `json:"prices_updated"`
}

// GroupTotal is one row of a rollup.
type GroupTotal struct {
	Key          string  `json:"key"`
	Calls        int     `json:"calls"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Cost         float64 `json:"cost"`
	Unpriced     int     `json:"unpriced_calls"`
}

// Summarise rolls a slice of calls into the dashboard's shape.
func Summarise(calls []Call, since time.Time) Summary {
	s := Summary{Since: since, PricesUpdated: PricesUpdated}
	byProvider := map[string]*GroupTotal{}
	bySurface := map[string]*GroupTotal{}
	byModel := map[string]*GroupTotal{}
	byAgent := map[string]*GroupTotal{}

	add := func(m map[string]*GroupTotal, key string, c Call) {
		if key == "" {
			key = "(none)"
		}
		g, ok := m[key]
		if !ok {
			g = &GroupTotal{Key: key}
			m[key] = g
		}
		g.Calls++
		g.InputTokens += c.InputTokens
		g.OutputTokens += c.OutputTokens
		g.Cost += c.Cost
		if !c.CostKnown {
			g.Unpriced++
		}
	}

	for _, c := range calls {
		s.Calls++
		s.InputTokens += c.InputTokens
		s.OutputTokens += c.OutputTokens
		s.Cost += c.Cost
		if !c.CostKnown {
			s.Unpriced++
		}
		if c.Error != "" {
			s.Errors++
		}
		add(byProvider, string(c.Provider), c)
		add(bySurface, c.Surface, c)
		add(byModel, c.Model, c)
		add(byAgent, c.Agent, c)
	}
	s.ByProvider = sorted(byProvider)
	s.BySurface = sorted(bySurface)
	s.ByModel = sorted(byModel)
	s.ByAgent = sorted(byAgent)
	return s
}

// sorted returns rollup rows, most expensive first, then most called — so the
// row worth looking at is the first one.
func sorted(m map[string]*GroupTotal) []GroupTotal {
	out := make([]GroupTotal, 0, len(m))
	for _, g := range m {
		out = append(out, *g)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			less := a.Cost < b.Cost || (a.Cost == b.Cost &&
				(a.Calls < b.Calls || (a.Calls == b.Calls && strings.Compare(a.Key, b.Key) > 0)))
			if !less {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
