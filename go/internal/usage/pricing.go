package usage

import (
	"net/url"
	"strings"
)

// What a model call costs, and who to bill it to.
//
// Grimoire is not a proxy for anyone's agent — it never sees the conversation
// an agent has with its own provider, and any dashboard claiming to show "your
// AI spend" from here would be inventing most of it. What it CAN account for
// exactly is the calls it makes ITSELF: ask_notes, reranking, summarising,
// intent classification. Those run on a key the operator configured, against a
// model the operator chose, and until now their cost was invisible.
//
// Prices are per MILLION tokens in USD, which is how every provider quotes
// them. They are a reference table, not a billing system: providers change
// prices, negotiate rates, and meter differently. PricesUpdated says when this
// table was last checked, and every surface that reports money says so too —
// a cost figure with no date on it is a guess wearing a decimal point.

// PricesUpdated is when the table below was last verified against published
// rates. Reported alongside every cost so nobody reads a stale number as live.
const PricesUpdated = "2026-08-29"

// Price is what one model costs per million tokens.
type Price struct {
	Input  float64
	Output float64
}

// Provider is a normalised backend name.
type Provider string

// The providers this recognises. Local ones are free by definition — the
// electricity is real but it is not a line item anybody can reconcile.
const (
	OpenAI     Provider = "openai"
	Anthropic  Provider = "anthropic"
	Google     Provider = "google"
	Groq       Provider = "groq"
	Together   Provider = "together"
	Fireworks  Provider = "fireworks"
	DeepSeek   Provider = "deepseek"
	Mistral    Provider = "mistral"
	OpenRouter Provider = "openrouter"
	Perplexity Provider = "perplexity"
	XAI        Provider = "xai"
	Cerebras   Provider = "cerebras"
	DeepInfra  Provider = "deepinfra"
	Azure      Provider = "azure"
	Ollama     Provider = "ollama"
	LMStudio   Provider = "lmstudio"
	VLLM       Provider = "vllm"
	Unknown    Provider = "unknown"
)

// Local reports whether a provider runs on hardware the operator owns, and so
// has no per-token price to report.
func (p Provider) Local() bool {
	switch p {
	case Ollama, LMStudio, VLLM:
		return true
	}
	return false
}

// hostProviders maps an API host to the provider serving it.
//
// Matched on host rather than configured name because grimoire's "openai"
// backend speaks to anything OpenAI-compatible — the base URL is what actually
// decides who is being billed, and an operator pointing it at Groq is not
// paying OpenAI's prices.
var hostProviders = map[string]Provider{
	"api.openai.com":                    OpenAI,
	"api.anthropic.com":                 Anthropic,
	"generativelanguage.googleapis.com": Google,
	"api.groq.com":                      Groq,
	"api.together.xyz":                  Together,
	"api.together.ai":                   Together,
	"api.fireworks.ai":                  Fireworks,
	"api.deepseek.com":                  DeepSeek,
	"api.mistral.ai":                    Mistral,
	"openrouter.ai":                     OpenRouter,
	"api.perplexity.ai":                 Perplexity,
	"api.x.ai":                          XAI,
	"api.cerebras.ai":                   Cerebras,
	"api.deepinfra.com":                 DeepInfra,
}

// ProviderFor identifies who serves a base URL.
func ProviderFor(backend, baseURL string) Provider {
	switch backend {
	case "ollama":
		return Ollama
	case "claude":
		return Anthropic
	}
	if baseURL == "" {
		return OpenAI // the openai backend's own default
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return Unknown
	}
	host := strings.ToLower(u.Hostname())
	if p, ok := hostProviders[host]; ok {
		return p
	}
	// Azure names a resource per customer, so it cannot be an exact match.
	if strings.HasSuffix(host, ".openai.azure.com") {
		return Azure
	}
	// Anything on a private address or localhost is somebody's own box.
	if host == "localhost" || host == "127.0.0.1" || host == "::1" ||
		strings.HasPrefix(host, "192.168.") || strings.HasPrefix(host, "10.") ||
		strings.HasPrefix(host, "100.") {
		if strings.Contains(baseURL, ":1234") {
			return LMStudio // its documented default port
		}
		return VLLM
	}
	return Unknown
}

