package mcp

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The memory-bank tools beyond retain and recall: reflect, observations,
// mental models, directives, operations and templates. Like the rest, each
// is a thin call to the HTTP API.

func bankReasoningTools() []tool {
	bankArg := strProp("bank id (defaults to the server's " + EnvBank + ")")
	boolProp := func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
	return []tool{
		{
			Name: "reflect",
			Description: "Ask a memory bank a question that needs reasoning, not just lookup: it searches " +
				"the bank's mental models, observations and facts in that order, reasons over what it " +
				"finds and answers with the memories it rests on. Prefer this to bank_recall when you " +
				"want an answer rather than raw facts. What a person wrote outranks what a model inferred.",
			InputSchema: obj(map[string]any{
				"bank":                 bankArg,
				"query":                strProp("the question"),
				"budget":               strProp("how hard to look: low (default), mid or high"),
				"max_tokens":           intProp("target answer length in tokens (default 4096)"),
				"context":              strProp("optional extra context for the question"),
				"tags":                 arrProp("only memories with these tags"),
				"apply_all_directives": boolProp("apply every directive, not only those the tags reach"),
				"response_schema": map[string]any{"type": "object",
					"description": "optional JSON Schema (type object) for a structured answer"},
				"include_trace": boolProp("also return the retrieval steps taken"),
			}, "query"),
		},
		{
			Name: "consolidate",
			Description: "Ask a memory bank to fold its newly retained facts into observations now " +
				"(it also does this on its own after a retain when configured to). Runs in the " +
				"background; returns an operation_id. Needs a language model.",
			InputSchema: obj(map[string]any{"bank": bankArg}),
		},
		{
			Name: "list_observations",
			Description: "List a memory bank's observations — durable statements consolidated from many " +
				"facts, each with how many facts support it and whether a person wrote it.",
			InputSchema: obj(map[string]any{
				"bank":            bankArg,
				"q":               strProp("optional text the observation must contain"),
				"limit":           intProp("max observations (default 100)"),
				"include_history": boolProp("also return superseded versions"),
			}),
		},
		{
			Name: "update_observation",
			Description: "Edit the text of a memory bank's observation. The edit makes it a person's: " +
				"consolidation will never revise or retire it afterwards, only file a challenge beside it.",
			InputSchema: obj(map[string]any{"bank": bankArg, "id": strProp("observation id"),
				"text": strProp("the new wording")}, "id", "text"),
		},
		{
			Name:        "get_bank_memory",
			Description: "Read one fact or observation of a memory bank by id.",
			InputSchema: obj(map[string]any{"bank": bankArg, "id": strProp("memory id")}, "id"),
		},
		{
			Name: "list_mental_models",
			Description: "List a memory bank's mental models: standing questions whose answers the bank " +
				"keeps up to date (folders in the id form a tree of knowledge pages).",
			InputSchema: obj(map[string]any{"bank": bankArg, "tags": arrProp("only models with these tags")}),
		},
		{
			Name:        "get_mental_model",
			Description: "Read a mental model's current answer, whether it is stale, and any proposal waiting for review.",
			InputSchema: obj(map[string]any{"bank": bankArg, "id": strProp("model id, e.g. people/dana")}, "id"),
		},
		{
			Name: "create_mental_model",
			Description: "Create a mental model: a question the bank should keep a written answer to " +
				"(e.g. 'what are Dana's working preferences?'). Its first answer is written in the background.",
			InputSchema: obj(map[string]any{
				"bank":         bankArg,
				"name":         strProp("display name"),
				"question":     strProp("the standing question"),
				"id":           strProp("optional id; a/b/c puts it in folders"),
				"tags":         arrProp("optional tags scoping which memories it draws on"),
				"refresh":      strProp("auto (default: refreshed after consolidation) or manual"),
				"refresh_mode": strProp("full (default: rewrite the answer) or delta (edit only the sections new facts touch)"),
				"max_tokens":   intProp("answer length target (default 2048)"),
			}, "question"),
		},
		{
			Name:        "update_mental_model",
			Description: "Change a mental model's question, name, tags or refresh setting. Its answer is not touched.",
			InputSchema: obj(map[string]any{
				"bank": bankArg, "id": strProp("model id"), "name": strProp("new name"),
				"question": strProp("new question"), "tags": arrProp("new tags"), "refresh": strProp("auto or manual"),
			}, "id"),
		},
		{
			Name:        "delete_mental_model",
			Description: "Delete a mental model and any proposal waiting on it. Its earlier versions stay in history; ask the user first.",
			InputSchema: obj(map[string]any{"bank": bankArg, "id": strProp("model id")}, "id"),
		},
		{
			Name: "refresh_mental_model",
			Description: "Rewrite a mental model's answer from what the bank knows now. Runs in the background. " +
				"If a person edited the answer, the new one waits as a proposal instead of replacing theirs.",
			InputSchema: obj(map[string]any{"bank": bankArg, "id": strProp("model id"),
				"mode": strProp("optional: delta edits only the sections new facts touch (a person's sections are never changed); full rewrites. Default is the model's own setting.")}, "id"),
		},
		{
			Name:        "list_directives",
			Description: "List a memory bank's directives: standing rules every reflect answer must follow.",
			InputSchema: obj(map[string]any{"bank": bankArg}),
		},
		{
			Name:        "create_directive",
			Description: "Add a directive to a memory bank, e.g. 'never quote salaries'. Ask the user before adding one.",
			InputSchema: obj(map[string]any{
				"bank": bankArg, "text": strProp("the rule"), "name": strProp("optional short name"),
				"tags": arrProp("optional: apply only when a reflect is scoped to these tags"),
			}, "text"),
		},
		{
			Name:        "delete_directive",
			Description: "Remove a directive from a memory bank, so reflect answers stop following it. Ask the user first.",
			InputSchema: obj(map[string]any{"bank": bankArg, "id": strProp("directive id")}, "id"),
		},
		{
			Name:        "list_operations",
			Description: "List a memory bank's background operations (retains, consolidations, refreshes) and their status.",
			InputSchema: obj(map[string]any{"bank": bankArg, "status": strProp("optional status filter"),
				"type": strProp("optional: retain, consolidation or refresh_mental_model"), "limit": intProp("max (default 20)")}),
		},
		{
			Name:        "get_operation",
			Description: "Check one background operation: queued, running, completed (with its result), failed or cancelled.",
			InputSchema: obj(map[string]any{"bank": bankArg, "operation_id": strProp("operation id")}, "operation_id"),
		},
		{
			Name:        "cancel_operation",
			Description: "Cancel a queued or running background operation.",
			InputSchema: obj(map[string]any{"bank": bankArg, "operation_id": strProp("operation id")}, "operation_id"),
		},
		{
			Name:        "list_bank_templates",
			Description: "List the built-in memory-bank templates (assistant, coding-agent, support, research, plain-retrieval).",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name: "import_bank_template",
			Description: "Set a memory bank up from a built-in template: mission, settings, mental models and " +
				"directives. Additive — nothing already there is removed. Use dry_run to see what it would change.",
			InputSchema: obj(map[string]any{"bank": bankArg, "template": strProp("template id"),
				"dry_run": boolProp("report what would change without changing it")}, "template"),
		},
	}
}

