package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/usage"
)

// CompleteOpts are the knobs a structured caller needs and the plain Complete
// never exposed: a system prompt kept apart from the content (so it stays
// cacheable and cannot be talked over by the content), a temperature, an
// output budget, a JSON contract, and a reasoning effort.
//
// Every field is optional and the zero value means "whatever the backend does
// by default" — not "whatever Complete does", because Complete pins a few
// choices (temperature 0.2 on OpenAI, 1024 tokens on Claude) that are right for
// short answers and wrong for extraction.
type CompleteOpts struct {
	System string
	// Temperature is a pointer because zero is a meaningful temperature.
	Temperature *float64
	// MaxTokens bounds the reply. Zero leaves it to the backend, except on
	// Claude, whose API requires a value and gets DefaultClaudeMaxTokens.
	MaxTokens int
	// JSON asks the backend for one JSON value: response_format on
	// OpenAI-compatible servers, format=json on Ollama, and an instruction on
	// Claude, which has no switch for it. Callers still decode leniently with
	// DecodeJSON — a model told to emit JSON does not always comply.
	JSON bool
	// Effort is the reasoning effort for models that think before answering.
	// Empty falls back to the llm_reasoning_effort setting; "off" disables
	// thinking where the backend has a way to say so.
	Effort string
	// Backend overrides the configured backend, as Complete's second argument
	// does.
	Backend string
}

// Completion is one reply and what it cost.
type Completion struct {
	Text string
	// FinishReason is normalised across backends: "length" means the reply
	// was cut off by the output budget, which a structured caller has to treat
	// as a failure rather than parse.
	FinishReason string
	Usage        usage.Tokens
	Model        string
}

// Truncated reports whether the output budget cut the reply short.
func (c Completion) Truncated() bool { return c.FinishReason == "length" }

// DefaultClaudeMaxTokens is the budget Complete has always used on Claude.
const DefaultClaudeMaxTokens = 1024

// MaxOutputTokens caps what any caller may ask for. Claude's larger models
// accept 64k output tokens; a caller wanting more is a bug, not a need.
const MaxOutputTokens = 64000

// Temp is a convenience for CompleteOpts.Temperature.
func Temp(t float64) *float64 { return &t }

// ErrNoBackend is returned when no model is configured.
var ErrNoBackend = errors.New("no llm backend configured")

// effort resolves the reasoning effort for one call.
func (c *Client) effort(o CompleteOpts) string {
	if e := strings.TrimSpace(o.Effort); e != "" {
		return strings.ToLower(e)
	}
	return strings.ToLower(strings.TrimSpace(c.get("llm_reasoning_effort")))
}

// extraBody is the operator's llm_extra_body setting: a JSON object merged into
// every OpenAI-compatible request. It exists because OpenAI-compatible servers
// disagree about everything beyond the core fields — DeepSeek turns thinking off
// with {"thinking":{"type":"disabled"}}, vLLM takes chat_template_kwargs,
// OpenRouter takes provider routing — and a setting per vendor quirk would never
// end. An unparseable value is ignored rather than failing every call.
func (c *Client) extraBody() map[string]any {
	raw := strings.TrimSpace(c.get("llm_extra_body"))
	if raw == "" {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil
	}
	return m
}

// applyOpenAIEffort writes the reasoning switch into an OpenAI-compatible body.
//
// "off" is the one value that is not passed through verbatim. OpenAI-style
// servers spell "do not think" differently, and the benchmark model's provider
// (DeepSeek) documents `thinking: {"type": "disabled"}` for its OpenAI
// endpoint, with reasoning_effort only meaningful when thinking is on. Any
// other value — low, medium, high, max, none — is sent as reasoning_effort.
func applyOpenAIEffort(body map[string]any, effort string) {
	switch effort {
	case "":
	case "off", "disabled", "false":
		body["thinking"] = map[string]any{"type": "disabled"}
	default:
		body["reasoning_effort"] = effort
	}
}

