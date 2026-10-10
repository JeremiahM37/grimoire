package rulecheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLM writes candidate checks with a local model served by Ollama. The model
// only proposes; every candidate is validated, compiled and measured before it
// is trusted, and nothing leaves the machine.
type LLM struct {
	URL    string // http://host:11434
	Model  string
	NumCtx int
	HTTP   *http.Client
}

// DefaultLLM is the lab's local model.
func DefaultLLM(url string) *LLM {
	return &LLM{URL: strings.TrimRight(url, "/"), Model: "qwen3.6:35b-a3b", NumCtx: 16384,
		HTTP: &http.Client{Timeout: 240 * time.Second}}
}

// The reply is requested as plain JSON with thinking off: on Ollama 0.20 a
// schema-constrained reply with thinking on spent the whole token budget
// thinking (82 s, empty answer), and think:false with a schema is unreliable.
// The reply is validated here instead.
const llmPrompt = `You turn a standing rule for a coding agent into checks over the agent's tool calls.
A tool call has a tool name (Bash, Edit, Write, ...) and a target: the shell command, or the file path.
Check shapes:
- forbid: the call must never match "action" (e.g. rule "never force-push" -> action "\bgit\s+push\b.*(--force|-f\b)").
- require_before: a call matching "action" must be preceded earlier in the same session by a call matching "before" (e.g. "run the tests before pushing": action "\bgit\s+push\b", before "\b(pytest|go test|npm test)\b").
- require_with: a call matching "action" must itself also match "with" (e.g. "always pass -y to apt": action "\bapt(-get)?\s+install\b", with "\s-y\b").
Optional: tools (default ["Bash"]; use ["Edit","Write"] for file paths), scope_cwd (regex over the working directory, only when the rule names a repo or directory), scope_agent.
Patterns are Go RE2 regular expressions over the target text: no lookahead, no backreferences. Keep them specific: they must match the forbidden or required action and little else. Under 120 characters each.
Reply with only a JSON object {"checks":[{"shape":...,"tools":[...],"action":...,"before":...,"with":...,"scope_cwd":...,"scope_agent":...,"why":...}]}, omitting fields that do not apply. Return up to 3 checks, best first. If the rule is a preference about style, tone or design that no tool call can reveal (for example "write short answers"), return {"checks":[]}.

Rule:
`

// Candidates asks the model for checks for one rule.
func (l *LLM) Candidates(ctx context.Context, ruleText string) ([]Candidate, error) {
	body, _ := json.Marshal(map[string]any{
		"model": l.Model, "stream": false, "think": false,
		"messages": []map[string]string{{"role": "user", "content": llmPrompt + clip(ruleText, 1500)}},
		"options":  map[string]any{"num_ctx": l.NumCtx, "temperature": 0, "num_predict": 1500},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", l.URL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ollama %d: %s", resp.StatusCode, clip(string(raw), 200))
	}
	var env struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	return ParseLLM(env.Message.Content)
}

// ParseLLM validates the model's JSON. Candidates that do not compile are
// dropped, never repaired.
func ParseLLM(content string) ([]Candidate, error) {
	content = strings.TrimSpace(content)
	if i, j := strings.Index(content, "{"), strings.LastIndex(content, "}"); i >= 0 && j > i {
		content = content[i : j+1]
	}
	var out struct {
		Checks []struct {
			Shape, Action, Before, With, Why string
			Tools                            []string
			ScopeCwd                         string `json:"scope_cwd"`
			ScopeAgent                       string `json:"scope_agent"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return nil, fmt.Errorf("model did not return the schema: %v", err)
	}
	var cands []Candidate
	for _, c := range out.Checks {
		sp := Spec{Shape: c.Shape, Tools: c.Tools, Action: c.Action, Before: c.Before, With: c.With,
			Scope: Scope{Cwd: c.ScopeCwd, Agent: c.ScopeAgent}}
		if vague.MatchString(strings.TrimSpace(sp.Scope.Cwd)) { // ".*" means "anywhere"
			sp.Scope.Cwd = ""
		}
		if sp.Shape == ShapeForbid {
			sp.Before, sp.With = "", ""
		}
		if _, err := sp.Compile(); err != nil {
			continue
		}
		cands = append(cands, Candidate{Spec: sp, Source: "llm", Why: clip(c.Why, 160)})
		if len(cands) == 3 {
			break
		}
	}
	return cands, nil
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Merge combines deterministic and model candidates, dropping duplicates by
// their action pattern, capped at MaxCandidates.
func Merge(groups ...[]Candidate) []Candidate {
	var out []Candidate
	seen := map[string]bool{}
	for _, g := range groups {
		for _, c := range g {
			k := c.Shape + "|" + c.Action + "|" + c.Before + "|" + c.With + "|" + c.Scope.Cwd + "|" + strings.Join(orBash(c.Tools), ",")
			if seen[k] || len(out) >= MaxCandidates {
				continue
			}
			seen[k] = true
			out = append(out, c)
		}
	}
	return out
}
