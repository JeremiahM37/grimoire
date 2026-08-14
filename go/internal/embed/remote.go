package embed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Remote embedding backends: an OpenAI-compatible /embeddings endpoint and
// Ollama. Both are optional; the ladder in Chain falls back rather than failing,
// because indexing must never break because an AI service is down.

// Ollama embeds via a local Ollama server.
type Ollama struct {
	URL    string
	Model  string
	Client *http.Client
}

func NewOllama(url, model string) *Ollama {
	if model == "" {
		model = "nomic-embed-text"
	}
	return &Ollama{
		URL: strings.TrimRight(url, "/"), Model: model,
		Client: &http.Client{Timeout: 120 * time.Second},
	}
}

func (o *Ollama) Signature() string { return "ollama:" + o.Model }

// Dim is not known until the model answers; retrieval only needs vectors to be
// mutually consistent, so this reports 0 for "ask the model".
func (o *Ollama) Dim() int { return 0 }

// Embed sends the whole batch in one round-trip via /api/embed, falling back to
// the older per-text /api/embeddings endpoint. Bulk ingestion one chunk at a
// time crawls on a large vault, which is why the batch path comes first.
func (o *Ollama) Embed(texts []string) [][]float32 {
	if vecs, err := o.embedBatch(texts); err == nil {
		return vecs
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := o.embedOne(t)
		if err != nil {
			// a partial failure must not poison the index with a wrong-length
			// vector; an empty one is skipped by the caller
			out[i] = nil
			continue
		}
		out[i] = v
	}
	return out
}

func (o *Ollama) embedBatch(texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": o.Model, "input": texts})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := o.post(o.URL+"/api/embed", body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama /api/embed returned %d vectors for %d texts",
			len(resp.Embeddings), len(texts))
	}
	return resp.Embeddings, nil
}

func (o *Ollama) embedOne(text string) ([]float32, error) {
	body, err := json.Marshal(map[string]any{"model": o.Model, "prompt": text})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := o.post(o.URL+"/api/embeddings", body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Embedding) == 0 {
		return nil, fmt.Errorf("ollama returned an empty embedding")
	}
	return resp.Embedding, nil
}

func (o *Ollama) post(url string, body []byte, out any) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: %s: %s", url, resp.Status, strings.TrimSpace(string(raw)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// OpenAI embeds via any OpenAI-compatible /embeddings endpoint (OpenAI,
// OpenRouter, Together, vLLM, LM Studio, LiteLLM…). One protocol, no adapters.
type OpenAI struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
}

func NewOpenAI(baseURL, apiKey, model string) *OpenAI {
	if model == "" {
		model = "nomic-embed-text"
	}
	return &OpenAI{
		BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, Model: model,
		Client: &http.Client{Timeout: 60 * time.Second},
	}
}

func (o *OpenAI) Signature() string { return "openai:" + o.BaseURL + ":" + o.Model }
func (o *OpenAI) Dim() int          { return 0 }

func (o *OpenAI) Embed(texts []string) [][]float32 {
	body, err := json.Marshal(map[string]any{"model": o.Model, "input": texts})
	if err != nil {
		return make([][]float32, len(texts))
	}
	req, err := http.NewRequest(http.MethodPost, o.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return make([][]float32, len(texts))
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return make([][]float32, len(texts))
	}
	defer resp.Body.Close()
	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return make([][]float32, len(texts))
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		if i < len(parsed.Data) {
			out[i] = parsed.Data[i].Embedding
		}
	}
	return out
}

// Chain is the fallback ladder: an explicit OpenAI-compatible endpoint, else
// Ollama, else a local model, else the hasher.
//
// Every remote failure falls DOWN the ladder rather than propagating, because
// indexing must never break on an AI outage — a degraded vector is recoverable
// by re-indexing, a failed index is not.
type Chain struct {
	Backends []Backend
}

// Backend is one embedding implementation.
type Backend interface {
	Embed(texts []string) [][]float32
	Signature() string
	Dim() int
}

func (c *Chain) Signature() string {
	if len(c.Backends) == 0 {
		return Hash{}.Signature()
	}
	return c.Backends[0].Signature()
}

func (c *Chain) Dim() int {
	if len(c.Backends) == 0 {
		return Dim
	}
	return c.Backends[0].Dim()
}

func (c *Chain) Embed(texts []string) [][]float32 {
	for _, b := range c.Backends {
		out := b.Embed(texts)
		if len(out) != len(texts) {
			continue
		}
		ok := true
		for _, v := range out {
			if len(v) == 0 {
				ok = false
				break
			}
		}
		if ok {
			return out
		}
	}
	return Hash{}.Embed(texts)
}
