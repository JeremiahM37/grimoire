// Package decide asks a typed-decision model a bounded question about a piece
// of state and gets back a probability or a label, not prose.
//
// The wire format is TypeSafe's Jev API (POST /v1/systemone with a state, a
// model and named questions of type noul, choice or score). Two kinds of server
// speak it: Jev itself, hosted and paid, and laya-serve, which runs Convai's
// open-weight Laya models locally and exposes the same endpoint. One client
// covers both; which one a deployment uses is a URL.
//
// Everything that calls this has a fallback, and a decision server that is
// down, slow or unconfigured must change nothing but the quality of a guess.
package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Question types.
const (
	Noul   = "noul"   // yes/no; the answer is P(yes)
	Choice = "choice" // one of the options in Criteria
	Score  = "score"  // a point on the scale in Criteria
)

// Question is one typed question. Criteria is a map of label to description
// for a choice, a list of levels for a score, and optionally a map with
// "true"/"false" descriptions for a noul.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Answer is one answer. Exactly one of the value fields is meaningful,
// according to Type.
type Answer struct {
	Type       string
	Noul       float64 // P(yes), for noul
	Choice     string  // the chosen label, for choice
	Score      float64 // the point on the scale, for score
	Confidence float64 // when the server reports one
	// Probabilities is the distribution over a choice's options, when the
	// server reports it (Jev does).
	Probabilities map[string]float64
}

// Result is one response: the answers by name, which model gave them, and
// what the call consumed, so a caller can book its cost.
type Result struct {
	Answers      map[string]Answer
	Model        string
	InputTokens  int
	OutputTokens int
}

// Client calls one decision server.
type Client struct {
	// BaseURL is the server root: https://api.typesafe.ai for Jev, or
	// http://127.0.0.1:8000 for a local laya-serve.
	BaseURL string
	Model   string
	APIKey  string
	HTTP    *http.Client
}

// DefaultTimeout bounds a decision. Decisions sit on write paths, and a slow
// one must give up and let the fallback answer rather than stall the write.
const DefaultTimeout = 3 * time.Second

// maxResponse bounds what is read back. A decision response is a few hundred
// bytes; anything near this is not one.
const maxResponse = 1 << 20

// ErrOff means no decision server is configured.
var ErrOff = errors.New("no decision server configured")

// Ask sends one state and its questions and returns the answers by name.
func (c *Client) Ask(ctx context.Context, state any, questions map[string]Question) (Result, error) {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" {
		return Result{}, ErrOff
	}
	model := c.Model
	if model == "" {
		model = "jev-latest"
	}
	body, err := json.Marshal(map[string]any{"state": state, "model": model, "questions": questions})
	if err != nil {
		return Result{}, err
	}
	url := strings.TrimRight(c.BaseURL, "/")
	if !strings.HasSuffix(url, "/v1/systemone") {
		url += "/v1/systemone"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return Result{}, err
	}
	if resp.StatusCode != http.StatusOK {
		// The body can echo the request; keep the error short and never
		// include the key.
		msg := strings.TrimSpace(string(raw))
		if c.APIKey != "" {
			msg = strings.ReplaceAll(msg, c.APIKey, "[redacted]")
		}
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return Result{}, fmt.Errorf("decision server: %s: %s", resp.Status, msg)
	}
	return parse(raw)
}

// parse reads {"answers": {name: {"type": T, T: value, ...}}}. Unknown fields
// are ignored, so a server that adds routing or usage details still parses.
func parse(raw []byte) (Result, error) {
	var out struct {
		Model   string                                `json:"model"`
		Answers map[string]map[string]json.RawMessage `json:"answers"`
		Usage   struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("decision server: unreadable answer: %w", err)
	}
	answers := make(map[string]Answer, len(out.Answers))
	for name, fields := range out.Answers {
		var a Answer
		_ = json.Unmarshal(fields["type"], &a.Type)
		if v, ok := fields["confidence"]; ok {
			_ = json.Unmarshal(v, &a.Confidence)
		}
		if v, ok := fields["probabilities"]; ok {
			_ = json.Unmarshal(v, &a.Probabilities)
		}
		switch a.Type {
		case Noul:
			if err := json.Unmarshal(fields[Noul], &a.Noul); err != nil {
				return Result{}, fmt.Errorf("decision server: %s: noul answer is not a number", name)
			}
			if a.Noul < 0 || a.Noul > 1 {
				return Result{}, fmt.Errorf("decision server: %s: probability %v out of range", name, a.Noul)
			}
		case Choice:
			if err := unmarshalChoice(fields[Choice], &a); err != nil {
				return Result{}, fmt.Errorf("decision server: %s: %w", name, err)
			}
		case Score:
			if err := json.Unmarshal(fields[Score], &a.Score); err != nil {
				return Result{}, fmt.Errorf("decision server: %s: score answer is not a number", name)
			}
		default:
			return Result{}, fmt.Errorf("decision server: %s: unknown answer type %q", name, a.Type)
		}
		answers[name] = a
	}
	return Result{Answers: answers, Model: out.Model,
		InputTokens: out.Usage.Input, OutputTokens: out.Usage.Output}, nil
}

// unmarshalChoice accepts the label as a bare string, or as an object with a
// label and a confidence, since servers differ on which they send.
func unmarshalChoice(raw json.RawMessage, a *Answer) error {
	if err := json.Unmarshal(raw, &a.Choice); err == nil {
		return nil
	}
	var obj struct {
		Label      string  `json:"label"`
		Choice     string  `json:"choice"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return errors.New("choice answer is neither a label nor an object")
	}
	a.Choice = obj.Label
	if a.Choice == "" {
		a.Choice = obj.Choice
	}
	if obj.Confidence > 0 {
		a.Confidence = obj.Confidence
	}
	return nil
}