// prices is the reference table, keyed by provider then by a model prefix.
//
// Prefixes rather than exact ids: providers ship dated snapshots
// ("claude-sonnet-5-20260101", "gpt-5-mini-2026-04") that share a price with
// the family. An exact-match table goes stale the day a snapshot ships and
// silently reports zero.
var prices = map[Provider]map[string]Price{
	OpenAI: {
		"gpt-5":       {1.25, 10.00},
		"gpt-5-mini":  {0.25, 2.00},
		"gpt-5-nano":  {0.05, 0.40},
		"gpt-4.1":     {2.00, 8.00},
		"gpt-4o":      {2.50, 10.00},
		"gpt-4o-mini": {0.15, 0.60},
		"o3":          {2.00, 8.00},
		"o4-mini":     {1.10, 4.40},
	},
	Anthropic: {
		"claude-opus-5":    {15.00, 75.00},
		"claude-sonnet-5":  {3.00, 15.00},
		"claude-haiku-4":   {0.80, 4.00},
		"claude-3-5-haiku": {0.80, 4.00},
	},
	Google: {
		"gemini-2.5-pro":   {1.25, 10.00},
		"gemini-2.5-flash": {0.30, 2.50},
		"gemini-2.0-flash": {0.10, 0.40},
	},
	Groq: {
		"llama-3.3-70b": {0.59, 0.79},
		"llama-3.1-8b":  {0.05, 0.08},
		"mixtral":       {0.24, 0.24},
	},
	Together: {
		"meta-llama/Llama-3.3-70B": {0.88, 0.88},
		"Qwen":                     {0.60, 0.60},
		"mistralai":                {0.60, 0.60},
	},
	Fireworks: {
		"llama-v3p3-70b": {0.90, 0.90},
		"qwen":           {0.90, 0.90},
	},
	DeepSeek: {
		"deepseek-chat":     {0.27, 1.10},
		"deepseek-reasoner": {0.55, 2.19},
	},
	Mistral: {
		"mistral-large": {2.00, 6.00},
		"mistral-small": {0.20, 0.60},
		"codestral":     {0.30, 0.90},
		"open-mistral":  {0.25, 0.25},
	},
	Perplexity: {
		"sonar-pro": {3.00, 15.00},
		"sonar":     {1.00, 1.00},
	},
	XAI: {
		"grok-4":      {3.00, 15.00},
		"grok-3-mini": {0.30, 0.50},
		"grok-3":      {3.00, 15.00},
	},
	Cerebras: {
		"llama-3.3-70b": {0.85, 1.20},
		"llama3.1-8b":   {0.10, 0.10},
		"qwen-3":        {0.60, 1.20},
	},
	DeepInfra: {
		"meta-llama": {0.23, 0.40},
		"Qwen":       {0.27, 0.40},
	},
	Azure: {
		"gpt-5":       {1.25, 10.00},
		"gpt-4.1":     {2.00, 8.00},
		"gpt-4o":      {2.50, 10.00},
		"gpt-4o-mini": {0.15, 0.60},
	},
	// OpenRouter proxies hundreds of models at per-model rates it sets itself.
	// Guessing a price for "whatever you routed to" would be fiction, so calls
	// through it are recorded with tokens and no cost, and the dashboard says
	// so rather than showing zero as though it were free.
	OpenRouter: {},
}

// PriceFor returns the price for a model, and whether one is known.
//
// Unknown is a real answer and is reported as such. A missing price rendered as
// $0.00 is the failure mode this avoids: it makes an unmetered provider look
// free, which is exactly backwards.
func PriceFor(p Provider, model string) (Price, bool) {
	if p.Local() {
		return Price{}, true // genuinely free, not merely unknown
	}
	table, ok := prices[p]
	if !ok {
		return Price{}, false
	}
	m := strings.ToLower(strings.TrimSpace(model))
	// Longest matching prefix wins, so "gpt-5-mini" does not price as "gpt-5".
	best, bestLen, found := Price{}, -1, false
	for prefix, price := range table {
		lp := strings.ToLower(prefix)
		if strings.Contains(m, lp) && len(lp) > bestLen {
			best, bestLen, found = price, len(lp), true
		}
	}
	return best, found
}

// Cost prices one call. The bool reports whether the figure is real; false
// means no price is known and the caller must not present zero as a total.
func Cost(p Provider, model string, inTokens, outTokens int) (float64, bool) {
	price, ok := PriceFor(p, model)
	if !ok {
		return 0, false
	}
	const perMillion = 1_000_000.0
	return float64(inTokens)/perMillion*price.Input +
		float64(outTokens)/perMillion*price.Output, true
}

// Providers lists every provider this knows how to price, for the UI.
func Providers() []Provider {
	return []Provider{
		OpenAI, Anthropic, Google, Groq, Together, Fireworks, DeepSeek,
		Mistral, OpenRouter, Perplexity, XAI, Cerebras, DeepInfra, Azure,
		Ollama, LMStudio, VLLM,
	}
}
