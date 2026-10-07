package mcp

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// EnvBank selects the memory bank the bank tools use when a call does not
// name one — so an agent launched for one project writes to that project's
// bank without having to be told its name on every call.
const EnvBank = "GRIMOIRE_BANK"

// bankTools are the memory-bank tools. Their names avoid the per-fact memory
// tools (`recall`, `list_documents`), which keep their meaning.
func bankTools() []tool {
	bankArg := strProp("bank id (defaults to the server's " + EnvBank + ")")
	return []tool{
		{
			Name: "retain",
			Description: "Hand a memory bank raw content — a conversation transcript, a " +
				"document, notes — and let it extract the durable facts, resolve who and what " +
				"they are about, and date them. Use this at the end of a conversation or task " +
				"instead of deciding fact by fact what to remember. Retaining the same " +
				"document_id again updates that document and re-reads only what changed. " +
				"It runs in the background and returns an operation_id (see get_operation).",
			InputSchema: obj(map[string]any{
				"bank":        bankArg,
				"content":     strProp("the content: plain text, or a JSON array of {speaker, text, timestamp} turns"),
				"document_id": strProp("optional stable id for this source (a session id, a file path)"),
				"timestamp":   strProp("optional ISO-8601 time the content was written; relative dates in it resolve against this"),
				"context":     strProp("optional note about the source, e.g. 'support chat with Dana'"),
				"tags":        arrProp("optional tags, for filtering recall"),
				"update_mode": strProp("replace (default) or append to the stored document"),
			}, "content"),
		},
		{
			Name: "bank_recall",
			Description: "Search a memory bank for what is known about something. Combines " +
				"meaning, exact words, the people and things involved, and time — so " +
				"'what did Dana say about the migration last spring?' works. Returns individual " +
				"facts, with what a person wrote ranked above what a model extracted.",
			InputSchema: obj(map[string]any{
				"bank":            bankArg,
				"query":           strProp("what you want to know"),
				"max_tokens":      intProp("budget for returned facts (default 4096)"),
				"budget":          strProp("search breadth: low, mid (default) or high"),
				"types":           arrProp("fact types: world, experience"),
				"tags":            arrProp("only facts with these tags"),
				"query_timestamp": strProp("ISO-8601 'now' for the question; relative dates resolve against it"),
				"include_chunks": map[string]any{"type": "boolean",
					"description": "also return the source passages the facts came from"},
			}, "query"),
		},
		{
			Name:        "list_banks",
			Description: "List the memory banks you can read, with how many facts and documents each holds.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name: "create_bank",
			Description: "Create a memory bank, optionally with a mission (what it is for) and " +
				"a retain mission (what extraction should focus on). Banks are also created " +
				"on first retain, so this is only needed to set them up deliberately.",
			InputSchema: obj(map[string]any{
				"bank":           strProp("new bank id: lowercase letters, digits, . _ : -"),
				"name":           strProp("display name"),
				"mission":        strProp("what this bank is for"),
				"retain_mission": strProp("what extraction should keep, e.g. 'only decisions and their reasons'"),
			}, "bank"),
		},
		{
			Name:        "bank_profile",
			Description: "Read a memory bank's profile: name, mission, retain mission, disposition, directives and settings.",
			InputSchema: obj(map[string]any{"bank": bankArg}),
		},
		{
			Name:        "list_bank_memories",
			Description: "List the facts in a memory bank, optionally filtered by text, type or document.",
			InputSchema: obj(map[string]any{
				"bank":        bankArg,
				"q":           strProp("optional text the fact must contain"),
				"type":        strProp("optional fact type: world or experience"),
				"document_id": strProp("optional: only facts from this document"),
				"limit":       intProp("max facts (default 100)"),
			}),
		},
		{
			Name: "delete_bank_memory",
			Description: "Delete one fact from a memory bank by id. A fact a person wrote or " +
				"corrected is refused unless force is set — ask before forcing.",
			InputSchema: obj(map[string]any{
				"bank": bankArg,
				"id":   strProp("fact id from bank_recall or list_bank_memories"),
				"force": map[string]any{"type": "boolean",
					"description": "also delete a fact a person wrote"},
			}, "id"),
		},
		{
			Name:        "list_entities",
			Description: "List the people, places, organisations and things a memory bank knows about, most mentioned first.",
			InputSchema: obj(map[string]any{
				"bank":  bankArg,
				"q":     strProp("optional name filter"),
				"limit": intProp("max entities (default 100)"),
			}),
		},
		{
			Name:        "list_bank_documents",
			Description: "List the sources retained into a memory bank.",
			InputSchema: obj(map[string]any{
				"bank":  bankArg,
				"limit": intProp("max documents (default 100)"),
			}),
		},
		{
			Name:        "get_bank_document",
			Description: "Read one retained source and the facts extracted from it.",
			InputSchema: obj(map[string]any{
				"bank":        bankArg,
				"document_id": strProp("the document id"),
			}, "document_id"),
		},
		{
			Name: "delete_bank_document",
			Description: "Delete a retained source and the facts a model extracted from it. " +
				"Facts a person wrote or corrected are kept unless force is set.",
			InputSchema: obj(map[string]any{
				"bank":        bankArg,
				"document_id": strProp("the document id"),
				"force": map[string]any{"type": "boolean",
					"description": "also delete facts a person wrote"},
			}, "document_id"),
		},
	}
}

