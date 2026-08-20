// Package mcp exposes the substrate over the Model Context Protocol.
//
// Port of server/mcp_server.py. This is the interface agents actually use, and
// the two mechanisms that decide whether they use it at all live here:
//
//   - `instructions`, returned on initialize. Mounted-but-unadvertised tools are
//     close to invisible to an agent; spelling out "call get_briefing first,
//     consult before assuming project facts" is what moved adoption from near
//     zero to ~90% in the benchmark rounds.
//   - tool DESCRIPTIONS. They are the contract an agent reads, so each one says
//     when to reach for the tool, not merely what it does.
//
// Transport is stdio (JSON-RPC over stdin/stdout), which is what a local agent
// launches. The tools are thin wrappers over the HTTP API so both surfaces
// cannot drift apart.
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ProtocolVersion is the MCP revision this server speaks.
const ProtocolVersion = "2024-11-05"

// Instructions are returned on initialize. Agents reliably read these; they are
// the difference between a mounted knowledge base and a used one.
const Instructions = "This server is the team's knowledge base and memory: runbooks, " +
	"conventions, ticket decisions, and what previous agents learned. " +
	"Call get_briefing ONCE at the start of any work session (pinned notes, " +
	"onboarding rules, recent agent memories). Before assuming any " +
	"project-specific fact or choosing an approach, check search_notes / " +
	"ask_notes / recall — teams record accepted fixes that are not visible " +
	"in the code. Use remember to persist anything future agents need."

// Server bridges MCP calls to the local HTTP API.
type Server struct {
	BaseURL string
	Agent   string
	Client  *http.Client

	// AuthToken is presented to the API when it is gated by
	// GRIMOIRE_AUTH_TOKEN. This server is an HTTP client of that API, so
	// without it every tool call against a gated instance returns 401.
	AuthToken string
}

func New(baseURL, agent string) *Server {
	if agent == "" {
		agent = "agent"
	}
	return &Server{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Agent:     agent,
		AuthToken: os.Getenv("GRIMOIRE_AUTH_TOKEN"),
		Client:    &http.Client{Timeout: 120 * time.Second},
	}
}

// ---------------------------------------------------------------- JSON-RPC

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve runs the stdio loop until in is exhausted.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // notes can be large
	enc := json.NewEncoder(out)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			continue // a malformed frame is not worth killing the session over
		}
		resp := s.handle(req)
		if resp == nil {
			continue // notification: no id, no reply
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (s *Server) handle(req request) *response {
	if len(req.ID) == 0 {
		return nil // notification
	}
	ok := func(result any) *response {
		return &response{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	switch req.Method {
	case "initialize":
		return ok(map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "grimoire", "version": "1.0.0"},
			"instructions":    Instructions,
		})
	case "tools/list":
		return ok(map[string]any{"tools": Tools()})
	case "tools/call":
		return s.callTool(req, ok)
	case "ping":
		return ok(map[string]any{})
	default:
		return &response{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32601, Message: "unknown method: " + req.Method}}
	}
}

func (s *Server) callTool(req request, ok func(any) *response) *response {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &response{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32602, Message: "invalid params"}}
	}
	result, err := s.dispatch(params.Name, params.Arguments)
	if err != nil {
		// Tool failures are reported as results with isError, not as protocol
		// errors: the agent should see the message and adapt, not have the
		// call look like a transport fault.
		return ok(map[string]any{
			"content": []map[string]string{{"type": "text", "text": err.Error()}},
			"isError": true,
		})
	}
	body, _ := json.MarshalIndent(result, "", "  ")
	return ok(map[string]any{
		"content": []map[string]string{{"type": "text", "text": string(body)}},
	})
}

// ---------------------------------------------------------------- HTTP bridge

func (s *Server) api(method, path string, body any) (any, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.AuthToken)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grimoire unreachable at %s: %w", s.BaseURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	var out any
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw), nil
	}
	return out, nil
}

func str(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// strList reads a list argument, tolerating a single string — several MCP
// clients send one when the list has one element.
func strList(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	}
	return nil
}

func num(args map[string]any, key string, def int) int {
	if v, ok := args[key].(float64); ok {
		return int(v)
	}
	return def
}