// bankScoped are the tools that act on one bank and take the bank argument.
var bankScoped = map[string]bool{
	"retain": true, "bank_recall": true, "bank_index": true, "bank_duplicates": true, "bank_merge_duplicates": true, "bank_timeline": true, "bank_get": true, "bank_profile": true, "list_bank_memories": true,
	"delete_bank_memory": true, "list_entities": true, "list_bank_documents": true, "get_bank_document": true,
	"delete_bank_document": true, "reflect": true, "consolidate": true, "list_observations": true, "update_observation": true,
	"get_bank_memory": true, "list_mental_models": true, "get_mental_model": true, "create_mental_model": true,
	"update_mental_model": true, "delete_mental_model": true, "refresh_mental_model": true,
	"list_directives": true, "create_directive": true, "delete_directive": true, "list_operations": true,
	"get_operation": true, "cancel_operation": true, "import_bank_template": true,
}

// multiBankOnly are hidden from a single-bank endpoint: there is no other
// bank to list or create there.
var multiBankOnly = map[string]bool{"list_banks": true, "create_bank": true}

func modelPath(base, id string) string { return base + "/mental-models/" + url.PathEscape(id) }

func (s *Server) dispatchBankReasoning(name, base string, args map[string]any) (any, bool, error) {
	switch name {
	case "reflect":
		body := map[string]any{"query": str(args, "query")}
		for _, k := range []string{"budget", "context"} {
			if v := str(args, k); v != "" {
				body[k] = v
			}
		}
		if n := num(args, "max_tokens", 0); n > 0 {
			body["max_tokens"] = n
		}
		if tags := strList(args, "tags"); len(tags) > 0 {
			body["tags"] = tags
		}
		if boolean(args, "apply_all_directives") {
			body["apply_all_directives"] = true
		}
		if sc, ok := args["response_schema"].(map[string]any); ok {
			body["response_schema"] = sc
		}
		if boolean(args, "include_trace") {
			body["include"] = map[string]any{"tool_calls": map[string]any{"output": false}}
		}
		r, err := s.api("POST", base+"/reflect", body)
		return r, true, err
	case "consolidate":
		r, err := s.api("POST", base+"/consolidate", map[string]any{})
		return r, true, err
	case "list_observations":
		q := url.Values{}
		if v := str(args, "q"); v != "" {
			q.Set("q", v)
		}
		if n := num(args, "limit", 0); n > 0 {
			q.Set("limit", fmt.Sprint(n))
		}
		if boolean(args, "include_history") {
			q.Set("include_history", "1")
		}
		r, err := s.api("GET", base+"/observations?"+q.Encode(), nil)
		return r, true, err
	case "update_observation":
		r, err := s.api("PATCH", base+"/observations/"+url.PathEscape(str(args, "id")), map[string]any{"text": str(args, "text")})
		return r, true, err
	case "get_bank_memory":
		r, err := s.api("GET", base+"/memories/"+url.PathEscape(str(args, "id")), nil)
		return r, true, err
	case "list_mental_models":
		q := url.Values{}
		if tags := strList(args, "tags"); len(tags) > 0 {
			q.Set("tags", strings.Join(tags, ","))
		}
		r, err := s.api("GET", base+"/mental-models?"+q.Encode(), nil)
		return r, true, err
	case "get_mental_model":
		r, err := s.api("GET", modelPath(base, str(args, "id")), nil)
		return r, true, err
	case "create_mental_model", "update_mental_model":
		body := map[string]any{}
		for _, k := range []string{"name", "question", "refresh", "refresh_mode", "id"} {
			if v := str(args, k); v != "" {
				body[k] = v
			}
		}
		if n := num(args, "max_tokens", 0); n > 0 {
			body["max_tokens"] = n
		}
		if _, ok := args["tags"]; ok {
			body["tags"] = strList(args, "tags")
		}
		if name == "create_mental_model" {
			r, err := s.api("POST", base+"/mental-models", body)
			return r, true, err
		}
		delete(body, "id")
		r, err := s.api("PATCH", modelPath(base, str(args, "id")), body)
		return r, true, err
	case "delete_mental_model":
		r, err := s.api("DELETE", modelPath(base, str(args, "id")), nil)
		return r, true, err
	case "refresh_mental_model":
		body := map[string]any{}
		if v := str(args, "mode"); v != "" {
			body["mode"] = v
		}
		r, err := s.api("POST", modelPath(base, str(args, "id"))+"/refresh", body)
		return r, true, err
	case "list_directives":
		r, err := s.api("GET", base+"/directives", nil)
		return r, true, err
	case "create_directive":
		body := map[string]any{"text": str(args, "text")}
		if v := str(args, "name"); v != "" {
			body["name"] = v
		}
		if tags := strList(args, "tags"); len(tags) > 0 {
			body["tags"] = tags
		}
		r, err := s.api("POST", base+"/directives", body)
		return r, true, err
	case "delete_directive":
		r, err := s.api("DELETE", base+"/directives/"+url.PathEscape(str(args, "id")), nil)
		return r, true, err
	case "list_operations":
		q := url.Values{}
		for _, k := range []string{"status", "type"} {
			if v := str(args, k); v != "" {
				q.Set(k, v)
			}
		}
		q.Set("limit", fmt.Sprint(num(args, "limit", 20)))
		r, err := s.api("GET", base+"/operations?"+q.Encode(), nil)
		return r, true, err
	case "get_operation":
		r, err := s.api("GET", base+"/operations/"+url.PathEscape(str(args, "operation_id")), nil)
		return r, true, err
	case "cancel_operation":
		r, err := s.api("DELETE", base+"/operations/"+url.PathEscape(str(args, "operation_id")), nil)
		return r, true, err
	case "import_bank_template":
		path := base + "/import"
		if boolean(args, "dry_run") {
			path += "?dry_run=1"
		}
		r, err := s.api("POST", path, map[string]any{"template": str(args, "template")})
		return r, true, err
	}
	return nil, false, nil
}

