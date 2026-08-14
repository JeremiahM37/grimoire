package mcp

// Tool descriptions ARE the contract an agent reads before deciding whether to
// call anything. Each one says WHEN to reach for the tool, not merely what it
// does — a description that only restates the name gets the tool ignored.

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type": "object", "properties": props, "required": required,
	}
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// Tools returns the advertised tool list.
func Tools() []tool {
	return []tool{
		{
			Name: "get_briefing",
			Description: "START HERE when beginning work: the team's standing context in one " +
				"call — pinned notes, onboarding rules (environment requirements, required " +
				"steps), and the most recent agent memories. Cheap; call it before your first edit.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name: "kb_info",
			Description: "Connectivity and scope check: confirms the knowledge base is " +
				"reachable and reports note/tag counts. Use it to verify your mount — a " +
				"silent MCP failure otherwise looks identical to 'no knowledge exists'.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name: "search_notes",
			Description: "Full-text search across the knowledge base. Consult this BEFORE " +
				"assuming any project-specific fact: teams record accepted fixes, " +
				"conventions and decisions that are not visible in the code.",
			InputSchema: obj(map[string]any{
				"query": strProp("search terms"),
				"limit": intProp("max results (default 20)"),
			}, "query"),
		},
		{
			Name: "ask_notes",
			Description: "Semantic retrieval over the knowledge base — use when you have a " +
				"question rather than keywords, or when search_notes returns nothing " +
				"useful because the wording differs from how it was written down.",
			InputSchema: obj(map[string]any{
				"question": strProp("a natural-language question"),
				"k":        intProp("passages to return (default 8)"),
			}, "question"),
		},
		{
			Name:        "read_note",
			Description: "Read one note in full by path, including its links and backlinks.",
			InputSchema: obj(map[string]any{
				"path": strProp("vault-relative path, e.g. 'Projects/Roadmap.md'"),
			}, "path"),
		},
		{
			Name:        "list_notes",
			Description: "List notes, most recently updated first. Optionally filter by tag.",
			InputSchema: obj(map[string]any{"tag": strProp("optional tag filter")}),
		},
		{
			Name:        "create_note",
			Description: "Create a new note. Use for durable documentation; use `remember` for things you learned while working.",
			InputSchema: obj(map[string]any{
				"title": strProp("note title"),
				"body":  strProp("markdown body"),
				"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			}, "title"),
		},
		{
			Name:        "update_note",
			Description: "Replace a note's body. The previous version is snapshotted automatically, so the human can roll back.",
			InputSchema: obj(map[string]any{
				"path": strProp("vault-relative path"),
				"body": strProp("new markdown body"),
			}, "path", "body"),
		},
		{
			Name:        "append_daily",
			Description: "Append a line to today's inbox and daily note — the low-friction way to record something without choosing a location.",
			InputSchema: obj(map[string]any{"text": strProp("what to record")}, "text"),
		},
		{
			Name:        "backlinks",
			Description: "Which notes link TO this one. Use to find the context a note is discussed in.",
			InputSchema: obj(map[string]any{"path": strProp("vault-relative path")}, "path"),
		},
		{
			Name:        "list_tags",
			Description: "Every tag with its note count — a cheap map of what the knowledge base covers.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name: "get_fact",
			Description: "Look up structured `key:: value` facts declared in notes. " +
				"Deterministic: prefer this over retrieval when you need an exact value " +
				"(a port, an owner, a URL) rather than prose.",
			InputSchema: obj(map[string]any{
				"key":  strProp("fact key, e.g. 'port'"),
				"note": strProp("optional: restrict to one note"),
			}),
		},
		{
			Name: "remember",
			Description: "Persist something future agents will need — a root cause, a " +
				"convention you had to discover, a gotcha. Memories are ordinary notes the " +
				"human can read, edit and roll back. Use `topic` to group related memories.",
			InputSchema: obj(map[string]any{
				"text":  strProp("what to remember"),
				"topic": strProp("optional grouping, e.g. 'deploy'"),
				"task":  strProp("optional origin: ticket id, session, url"),
			}, "text"),
		},
		{
			Name: "recall",
			Description: "Search what previous agents recorded. Check this before " +
				"re-deriving anything that smells like it was learned the hard way.",
			InputSchema: obj(map[string]any{
				"query": strProp("what you are trying to remember"),
				"limit": intProp("max memories (default 10)"),
			}),
		},
		{
			Name: "list_grants",
			Description: "Active credential grants (grantee, scope, expiry — never values). " +
				"Errors with 423 while the human has the vault locked.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name: "use_credential",
			Description: "Make an authenticated request USING a credential without ever " +
				"seeing it: the value is injected server-side into the header you choose. " +
				"The secret never enters your context.",
			InputSchema: obj(map[string]any{
				"grant":  strProp("grant token"),
				"url":    strProp("target url"),
				"method": strProp("http method (default GET)"),
				"header": strProp("header to inject into (default Authorization)"),
				"json":   map[string]any{"type": "boolean", "description": "parse the response as json"},
			}, "grant", "url"),
		},
	}
}
