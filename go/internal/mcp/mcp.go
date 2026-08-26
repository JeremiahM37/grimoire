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

// The environment an agent's MCP client sets when it launches grimoire-mcp.
//
// These are constants rather than string literals because the name is written
// in three places that must agree — the binary that READS it, the `agent-setup`
// command that PRINTS it, and the README that documents it — and for a while
// they did not. `agent-setup` emitted GRIMOIRE_API, which nothing reads. The
// failure is silent and lands on exactly the people who get furthest: the
// default is http://127.0.0.1:9111, so anyone on that port never noticed, and
// anyone on a different port or host got a server that started fine, answered
// every tool call against the wrong address, and looked identical to "no
// knowledge exists". A docs-parity test now checks the README against these.
const (
	EnvURL       = "GRIMOIRE_URL"        // API base the MCP server talks to
	EnvAgentName = "GRIMOIRE_AGENT_NAME" // provenance stamped on what it writes
	EnvMCPToken  = "GRIMOIRE_MCP_TOKEN"  // bearer token the http transport demands
)

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

	// InboundToken gates the HTTP transport — the opposite direction from
	// AuthToken, which this server PRESENTS. This one it DEMANDS.
	//
	// It exists because a hosted agent cannot speak stdio. Reaching Claude.ai,
	// ChatGPT or any other cloud client means this transport is exposed, and
	// the thing being exposed is not a read-only search box: the same mount
	// carries `remember`, `create_note` and the credential broker. Unguarded,
	// publishing it hands anyone who finds the URL the vault and the ability to
	// spend its secrets.
	//
	// ListenAndServe refuses to bind a non-loopback address without it, so the
	// unsafe configuration is unreachable rather than merely discouraged.
	InboundToken string

	// AdminToken is presented to the administrative surface when that is
	// gated separately — list_grants and the credential console's own state
	// live there.
	AdminToken string
}

func New(baseURL, agent string) *Server {
	if agent == "" {
		agent = "agent"
	}
	return &Server{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Agent:     agent,
		AuthToken: os.Getenv("GRIMOIRE_AUTH_TOKEN"),
		// The administrative surface can be gated separately, and some tools
		// here are on it — list_grants reads the credential console's own
		// state. Without this an agent on a gated instance would find those
		// tools failing with 401 and nothing to point at.
		AdminToken:   os.Getenv("GRIMOIRE_ADMIN_TOKEN"),
		InboundToken: os.Getenv(EnvMCPToken),
		Client:       &http.Client{Timeout: 120 * time.Second},
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
	if s.AdminToken != "" {
		req.Header.Set("X-Grimoire-Admin", s.AdminToken)
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
		if boolean(args, "trusted_only") {
			q.Set("trusted", "1")
		}
		return s.api("GET", "/api/search?"+q.Encode(), nil)
	case "ask_notes":
		q := url.Values{}
		q.Set("q", str(args, "question"))
		q.Set("k", fmt.Sprint(num(args, "k", 8)))
		if boolean(args, "trusted_only") {
			q.Set("trusted", "1")
		}
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
		body := map[string]any{
			"text": str(args, "text"), "topic": str(args, "topic"),
			"task": str(args, "task"), "agent": s.Agent}
		for _, k := range []string{"session", "category", "expires_in", "origin"} {
			if v := str(args, k); v != "" {
				body[k] = v
			}
		}
		if boolean(args, "immutable") {
			body["immutable"] = true
		}
		return s.api("POST", "/api/memory", body)
	case "recall":
		q := url.Values{}
		q.Set("q", str(args, "query"))
		q.Set("limit", fmt.Sprint(num(args, "limit", 10)))
		for _, k := range []string{"agent", "session", "category"} {
			if v := str(args, k); v != "" {
				q.Set(k, v)
			}
		}
		for _, k := range []string{"include_superseded", "explain"} {
			if boolean(args, k) {
				q.Set(k, "1")
			}
		}
		return s.api("GET", "/api/memory?"+q.Encode(), nil)
	case "stale_notes":
		q := url.Values{}
		if n := num(args, "days", 0); n > 0 {
			q.Set("days", fmt.Sprint(n))
		}
		if n := num(args, "limit", 0); n > 0 {
			q.Set("limit", fmt.Sprint(n))
		}
		return s.api("GET", "/api/stale?"+q.Encode(), nil)
	case "memory_changes":
		q := url.Values{}
		if v := str(args, "since"); v != "" {
			q.Set("since", v)
		}
		if v := str(args, "agent"); v != "" {
			q.Set("agent", v)
		}
		if n := num(args, "limit", 0); n > 0 {
			q.Set("limit", fmt.Sprint(n))
		}
		return s.api("GET", "/api/memory/changes?"+q.Encode(), nil)
	case "forget":
		q := url.Values{}
		q.Set("path", str(args, "path"))
		q.Set("id", str(args, "id"))
		// The retraction is attributed to the agent making it, so the note
		// records who stopped believing what.
		q.Set("agent", s.Agent)
		return s.api("DELETE", "/api/memory/entry?"+q.Encode(), nil)
	case "memory_graph":
		q := url.Values{}
		if e := str(args, "entity"); e != "" {
			q.Set("entity", e)
		}
		q.Set("depth", fmt.Sprint(num(args, "depth", 1)))
		q.Set("limit", fmt.Sprint(num(args, "limit", 50)))
		return s.api("GET", "/api/memory/graph?"+q.Encode(), nil)
	case "memory_feedback":
		return s.api("POST", "/api/memory/feedback", map[string]any{
			"path": str(args, "path"), "id": str(args, "id"),
			"helpful": boolean(args, "helpful")})
	case "memory_scopes":
		return s.api("GET", "/api/memory/facets", nil)
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
	case "request_credential":
		// The grantee is the server's configured identity, never the caller's
		// claim — exactly as `remember` attributes a memory. An agent that
		// could name its own grantee could ask as somebody else, and then
		// collect a token issued to them.
		body := map[string]any{
			"secret": str(args, "secret"), "grantee": s.Agent,
			"scope": str(args, "scope"), "reason": str(args, "reason"),
		}
		if n := num(args, "ttl_seconds", 0); n > 0 {
			body["ttl_seconds"] = n
		}
		return s.api("POST", "/api/secrets/requests", body)
	case "check_credential_request":
		q := url.Values{}
		q.Set("grantee", s.Agent)
		return s.api("GET", "/api/secrets/requests/"+url.PathEscape(str(args, "id"))+
			"?"+q.Encode(), nil)
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
