package usage

import (
	"testing"
	"time"
)

// Summarise is what turns a ledger into the screen a person reads, and the two
// things it must not get wrong are both about money rather than arithmetic:
// the rows have to arrive in the order that puts the expensive one first, and
// an unpriced call has to stay countable after it has been rolled up. A total
// that has silently absorbed an unknown is indistinguishable from a complete
// one, and that is the whole failure this package exists to avoid.

func call(provider Provider, model, surface, agent string, in, out int) Call {
	c := Call{Provider: provider, Model: model, Surface: surface, Agent: agent,
		InputTokens: in, OutputTokens: out}
	c.Cost, c.CostKnown = Cost(provider, model, in, out)
	return c
}

func TestSummariseTotalsEveryDimension(t *testing.T) {
	s := Summarise([]Call{
		call(Anthropic, "claude-sonnet-5", "ask", "claude-code", 1_000_000, 100_000),
		call(Ollama, "qwen3.5:4b", "intent", "claude-code", 500, 50),
		call(OpenAI, "gpt-4o-mini", "rerank", "codex", 2_000, 100),
	}, time.Time{})

	if s.Calls != 3 {
		t.Errorf("calls = %d, want 3", s.Calls)
	}
	if s.InputTokens != 1_002_500 || s.OutputTokens != 100_150 {
		t.Errorf("tokens = %d/%d, want 1002500/100150", s.InputTokens, s.OutputTokens)
	}
	// $3 for a million sonnet input + $1.50 for 100k output. Local is free and
	// gpt-4o-mini's contribution is fractions of a cent.
	if s.Cost < 4.50 || s.Cost > 4.51 {
		t.Errorf("cost = %f, want ~4.50", s.Cost)
	}
	if s.Unpriced != 0 {
		t.Errorf("unpriced = %d, want 0 — every model here has a price", s.Unpriced)
	}
	for _, g := range []struct {
		name string
		rows []GroupTotal
		want int
	}{
		{"by provider", s.ByProvider, 3},
		{"by surface", s.BySurface, 3},
		{"by model", s.ByModel, 3},
		{"by agent", s.ByAgent, 2}, // two calls share claude-code
	} {
		if len(g.rows) != g.want {
			t.Errorf("%s has %d rows, want %d", g.name, len(g.rows), g.want)
		}
	}
	if s.PricesUpdated != PricesUpdated {
		t.Error("the summary must carry the price date; a cost with no date on it is a guess")
	}
}

// The first row is the one a person looks at, so it has to be the one worth
// looking at.
func TestRollupRowsAreMostExpensiveFirst(t *testing.T) {
	s := Summarise([]Call{
		call(Ollama, "qwen3.5:4b", "intent", "", 100, 10),
		call(Anthropic, "claude-opus-5", "ask", "", 1_000_000, 0),   // $15
		call(Anthropic, "claude-sonnet-5", "ask", "", 1_000_000, 0), // $3
		call(OpenAI, "gpt-4o-mini", "rerank", "", 1_000_000, 0),     // $0.15
	}, time.Time{})

	var got []string
	for _, r := range s.ByModel {
		got = append(got, r.Key)
	}
	want := []string{"claude-opus-5", "claude-sonnet-5", "gpt-4o-mini", "qwen3.5:4b"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("by model = %v, want %v", got, want)
		}
	}
}

// A free local call and an unpriceable routed call both total zero dollars.
// They are not the same thing, and the difference is the point.
func TestAnUnpricedCallStaysCountableAfterRollup(t *testing.T) {
	s := Summarise([]Call{
		call(OpenRouter, "anthropic/claude-sonnet-5", "ask", "", 900_000, 900_000),
		call(Ollama, "qwen3.5:4b", "intent", "", 100, 10),
	}, time.Time{})

	if s.Unpriced != 1 {
		t.Fatalf("summary unpriced = %d, want 1 — the routed call has no price on file", s.Unpriced)
	}
	if s.Cost != 0 {
		t.Errorf("cost = %f; an unpriced call must contribute nothing rather than a guess", s.Cost)
	}
	var routed, local GroupTotal
	for _, r := range s.ByProvider {
		switch r.Key {
		case string(OpenRouter):
			routed = r
		case string(Ollama):
			local = r
		}
	}
	if routed.Unpriced != routed.Calls {
		t.Errorf("openrouter row: %d of %d calls unpriced, want all — the UI reads this to "+
			"print \"not priced\" instead of $0.00", routed.Unpriced, routed.Calls)
	}
	if local.Unpriced != 0 {
		t.Error("a local call is free, not unknown; rolling it up as unpriced would hedge a total that is exact")
	}
	// And the tokens survive even where the money cannot be known — an
	// unpriceable call is still a measurable one.
	if routed.InputTokens != 900_000 || routed.OutputTokens != 900_000 {
		t.Errorf("routed tokens = %d/%d, want 900000/900000",
			routed.InputTokens, routed.OutputTokens)
	}
}

// Calls the console or a background job made carry no agent. They must still
// appear rather than vanishing into an empty key.
func TestCallsWithNoAgentAreLabelledNotDropped(t *testing.T) {
	s := Summarise([]Call{call(Ollama, "qwen3.5:4b", "embed", "", 10, 1)}, time.Time{})
	if len(s.ByAgent) != 1 || s.ByAgent[0].Key != "(none)" {
		t.Fatalf("by agent = %+v, want a single \"(none)\" row", s.ByAgent)
	}
}

func TestSummariseOfNothingIsEmptyNotBroken(t *testing.T) {
	s := Summarise(nil, time.Time{})
	if s.Calls != 0 || s.Cost != 0 || len(s.ByProvider) != 0 {
		t.Errorf("empty ledger summarised to %+v", s)
	}
	if s.PricesUpdated == "" {
		t.Error("even an empty report says when prices were checked")
	}
}

// Errors are recorded, and a failed call still spent input tokens.
func TestErrorsAreCounted(t *testing.T) {
	bad := call(Anthropic, "claude-sonnet-5", "ask", "", 1000, 0)
	bad.Error = "429 rate limited"
	s := Summarise([]Call{bad}, time.Time{})
	if s.Errors != 1 {
		t.Errorf("errors = %d, want 1", s.Errors)
	}
	if s.Calls != 1 {
		t.Error("a failed call is still a call that was billed for its input")
	}
}
