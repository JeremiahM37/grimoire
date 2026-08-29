package usage

import "testing"

// The base URL decides who is billed, not the configured backend name.
// Grimoire's "openai" backend speaks to anything OpenAI-compatible, and an
// operator pointing it at Groq is not paying OpenAI's prices.
func TestProviderIsDetectedFromTheBaseURL(t *testing.T) {
	for _, tc := range []struct {
		backend, base string
		want          Provider
	}{
		{"ollama", "", Ollama},
		{"claude", "", Anthropic},
		{"openai", "", OpenAI},
		{"openai", "https://api.openai.com/v1", OpenAI},
		{"openai", "https://api.groq.com/openai/v1", Groq},
		{"openai", "https://api.together.xyz/v1", Together},
		{"openai", "https://api.together.ai/v1", Together},
		{"openai", "https://api.fireworks.ai/inference/v1", Fireworks},
		{"openai", "https://api.deepseek.com/v1", DeepSeek},
		{"openai", "https://api.mistral.ai/v1", Mistral},
		{"openai", "https://openrouter.ai/api/v1", OpenRouter},
		{"openai", "https://api.perplexity.ai", Perplexity},
		{"openai", "https://api.x.ai/v1", XAI},
		{"openai", "https://api.cerebras.ai/v1", Cerebras},
		{"openai", "https://api.deepinfra.com/v1/openai", DeepInfra},
		{"openai", "https://generativelanguage.googleapis.com/v1beta/openai", Google},
		{"openai", "https://my-resource.openai.azure.com/openai/v1", Azure},
		{"openai", "http://localhost:1234/v1", LMStudio},
		{"openai", "http://127.0.0.1:8000/v1", VLLM},
		{"openai", "http://100.127.85.58:8000/v1", VLLM},
		{"openai", "https://something-nobody-has-heard-of.example/v1", Unknown},
	} {
		if got := ProviderFor(tc.backend, tc.base); got != tc.want {
			t.Errorf("ProviderFor(%q, %q) = %q, want %q", tc.backend, tc.base, got, tc.want)
		}
	}
}

// Fifteen-plus providers is the claim; this is what keeps it true.
func TestEveryListedProviderIsPricedOrLocal(t *testing.T) {
	ps := Providers()
	if len(ps) < 15 {
		t.Fatalf("only %d providers listed", len(ps))
	}
	for _, p := range ps {
		if p.Local() {
			continue
		}
		if p == OpenRouter {
			continue // priced per routed model; deliberately empty, see below
		}
		if len(prices[p]) == 0 {
			t.Errorf("%s is listed as a provider with no prices on file", p)
		}
	}
}

// Longest prefix wins, or "gpt-5-mini" silently bills at "gpt-5" rates — an
// eight-fold error in the direction that looks fine.
func TestLongestModelPrefixWins(t *testing.T) {
	full, ok := PriceFor(OpenAI, "gpt-5")
	if !ok {
		t.Fatal("gpt-5 unpriced")
	}
	mini, ok := PriceFor(OpenAI, "gpt-5-mini-2026-04-01")
	if !ok {
		t.Fatal("a dated gpt-5-mini snapshot was unpriced")
	}
	if mini.Input >= full.Input {
		t.Errorf("gpt-5-mini priced at %v, gpt-5 at %v — the longer prefix must win",
			mini.Input, full.Input)
	}
}

// Providers ship dated snapshots that share a family price. An exact-match
// table goes stale the day one ships and silently reports zero.
func TestDatedModelSnapshotsArePriced(t *testing.T) {
	for _, tc := range []struct {
		p     Provider
		model string
	}{
		{Anthropic, "claude-sonnet-5-20260101"},
		{OpenAI, "gpt-4o-2026-05-13"},
		{Google, "gemini-2.5-flash-preview"},
		{DeepSeek, "deepseek-chat"},
	} {
		if _, ok := PriceFor(tc.p, tc.model); !ok {
			t.Errorf("%s/%s has no price; a dated snapshot must inherit its family's",
				tc.p, tc.model)
		}
	}
}

// Local providers are free — a real answer, not a missing one.
func TestLocalProvidersAreFreeNotUnknown(t *testing.T) {
	for _, p := range []Provider{Ollama, LMStudio, VLLM} {
		cost, known := Cost(p, "qwen3.5:4b", 1_000_000, 1_000_000)
		if !known {
			t.Errorf("%s reported an unknown price; local inference is free", p)
		}
		if cost != 0 {
			t.Errorf("%s charged %v for local inference", p, cost)
		}
	}
}

// An unknown price must NOT read as zero cost. Zero presented as a total makes
// an unmetered provider look free, which is exactly backwards.
func TestUnknownPricesAreReportedNotZeroed(t *testing.T) {
	cost, known := Cost(Unknown, "some-model", 1000, 1000)
	if known {
		t.Error("an unknown provider reported a known price")
	}
	if cost != 0 {
		t.Errorf("cost = %v, want 0 alongside known=false", cost)
	}
	// OpenRouter routes to hundreds of models at rates it sets; guessing would
	// be fiction, so it must report unknown rather than free.
	if _, known := Cost(OpenRouter, "anthropic/claude-sonnet-5", 1000, 1000); known {
		t.Error("OpenRouter reported a known price for a routed model")
	}
}

func TestCostArithmetic(t *testing.T) {
	// 1M in and 1M out at claude-sonnet-5's $3/$15.
	cost, known := Cost(Anthropic, "claude-sonnet-5", 1_000_000, 1_000_000)
	if !known {
		t.Fatal("unpriced")
	}
	if want := 18.0; cost != want {
		t.Errorf("cost = %v, want %v", cost, want)
	}
	// And it scales linearly rather than by tier.
	half, _ := Cost(Anthropic, "claude-sonnet-5", 500_000, 500_000)
	if half != 9.0 {
		t.Errorf("half the tokens cost %v, want 9", half)
	}
}