// ------------------------------------------------------------ per-bank MCP

// callCtx is what an HTTP request says about the bank it works on: the
// /mcp/{bank} path (single-bank: the bank argument is hidden and fixed) or
// the X-Bank-Id header on /mcp (a default the argument may override).
type callCtx struct {
	bank   string
	single bool
}

var mcpBankRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,63}$`)

func validBankID(id string) bool {
	return mcpBankRE.MatchString(id) && !strings.Contains(id, "..") && !strings.Contains(id, "__")
}

// singleBankTools is the tool list for a single-bank endpoint: no
// multi-bank tools, and no bank argument on the rest.
func singleBankTools(all []tool) []tool {
	out := make([]tool, 0, len(all))
	for _, t := range all {
		if multiBankOnly[t.Name] {
			continue
		}
		if bankScoped[t.Name] {
			t.InputSchema = withoutBankArg(t.InputSchema)
		}
		out = append(out, t)
	}
	return out
}

func withoutBankArg(schema map[string]any) map[string]any {
	cp := map[string]any{}
	for k, v := range schema {
		cp[k] = v
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		np := map[string]any{}
		for k, v := range props {
			if k != "bank" {
				np[k] = v
			}
		}
		cp["properties"] = np
	}
	return cp
}

// allowlist is a bank's mcp_tools setting, cached briefly: every tools/list
// and every call consults it, and it changes rarely.
type allowCache struct {
	mu   sync.Mutex
	at   map[string]time.Time
	list map[string]map[string]bool
}

const allowTTL = 5 * time.Second

func (s *Server) bankAllowlist(bank string) map[string]bool {
	s.allowOnce.Do(func() { s.allow = &allowCache{at: map[string]time.Time{}, list: map[string]map[string]bool{}} })
	c := s.allow
	c.mu.Lock()
	if t, ok := c.at[bank]; ok && time.Since(t) < allowTTL {
		l := c.list[bank]
		c.mu.Unlock()
		return l
	}
	c.mu.Unlock()
	var set map[string]bool
	if r, err := s.api("GET", "/api/banks/"+url.PathEscape(bank), nil); err == nil {
		if m, ok := r.(map[string]any); ok {
			if cfg, ok := m["config"].(map[string]any); ok {
				if raw, _ := cfg["mcp_tools"].(string); strings.TrimSpace(raw) != "" {
					set = map[string]bool{}
					for _, n := range strings.Split(raw, ",") {
						if n = strings.TrimSpace(n); n != "" {
							set[n] = true
						}
					}
				}
			}
		}
	}
	c.mu.Lock()
	c.at[bank], c.list[bank] = time.Now(), set
	c.mu.Unlock()
	return set
}

// toolsFor is the tool list a request sees.
func (s *Server) toolsFor(rc callCtx) []tool {
	all := Tools()
	if rc.single {
		all = singleBankTools(all)
	}
	if rc.bank == "" {
		return all
	}
	allow := s.bankAllowlist(rc.bank)
	if allow == nil {
		return all
	}
	out := make([]tool, 0, len(all))
	for _, t := range all {
		// The allowlist governs what an agent may do to the bank. On a
		// single-bank endpoint that is every tool; on /mcp with a default
		// bank it is the bank tools.
		if allow[t.Name] || (!rc.single && !bankScoped[t.Name]) {
			out = append(out, t)
		}
	}
	return out
}

// prepareCall applies a request's bank to a tool call's arguments, and
// refuses a tool the endpoint or the bank does not offer.
func (s *Server) prepareCall(rc callCtx, name string, args map[string]any) (map[string]any, error) {
	if args == nil {
		args = map[string]any{}
	}
	if rc.single && multiBankOnly[name] {
		return nil, fmt.Errorf("tool %q is not available on a single-bank endpoint", name)
	}
	bank := rc.bank
	if bankScoped[name] {
		if rc.single {
			args["bank"] = rc.bank
		} else if b := strings.TrimSpace(str(args, "bank")); b != "" {
			bank = b
		} else if rc.bank != "" {
			args["bank"] = rc.bank
		}
	}
	if bank != "" && (rc.single || bankScoped[name]) {
		if allow := s.bankAllowlist(bank); allow != nil && !allow[name] {
			return nil, fmt.Errorf("tool %q is not enabled for bank %q", name, bank)
		}
	}
	return args, nil
}