func (s *Server) bankOf(args map[string]any) (string, error) {
	b := strings.TrimSpace(str(args, "bank"))
	if b == "" {
		b = strings.TrimSpace(s.Bank)
	}
	if b == "" {
		b = strings.TrimSpace(os.Getenv(EnvBank))
	}
	if b == "" {
		return "", fmt.Errorf("no bank: pass `bank` or set %s", EnvBank)
	}
	return url.PathEscape(b), nil
}

// dispatchBank handles the bank tools; handled is false for any other name.
func (s *Server) dispatchBank(name string, args map[string]any) (result any, handled bool, err error) {
	switch name {
	case "list_banks":
		r, err := s.api("GET", "/api/banks", nil)
		return r, true, err
	case "create_bank":
		body := map[string]any{"bank_id": str(args, "bank")}
		for _, k := range []string{"name", "mission", "retain_mission"} {
			if v := str(args, k); v != "" {
				body[k] = v
			}
		}
		r, err := s.api("POST", "/api/banks", body)
		return r, true, err
	case "list_bank_templates":
		r, err := s.api("GET", "/api/bank-templates", nil)
		return r, true, err
	default:
		if !bankScoped[name] {
			return nil, false, nil
		}
	}
	b, err := s.bankOf(args)
	if err != nil {
		return nil, true, err
	}
	base := "/api/banks/" + b
	switch name {
	case "retain":
		item := map[string]any{"content": str(args, "content")}
		for _, k := range []string{"document_id", "timestamp", "context", "update_mode"} {
			if v := str(args, k); v != "" {
				item[k] = v
			}
		}
		if tags := strList(args, "tags"); len(tags) > 0 {
			item["tags"] = tags
		}
		if s.Session != "" && item["document_id"] == nil {
			// The launcher's run id is the natural document: everything this
			// run retains accumulates into one source.
			item["document_id"] = s.Session
			item["update_mode"] = "append"
		}
		// Retaining runs in the background: extraction can take a while, and
		// an agent should not hold its turn for it. The answer carries the
		// operation id to poll with get_operation.
		r, err := s.api("POST", base+"/memories", map[string]any{"items": []any{item}, "async": true})
		return r, true, err
	case "bank_recall":
		body := map[string]any{"query": str(args, "query")}
		if n := num(args, "max_tokens", 0); n > 0 {
			body["max_tokens"] = n
		}
		for _, k := range []string{"budget", "query_timestamp"} {
			if v := str(args, k); v != "" {
				body[k] = v
			}
		}
		for _, k := range []string{"types", "tags"} {
			if v := strList(args, k); len(v) > 0 {
				body[k] = v
			}
		}
		if boolean(args, "include_chunks") {
			body["include"] = map[string]any{"chunks": map[string]any{}}
		}
		r, err := s.api("POST", base+"/memories/recall", body)
		return r, true, err
	case "bank_profile":
		r, err := s.api("GET", base, nil)
		return r, true, err
	case "list_bank_memories":
		q := url.Values{}
		for _, k := range []string{"q", "type", "document_id"} {
			if v := str(args, k); v != "" {
				q.Set(k, v)
			}
		}
		if n := num(args, "limit", 0); n > 0 {
			q.Set("limit", fmt.Sprint(n))
		}
		r, err := s.api("GET", base+"/memories?"+q.Encode(), nil)
		return r, true, err
	case "delete_bank_memory":
		path := base + "/memories/" + url.PathEscape(str(args, "id"))
		if boolean(args, "force") {
			path += "?force=true"
		}
		r, err := s.api("DELETE", path, nil)
		return r, true, err
	case "list_entities":
		q := url.Values{}
		if v := str(args, "q"); v != "" {
			q.Set("q", v)
		}
		if n := num(args, "limit", 0); n > 0 {
			q.Set("limit", fmt.Sprint(n))
		}
		r, err := s.api("GET", base+"/entities?"+q.Encode(), nil)
		return r, true, err
	case "list_bank_documents":
		q := url.Values{}
		if n := num(args, "limit", 0); n > 0 {
			q.Set("limit", fmt.Sprint(n))
		}
		r, err := s.api("GET", base+"/documents?"+q.Encode(), nil)
		return r, true, err
	case "get_bank_document":
		r, err := s.api("GET", base+"/documents/"+url.PathEscape(str(args, "document_id")), nil)
		return r, true, err
	case "delete_bank_document":
		path := base + "/documents/" + url.PathEscape(str(args, "document_id"))
		if boolean(args, "force") {
			path += "?force=true"
		}
		r, err := s.api("DELETE", path, nil)
		return r, true, err
	}
	if r, handled, err := s.dispatchBankReasoning(name, base, args); handled {
		return r, true, err
	}
	return nil, false, nil
}
