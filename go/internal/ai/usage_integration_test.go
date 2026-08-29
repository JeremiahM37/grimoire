package ai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/usage"
)

// Every provider reports usage in its own shape, and a field name read wrongly
// costs nothing visible — the call still answers, the ledger just records zero
// tokens forever. So each shape is exercised against a server that replies the
// way the real API does.

type capture struct{ calls []usage.Call }

func (c *capture) Record(call usage.Call) error {
	c.calls = append(c.calls, call)
	return nil
}

// clientFor builds a Client configured for a REAL provider URL but talking to a
// local server.
//
// The configured base URL has to stay the cloud one: it is what decides who is
// billed, and pointing it at 127.0.0.1 makes the call classify as local
// inference — correctly, which is why the transport is redirected instead.
func clientFor(t *testing.T, srv *httptest.Server, cap *capture, base string, extra map[string]string) *Client {
	t.Helper()
	st := map[string]string{"llm_base_url": base}
	for k, v := range extra {
		st[k] = v
	}
	c := New(mapSettings(st), func(string) (string, error) { return "test-key", nil })
	client := srv.Client()
	client.Transport = rewriteHost{srv.URL, client.Transport}
	c.HTTP = client
	c.Usage = usage.NewRecorder(cap)
	return c.WithSurface("ask", "claude-code")
}

// An OpenAI-compatible reply, which is what fourteen of the supported
// providers speak.
func TestOpenAICompatibleUsageIsRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "hi"}}},
			"usage":   map[string]int{"prompt_tokens": 1200, "completion_tokens": 340},
		})
	}))
	defer srv.Close()
	cap := &capture{}
	c := clientFor(t, srv, cap, "https://api.openai.com/v1",
		map[string]string{"llm": "openai", "llm_model": "gpt-4o-mini"})
	if _, err := c.Complete("q", "openai"); err != nil {
		t.Fatal(err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(cap.calls))
	}
	got := cap.calls[0]
	if got.InputTokens != 1200 || got.OutputTokens != 340 {
		t.Errorf("tokens = %d/%d, want 1200/340", got.InputTokens, got.OutputTokens)
	}
	if got.Surface != "ask" || got.Agent != "claude-code" {
		t.Errorf("attribution lost: surface=%q agent=%q", got.Surface, got.Agent)
	}
	if !got.CostKnown || got.Cost <= 0 {
		t.Errorf("cost = %v known=%v; gpt-4o-mini is priced", got.Cost, got.CostKnown)
	}
	if got.LatencyMS < 0 {
		t.Errorf("latency = %d", got.LatencyMS)
	}
}

// Anthropic uses input_tokens/output_tokens, not prompt/completion.
func TestAnthropicUsageIsRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{{"text": "hi"}},
			"usage":   map[string]int{"input_tokens": 900, "output_tokens": 120},
		})
	}))
	defer srv.Close()
	cap := &capture{}
	c := New(mapSettings(map[string]string{"llm": "claude", "llm_model": "claude-sonnet-5"}),
		func(string) (string, error) { return "k", nil })
	client := srv.Client()
	client.Transport = rewriteHost{srv.URL, client.Transport}
	c.HTTP = client
	c.Usage = usage.NewRecorder(cap)
	if _, err := c.WithSurface("rerank", "").Complete("q", "claude"); err != nil {
		t.Fatal(err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("recorded %d calls", len(cap.calls))
	}
	got := cap.calls[0]
	if got.InputTokens != 900 || got.OutputTokens != 120 {
		t.Errorf("tokens = %d/%d, want 900/120 — Anthropic spells them "+
			"input_tokens/output_tokens", got.InputTokens, got.OutputTokens)
	}
	if got.Provider != usage.Anthropic {
		t.Errorf("provider = %q", got.Provider)
	}
	if !got.CostKnown {
		t.Error("claude-sonnet-5 is priced and reported unknown")
	}
}

// Ollama uses prompt_eval_count/eval_count, and is free.
func TestOllamaUsageIsRecordedAndFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": "hi", "prompt_eval_count": 640, "eval_count": 88,
		})
	}))
	defer srv.Close()
	cap := &capture{}
	c := New(mapSettings(map[string]string{"llm": "ollama", "ollama_url": srv.URL,
		"llm_model": "qwen3.5:4b"}), nil)
	c.HTTP = srv.Client()
	c.Usage = usage.NewRecorder(cap)
	if _, err := c.WithSurface("intent", "").Complete("q", "ollama"); err != nil {
		t.Fatal(err)
	}
	got := cap.calls[0]
	if got.InputTokens != 640 || got.OutputTokens != 88 {
		t.Errorf("tokens = %d/%d, want 640/88", got.InputTokens, got.OutputTokens)
	}
	if got.Provider != usage.Ollama {
		t.Errorf("provider = %q", got.Provider)
	}
	if !got.CostKnown || got.Cost != 0 {
		t.Errorf("local inference: cost=%v known=%v, want 0/true", got.Cost, got.CostKnown)
	}
}

// A failed call still costs latency and must still be visible — otherwise a
// provider that is down looks like a provider nobody used.
func TestFailedCallsAreRecordedWithTheError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	cap := &capture{}
	c := clientFor(t, srv, cap, "https://api.openai.com/v1",
		map[string]string{"llm": "openai", "llm_model": "gpt-4o"})
	if _, err := c.Complete("q", "openai"); err == nil {
		t.Fatal("a 429 was treated as success")
	}
	if len(cap.calls) != 1 {
		t.Fatalf("a failed call was not recorded (%d rows)", len(cap.calls))
	}
	if cap.calls[0].Error == "" {
		t.Error("the failure was recorded with no error text")
	}
}

