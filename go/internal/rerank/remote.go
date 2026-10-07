package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// RemoteConfig configures a reranker served over HTTP.
type RemoteConfig struct {
	// URL is the service's base URL ("https://api.example.com/v1") or its
	// full rerank endpoint; "/rerank" is appended unless the path already
	// ends with it.
	URL string
	// APIKey is sent as a bearer token when set.
	APIKey string
	// Model is passed through as "model"; services that host one model
	// ignore it.
	Model string
	// Timeout bounds one request; 0 means 30s.
	Timeout time.Duration
	// Client overrides the HTTP client (its Timeout is replaced by Timeout).
	Client *http.Client
}

// Remote calls a /rerank endpoint. The request is the shape most rerank
// services and self-hosted inference servers share:
//
//	{"model": "...", "query": "...", "documents": ["...", ...], "top_n": N}
//
// and the response is either {"results": [{"index": i, "relevance_score": s}]}
// or a bare [{"index": i, "score": s}] list. A server that rejects
// "documents" and asks for "texts" — the other common field name — is
// retried once with that, and remembered.
type Remote struct {
	cfg      RemoteConfig
	endpoint string
	client   *http.Client
	useTexts atomic.Bool
}

// NewRemote returns a remote reranker.
func NewRemote(cfg RemoteConfig) (*Remote, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if base == "" {
		return nil, errors.New("remote reranker needs a URL")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("remote reranker URL %q must be http(s)", cfg.URL)
	}
	if !strings.HasSuffix(base, "/rerank") {
		base += "/rerank"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	c := &http.Client{}
	if cfg.Client != nil {
		cp := *cfg.Client
		c = &cp
	}
	c.Timeout = cfg.Timeout
	return &Remote{cfg: cfg, endpoint: base, client: c}, nil
}

// Name identifies the backend.
func (r *Remote) Name() string {
	if r.cfg.Model != "" {
		return "remote:" + r.cfg.Model
	}
	return "remote:" + r.endpoint
}

type remoteResult struct {
	Index          *int     `json:"index"`
	RelevanceScore *float64 `json:"relevance_score"`
	Score          *float64 `json:"score"`
}

// Score sends every document in one request and returns the scores in input
// order. A document the service leaves out of its results scores -Inf.
func (r *Remote) Score(ctx context.Context, query string, docs []string) ([]float32, error) {
	if len(docs) == 0 {
		return []float32{}, nil
	}
	body, status, err := r.post(ctx, query, docs, r.useTexts.Load())
	if err == nil && !r.useTexts.Load() && wantsTexts(status, body) {
		r.useTexts.Store(true)
		body, status, err = r.post(ctx, query, docs, true)
	}
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("rerank %s: HTTP %d: %s", r.endpoint, status, snippet(body))
	}
	results, err := parseResults(body)
	if err != nil {
		return nil, fmt.Errorf("rerank %s: %w", r.endpoint, err)
	}
	out := make([]float32, len(docs))
	for i := range out {
		out[i] = float32(math.Inf(-1))
	}
	for _, res := range results {
		if res.Index == nil || *res.Index < 0 || *res.Index >= len(docs) {
			return nil, fmt.Errorf("rerank %s: result index out of range", r.endpoint)
		}
		s := res.RelevanceScore
		if s == nil {
			s = res.Score
		}
		if s == nil {
			return nil, fmt.Errorf("rerank %s: result %d has no score", r.endpoint, *res.Index)
		}
		out[*res.Index] = float32(*s)
	}
	return out, nil
}

func (r *Remote) post(ctx context.Context, query string, docs []string, texts bool) ([]byte, int, error) {
	req := map[string]any{"query": query}
	if texts {
		req["texts"] = docs
	} else {
		req["documents"] = docs
		req["top_n"] = len(docs)
	}
	if r.cfg.Model != "" {
		req["model"] = r.cfg.Model
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, 0, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	if r.cfg.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)
	}
	resp, err := r.client.Do(hreq)
	if err != nil {
		return nil, 0, fmt.Errorf("rerank %s: %w", r.endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("rerank %s: reading response: %w", r.endpoint, err)
	}
	return body, resp.StatusCode, nil
}

// wantsTexts recognises a validation error about a missing "texts" field.
func wantsTexts(status int, body []byte) bool {
	if status != http.StatusUnprocessableEntity && status != http.StatusBadRequest {
		return false
	}
	return bytes.Contains(body, []byte("texts"))
}

func parseResults(body []byte) ([]remoteResult, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var list []remoteResult
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, fmt.Errorf("parsing response: %w", err)
		}
		return list, nil
	}
	var obj struct {
		Results []remoteResult `json:"results"`
		Data    []remoteResult `json:"data"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	if obj.Results == nil && obj.Data == nil {
		return nil, errors.New("response has no results")
	}
	return append(obj.Results, obj.Data...), nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