// CompleteWith runs one completion with options. It never changes what
// Complete sends: the existing callers' requests are built by Complete itself.
func (c *Client) CompleteWith(ctx context.Context, prompt string, o CompleteOpts) (Completion, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	backend := o.Backend
	if backend == "" {
		backend = c.Backend()
	}
	maxTok := min(o.MaxTokens, MaxOutputTokens)
	effort := c.effort(o)
	switch backend {
	case BackendClaudeCLI:
		comp, _, err := c.completeClaudeCLI(ctx, prompt, o)
		return comp, err
	case "ollama":
		body := map[string]any{"model": c.model(), "prompt": prompt, "stream": false, "think": false}
		switch effort {
		case "low", "medium", "high":
			body["think"] = effort
		case "on", "true":
			body["think"] = true
		}
		if o.System != "" {
			body["system"] = o.System
		}
		if o.JSON {
			body["format"] = "json"
		}
		opts := map[string]any{}
		if o.Temperature != nil {
			opts["temperature"] = *o.Temperature
		}
		if maxTok > 0 {
			opts["num_predict"] = maxTok
		}
		if len(opts) > 0 {
			body["options"] = opts
		}
		started := time.Now()
		out, err := c.postCtx(ctx, c.ollamaURL()+"/api/generate", nil, body)
		if err != nil {
			c.observe(backend, c.model(), usage.Tokens{}, started, err)
			return Completion{}, err
		}
		var r struct {
			Response   string `json:"response"`
			DoneReason string `json:"done_reason"`
			PromptEval int    `json:"prompt_eval_count"`
			Eval       int    `json:"eval_count"`
		}
		if err := json.Unmarshal(out, &r); err != nil {
			c.observe(backend, c.model(), usage.Tokens{}, started, err)
			return Completion{}, err
		}
		tok := usage.FromOllama(r.PromptEval, r.Eval)
		c.observe(backend, c.model(), tok, started, nil)
		return Completion{Text: strings.TrimSpace(r.Response), FinishReason: normFinish(r.DoneReason),
			Usage: tok, Model: c.model()}, nil

	case "openai":
		base := strings.TrimRight(c.get("llm_base_url"), "/")
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		headers := map[string]string{}
		if k := c.apiKey(); k != "" {
			headers["Authorization"] = "Bearer " + k
		}
		msgs := []map[string]string{}
		if o.System != "" {
			msgs = append(msgs, map[string]string{"role": "system", "content": o.System})
		}
		msgs = append(msgs, map[string]string{"role": "user", "content": prompt})
		body := map[string]any{"model": c.model(), "stream": false, "messages": msgs}
		// The operator's extra body goes in first so the per-call fields this
		// caller chose explicitly win over a blanket setting.
		for k, v := range c.extraBody() {
			body[k] = v
		}
		if o.Temperature != nil {
			body["temperature"] = *o.Temperature
		}
		if maxTok > 0 {
			body["max_tokens"] = maxTok
		}
		if o.JSON {
			body["response_format"] = map[string]any{"type": "json_object"}
		}
		applyOpenAIEffort(body, effort)
		started := time.Now()
		out, err := c.postCtx(ctx, base+"/chat/completions", headers, body)
		if err != nil {
			c.observe(backend, c.model(), usage.Tokens{}, started, err)
			return Completion{}, err
		}
		var r struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(out, &r); err != nil {
			c.observe(backend, c.model(), usage.Tokens{}, started, err)
			return Completion{}, err
		}
		tok := usage.FromOpenAI(r.Usage.PromptTokens, r.Usage.CompletionTokens)
		if len(r.Choices) == 0 {
			err := fmt.Errorf("no choices returned")
			c.observe(backend, c.model(), tok, started, err)
			return Completion{}, err
		}
		c.observe(backend, c.model(), tok, started, nil)
		return Completion{Text: strings.TrimSpace(r.Choices[0].Message.Content),
			FinishReason: normFinish(r.Choices[0].FinishReason), Usage: tok, Model: c.model()}, nil

	case "claude":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			key = c.apiKey()
		}
		if key == "" {
			return Completion{}, fmt.Errorf("no anthropic key")
		}
		model := c.model()
		if model == "qwen3.5:4b" {
			model = "claude-sonnet-5"
		}
		if maxTok <= 0 {
			maxTok = DefaultClaudeMaxTokens
		}
		body := map[string]any{"model": model, "max_tokens": maxTok,
			"messages": []map[string]string{{"role": "user", "content": prompt}}}
		system := o.System
		if o.JSON {
			// Claude has no JSON switch; the instruction lives in the system
			// prompt so the content cannot argue with it.
			system = strings.TrimSpace(system + "\n\nRespond with a single JSON value and nothing else.")
		}
		if system != "" {
			body["system"] = system
		}
		if o.Temperature != nil {
			body["temperature"] = *o.Temperature
		}
		started := time.Now()
		out, err := c.postCtx(ctx, "https://api.anthropic.com/v1/messages", map[string]string{
			"x-api-key": key, "anthropic-version": "2023-06-01"}, body)
		if err != nil {
			c.observe(backend, model, usage.Tokens{}, started, err)
			return Completion{}, err
		}
		var r struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			StopReason string `json:"stop_reason"`
			Usage      struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(out, &r); err != nil {
			c.observe(backend, model, usage.Tokens{}, started, err)
			return Completion{}, err
		}
		tok := usage.FromAnthropic(r.Usage.InputTokens, r.Usage.OutputTokens)
		c.observe(backend, model, tok, started, nil)
		var b strings.Builder
		for _, part := range r.Content {
			b.WriteString(part.Text)
		}
		return Completion{Text: strings.TrimSpace(b.String()), FinishReason: normFinish(r.StopReason),
			Usage: tok, Model: model}, nil
	}
	return Completion{}, ErrNoBackend
}

// normFinish maps each backend's stop vocabulary onto one: "length" when the
// output budget ended the reply, "stop" otherwise.
func normFinish(reason string) string {
	switch strings.ToLower(reason) {
	case "length", "max_tokens", "max_output_tokens":
		return "length"
	case "":
		return ""
	}
	return "stop"
}

// postCtx is post with a context, so a caller that is cancelled (a client
// that disconnected, an operation somebody cancelled) stops paying for the
// model call too.
func (c *Client) postCtx(ctx context.Context, url string, headers map[string]string, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// DecodeJSON reads the first JSON value out of a model reply into v.
//
// Models wrap JSON in markdown fences, prefix it with "Here is the JSON:",
// leave a <think> block in front of it, or keep talking after the closing
// brace. All four are tolerated: the value starts at the first { or [ after
// any reasoning block and ends where a JSON decoder says it ends, and anything
// after it is ignored.
func DecodeJSON(text string, v any) error {
	s := strings.TrimSpace(text)
	if i := strings.Index(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return fmt.Errorf("no JSON value in reply")
	}
	dec := json.NewDecoder(strings.NewReader(s[start:]))
	if err := dec.Decode(v); err != nil {
		// A fence's own backticks can sit inside a string the model forgot to
		// close; retry on the fenced body alone before giving up.
		if f := fencedBody(s); f != "" && f != s {
			return DecodeJSON(f, v)
		}
		return err
	}
	return nil
}

// fencedBody returns the contents of the first ``` fence, or "".
func fencedBody(s string) string {
	i := strings.Index(s, "```")
	if i < 0 {
		return ""
	}
	rest := s[i+3:]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	}
	if j := strings.Index(rest, "```"); j >= 0 {
		return strings.TrimSpace(rest[:j])
	}
	return ""
}