// Accounting must never be able to break the thing it is accounting for.
type brokenStore struct{}

func (brokenStore) Record(usage.Call) error { return errBroken }

var errBroken = &recordErr{}

type recordErr struct{}

func (*recordErr) Error() string { return "ledger unavailable" }

func TestALedgerFailureDoesNotBreakTheAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "still answered"}}},
			"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 5},
		})
	}))
	defer srv.Close()
	c := New(mapSettings(map[string]string{"llm": "openai", "llm_base_url": srv.URL,
		"llm_model": "gpt-4o"}), func(string) (string, error) { return "k", nil })
	c.HTTP = srv.Client()
	c.Usage = usage.NewRecorder(brokenStore{})
	out, err := c.WithSurface("ask", "").Complete("q", "openai")
	if err != nil {
		t.Fatalf("a broken ledger failed the answer: %v", err)
	}
	if out != "still answered" {
		t.Errorf("answer = %q", out)
	}
}

// A nil recorder is the default for a server with no index; it must not panic.
func TestNoRecorderIsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}}})
	}))
	defer srv.Close()
	c := New(mapSettings(map[string]string{"llm": "openai", "llm_base_url": srv.URL}),
		func(string) (string, error) { return "k", nil })
	c.HTTP = srv.Client()
	if _, err := c.Complete("q", "openai"); err != nil {
		t.Fatalf("nil recorder broke the call: %v", err)
	}
}

// rewriteHost redirects an absolute provider URL at a local server. Anthropic's
// endpoint is hardcoded — correctly, an operator does not get to point "claude"
// at an arbitrary host — so the transport is what gets redirected.
type rewriteHost struct {
	base string
	next http.RoundTripper
}

func (rw rewriteHost) RoundTrip(r *http.Request) (*http.Response, error) {
	u, err := url.Parse(rw.base)
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host, r.Host = u.Scheme, u.Host, u.Host
	next := rw.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(r)
}

// The OpenAI-compatible shape is what most supported providers speak, so one
// reply body is exercised against each of their base URLs — the cost differs by
// provider even when the wire format does not.
func TestUsageAcrossOpenAICompatibleProviders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
			"usage":   map[string]int{"prompt_tokens": 1_000_000, "completion_tokens": 0},
		})
	}))
	defer srv.Close()

	for _, tc := range []struct {
		base, model string
		want        usage.Provider
		priced      bool
	}{
		{"https://api.openai.com/v1", "gpt-4o-mini", usage.OpenAI, true},
		{"https://api.groq.com/openai/v1", "llama-3.1-8b-instant", usage.Groq, true},
		{"https://api.deepseek.com/v1", "deepseek-chat", usage.DeepSeek, true},
		{"https://api.mistral.ai/v1", "mistral-large-latest", usage.Mistral, true},
		{"https://api.x.ai/v1", "grok-4", usage.XAI, true},
		{"https://api.cerebras.ai/v1", "llama3.1-8b", usage.Cerebras, true},
		{"https://api.perplexity.ai", "sonar-pro", usage.Perplexity, true},
		{"https://api.fireworks.ai/inference/v1", "llama-v3p3-70b-instruct", usage.Fireworks, true},
		{"https://api.deepinfra.com/v1/openai", "meta-llama/Llama-3.3-70B", usage.DeepInfra, true},
		{"https://api.together.xyz/v1", "Qwen/Qwen2.5-72B", usage.Together, true},
		{"https://generativelanguage.googleapis.com/v1beta/openai", "gemini-2.5-flash", usage.Google, true},
		{"https://x.openai.azure.com/openai/v1", "gpt-4o", usage.Azure, true},
		{"http://localhost:1234/v1", "local-model", usage.LMStudio, true},
		// Routed models are priced by the router, not by us. Reported unknown
		// rather than free, so a total is never quietly understated.
		{"https://openrouter.ai/api/v1", "anthropic/claude-sonnet-5", usage.OpenRouter, false},
	} {
		cap := &capture{}
		c := clientFor(t, srv, cap, tc.base, map[string]string{"llm": "openai", "llm_model": tc.model})
		if _, err := c.Complete("q", "openai"); err != nil {
			t.Errorf("%s: %v", tc.want, err)
			continue
		}
		if len(cap.calls) != 1 {
			t.Errorf("%s: recorded %d calls", tc.want, len(cap.calls))
			continue
		}
		got := cap.calls[0]
		if got.Provider != tc.want {
			t.Errorf("%s: detected as %q", tc.want, got.Provider)
		}
		if got.InputTokens != 1_000_000 {
			t.Errorf("%s: input tokens = %d", tc.want, got.InputTokens)
		}
		if got.CostKnown != tc.priced {
			t.Errorf("%s/%s: costKnown = %v, want %v", tc.want, tc.model, got.CostKnown, tc.priced)
		}
		if tc.priced && !tc.want.Local() && got.Cost <= 0 {
			t.Errorf("%s/%s: a million input tokens cost %v", tc.want, tc.model, got.Cost)
		}
	}
}