func boolean(args map[string]any, key string) bool {
	v, _ := args[key].(bool)
	return v
}

func (s *Server) dispatch(name string, args map[string]any) (any, error) {
	switch name {
	case "kb_info":
		return s.api("GET", "/api/health", nil)
	case "get_briefing":
		return s.api("GET", "/api/briefing", nil)
	case "search_notes":
		q := url.Values{}
		q.Set("q", str(args, "query"))
		return s.api("GET", "/api/search?"+q.Encode(), nil)
	case "ask_notes":
		q := url.Values{}
		q.Set("q", str(args, "question"))
		q.Set("k", fmt.Sprint(num(args, "k", 8)))
		return s.api("GET", "/api/retrieve?"+q.Encode(), nil)
	case "search_web":
		q := url.Values{}
		q.Set("q", str(args, "query"))
		q.Set("n", fmt.Sprint(num(args, "n", 5)))
		return s.api("GET", "/api/web/search?"+q.Encode(), nil)
	case "open_urls":
		body := map[string]any{"urls": strList(args, "urls")}
		if n := num(args, "max_chars", 0); n > 0 {
			body["max_chars"] = n
		}
		return s.api("POST", "/api/web/fetch", body)
	case "read_note":
		return s.api("GET", "/api/notes/"+str(args, "path"), nil)
	case "list_notes":
		q := url.Values{}
		if tag := str(args, "tag"); tag != "" {
			q.Set("tag", tag)
		}
		return s.api("GET", "/api/notes?"+q.Encode(), nil)
	case "create_note":
		body := map[string]any{"title": str(args, "title"), "body": str(args, "body")}
		if tags, ok := args["tags"].([]any); ok {
			body["tags"] = tags
		}
		return s.api("POST", "/api/notes", body)
	case "update_note":
		return s.api("PUT", "/api/notes/"+str(args, "path"),
			map[string]any{"body": str(args, "body")})
	case "append_daily":
		return s.api("POST", "/api/capture", map[string]any{
			"text": str(args, "text"), "source": "mcp"})
	case "backlinks":
		note, err := s.api("GET", "/api/notes/"+str(args, "path"), nil)
		if err != nil {
			return nil, err
		}
		if m, ok := note.(map[string]any); ok {
			return m["backlinks"], nil
		}
		return note, nil
	case "list_tags":
		return s.api("GET", "/api/tags", nil)
	case "get_fact":
		q := url.Values{}
		if k := str(args, "key"); k != "" {
			q.Set("key", k)
		}
		if n := str(args, "note"); n != "" {
			q.Set("note", n)
		}
		return s.api("GET", "/api/facts?"+q.Encode(), nil)
	case "remember":
		// attribution comes from the server's configured identity, not from the
		// caller — an agent must not be able to write memories as someone else
		return s.api("POST", "/api/memory", map[string]any{
			"text": str(args, "text"), "topic": str(args, "topic"),
			"task": str(args, "task"), "agent": s.Agent})
	case "recall":
		q := url.Values{}
		q.Set("q", str(args, "query"))
		q.Set("limit", fmt.Sprint(num(args, "limit", 10)))
		return s.api("GET", "/api/memory?"+q.Encode(), nil)
	case "set_fact":
		return s.api("POST", "/api/facts", map[string]any{
			"note": str(args, "note"), "key": str(args, "key"),
			"value": str(args, "value"),
		})
	case "consolidate_memory":
		// both fields are optional; an empty body means "every memory note"
		body := map[string]any{}
		if t := str(args, "topic"); t != "" {
			body["topic"] = t
		}
		if p := str(args, "path"); p != "" {
			body["path"] = p
		}
		return s.api("POST", "/api/memory/consolidate", body)
	case "list_grants":
		return s.api("GET", "/api/grants", nil)
	case "use_credential":
		return s.api("POST", "/api/secrets/broker", map[string]any{
			"grant": str(args, "grant"), "url": str(args, "url"),
			"method": strOr(str(args, "method"), "GET"),
			"header": strOr(str(args, "header"), "Authorization"),
			"body":   args["body"], "json": boolean(args, "json"),
		})
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func strOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
