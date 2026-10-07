package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// The bank surfaces whose server side is still being built: reflect,
// observations, mental models, operations and templates.
//
// Every call to them is in this file so the route shapes are in one place to
// align when the server lands. Each one turns "this server has no such route"
// into errNotAvailable rather than a raw 404, so an older server reads as
// "not here yet", never as "your bank is missing".

func notYet(err error, what string) error {
	if routeMissing(err) {
		return fmt.Errorf("%s: %w", what, errNotAvailable)
	}
	return err
}

// ---- reflect ------------------------------------------------------------------

func bankReflect(c *bankClient, f *bankFlags) error {
	bank, err := f.need(0, "BANK")
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(f.pos[1:], " "))
	if query == "" {
		query = strings.TrimSpace(stdinOrArgs(nil))
	}
	if query == "" {
		return fmt.Errorf("missing QUERY")
	}
	body := map[string]any{"query": query, "budget": f.str("--budget", "low"),
		"include": map[string]any{"facts": map[string]any{}}}
	if _, ok := f.get("--max-tokens"); ok {
		n, err := f.int("--max-tokens", 4096)
		if err != nil {
			return err
		}
		body["max_tokens"] = n
	}
	if v, ok := f.get("--context"); ok {
		body["context"] = v
	}
	if t := f.list("--tags"); t != nil {
		body["tags"] = t
		body["tags_match"] = f.str("--tags-match", "any")
	}
	if path, ok := f.get("--schema"); ok {
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var schema map[string]any
		if err := json.Unmarshal(raw, &schema); err != nil {
			return fmt.Errorf("--schema: %v", err)
		}
		body["response_schema"] = schema
	}
	if f.on["--trace"] {
		body["include"].(map[string]any)["tool_calls"] = map[string]any{}
	}
	var out struct {
		Text    string `json:"text"`
		BasedOn struct {
			Memories     []recallFact `json:"memories"`
			Observations []struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			} `json:"observations"`
			MentalModels []struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			} `json:"mental_models"`
		} `json:"based_on"`
		Structured json.RawMessage `json:"structured_output"`
		Usage      struct {
			Total int `json:"total_tokens"`
		} `json:"usage"`
	}
	var raw json.RawMessage
	if err := c.do("POST", bankPath(bank, "reflect"), body, &raw); err != nil {
		return notYet(err, "reflect")
	}
	if f.on["--json"] {
		fmt.Println(string(raw))
		return nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(out.Text))
	if len(out.Structured) > 0 && string(out.Structured) != "null" {
		fmt.Printf("\nstructured: %s\n", out.Structured)
	}
	cites := len(out.BasedOn.Memories) + len(out.BasedOn.Observations) + len(out.BasedOn.MentalModels)
	if cites > 0 {
		fmt.Println("\nbased on:")
		for _, m := range out.BasedOn.MentalModels {
			fmt.Printf("  [model %s] %s\n", m.ID, firstLine(m.Text))
		}
		for _, o := range out.BasedOn.Observations {
			fmt.Printf("  [observation %s] %s\n", o.ID, firstLine(o.Text))
		}
		for _, m := range out.BasedOn.Memories {
			fmt.Printf("  [%s %s] %s\n", m.Type, m.ID, firstLine(m.Text))
		}
	}
	if out.Usage.Total > 0 {
		fmt.Printf("model tokens: %d\n", out.Usage.Total)
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 160 {
		s = s[:157] + "…"
	}
	return s
}

// ---- observations -------------------------------------------------------------

func bankObservations(c *bankClient, f *bankFlags) error {
	bank, err := f.need(0, "BANK")
	if err != nil {
		return err
	}
	if len(f.pos) > 1 && f.pos[1] == "consolidate" {
		var out struct {
			OperationID string `json:"operation_id"`
		}
		if err := c.do("POST", bankPath(bank, "consolidate"), map[string]any{}, &out); err != nil {
			return notYet(err, "consolidation")
		}
		fmt.Printf("consolidation queued: operation %s\n", out.OperationID)
		return nil
	}
	path := bankPath(bank, "observations")
	if q, ok := f.get("--q"); ok {
		path += "?q=" + url.QueryEscape(q)
	}
	var out struct {
		Items []struct {
			ID         string   `json:"id"`
			Text       string   `json:"text"`
			Evidence   []string `json:"evidence"`
			Proof      int      `json:"proof_count"`
			Authority  string   `json:"authority"`
			DisputedBy string   `json:"disputed_by"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := c.do("GET", path, nil, &out); err != nil {
		return notYet(err, "observations")
	}
	if f.on["--json"] {
		printJSON(out)
		return nil
	}
	for _, o := range out.Items {
		mark := ""
		if o.Authority == "human" {
			mark = " (yours)"
		}
		if o.DisputedBy != "" {
			mark += " (disputed by " + o.DisputedBy + ")"
		}
		fmt.Printf("- %s%s\n    %s · proof %d · %d evidence\n", o.Text, mark, o.ID, o.Proof, len(o.Evidence))
	}
	fmt.Printf("%d observation(s)\n", out.Total)
	return nil
}

// ---- mental models ------------------------------------------------------------

type mentalModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	SourceQuery   string   `json:"source_query"`
	Content       string   `json:"content"`
	Tags          []string `json:"tags"`
	LastRefreshed string   `json:"last_refreshed_at"`
	IsStale       bool     `json:"is_stale"`
	Authority     string   `json:"authority"`
	Pending       *struct {
		Content   string `json:"content"`
		CreatedAt string `json:"created_at"`
	} `json:"pending_proposal"`
}

func modelPath(bank, id string, rest ...string) string {
	return bankPath(bank, append([]string{"mental-models", url.PathEscape(id)}, rest...)...)
}

func createModel(c *bankClient, bank string, m templateModel) error {
	body := map[string]any{"name": m.Name, "source_query": m.SourceQuery, "tags": m.Tags}
	if m.ID != "" {
		body["id"] = m.ID
	}
	if m.MaxTokens > 0 {
		body["max_tokens"] = m.MaxTokens
	}
	if m.Trigger != nil {
		body["trigger"] = m.Trigger
	}
	if body["tags"] == nil {
		body["tags"] = []string{}
	}
	return c.do("POST", bankPath(bank, "mental-models"), body, nil)
}

func bankModels(c *bankClient, f *bankFlags) error {
	sub, err := f.need(0, "ls|show|create|refresh")
	if err != nil {
		return err
	}
	bank, err := f.need(1, "BANK")
	if err != nil {
		return err
	}
	switch sub {
	case "ls", "list":
		var out struct {
			Items []mentalModel `json:"items"`
		}
		if err := c.do("GET", bankPath(bank, "mental-models"), nil, &out); err != nil {
			return notYet(err, "mental models")
		}
		if f.on["--json"] {
			printJSON(out)
			return nil
		}
		for _, m := range out.Items {
			state := "refreshed " + orDash(m.LastRefreshed)
			if m.IsStale {
				state += " · stale"
			}
			if m.Pending != nil {
				state += " · refresh proposal waiting for you"
			}
			fmt.Printf("%-32s %s\n    %s · %s\n", m.ID, m.Name, firstLine(m.SourceQuery), state)
		}
		return nil
	case "show":
		id, err := f.need(2, "model ID")
		if err != nil {
			return err
		}
		var m mentalModel
		if err := c.do("GET", modelPath(bank, id), nil, &m); err != nil {
			return notYet(err, "mental models")
		}
		if f.on["--json"] {
			printJSON(m)
			return nil
		}
		fmt.Printf("%s — %s\nquestion: %s\nrefreshed: %s\n\n%s\n", m.ID, m.Name, m.SourceQuery, orDash(m.LastRefreshed), m.Content)
		if m.Pending != nil {
			fmt.Printf("\n--- proposed refresh (%s), not applied because a person edited this model:\n%s\n", m.Pending.CreatedAt, m.Pending.Content)
		}
		return nil
	case "create":
		name := strings.TrimSpace(strings.Join(f.pos[2:], " "))
		query := f.str("--query", "")
		if name == "" || query == "" {
			return fmt.Errorf("usage: grimoire bank models create BANK NAME --query QUESTION [--id ID] [--tags a,b]")
		}
		var out struct {
			ID          string `json:"mental_model_id"`
			OperationID string `json:"operation_id"`
		}
		body := map[string]any{"name": name, "source_query": query, "tags": f.list("--tags")}
		if body["tags"] == nil {
			body["tags"] = []string{}
		}
		if id, ok := f.get("--id"); ok {
			body["id"] = id
		}
		if err := c.do("POST", bankPath(bank, "mental-models"), body, &out); err != nil {
			return notYet(err, "mental models")
		}
		fmt.Printf("created mental model %s (operation %s)\n", out.ID, orDash(out.OperationID))
		return nil
	case "refresh":
		id, err := f.need(2, "model ID")
		if err != nil {
			return err
		}
		var out struct {
			OperationID string `json:"operation_id"`
			Status      string `json:"status"`
		}
		if err := c.do("POST", modelPath(bank, id, "refresh"), map[string]any{}, &out); err != nil {
			return notYet(err, "mental models")
		}
		fmt.Printf("refresh %s: operation %s\n", orDash(out.Status), orDash(out.OperationID))
		return nil
	}
	return fmt.Errorf("models takes ls, show, create or refresh")
}

// ---- operations ---------------------------------------------------------------

type operation struct {
	ID       string `json:"id"`
	OpID     string `json:"operation_id"`
	Type     string `json:"operation_type"`
	Status   string `json:"status"`
	Created  string `json:"created_at"`
	Updated  string `json:"updated_at"`
	Error    string `json:"error_message"`
	Progress *struct {
		Stage     string `json:"stage"`
		Processed int    `json:"processed"`
		Total     int    `json:"total"`
	} `json:"progress"`
}

func (o operation) id() string {
	if o.ID != "" {
		return o.ID
	}
	return o.OpID
}

func bankOps(c *bankClient, f *bankFlags) error {
	sub, err := f.need(0, "ls|show|cancel")
	if err != nil {
		return err
	}
	bank, err := f.need(1, "BANK")
	if err != nil {
		return err
	}
	switch sub {
	case "ls", "list":
		path := bankPath(bank, "operations")
		if s, ok := f.get("--status"); ok {
			path += "?status=" + url.QueryEscape(s)
		}
		var out struct {
			Operations []operation `json:"operations"`
			Total      int         `json:"total"`
		}
		if err := c.do("GET", path, nil, &out); err != nil {
			return notYet(err, "operations")
		}
		if f.on["--json"] {
			printJSON(out)
			return nil
		}
		for _, o := range out.Operations {
			progress := ""
			if o.Progress != nil && o.Progress.Total > 0 {
				progress = fmt.Sprintf(" %d/%d %s", o.Progress.Processed, o.Progress.Total, o.Progress.Stage)
			}
			fmt.Printf("%-38s %-22s %-10s %s%s\n", o.id(), o.Type, o.Status, o.Created, progress)
			if o.Error != "" {
				fmt.Printf("    %s\n", firstLine(o.Error))
			}
		}
		fmt.Printf("%d operation(s)\n", out.Total)
		return nil
	case "show":
		id, err := f.need(2, "operation ID")
		if err != nil {
			return err
		}
		var raw json.RawMessage
		if err := c.do("GET", bankPath(bank, "operations", url.PathEscape(id)), nil, &raw); err != nil {
			return notYet(err, "operations")
		}
		var pretty any
		_ = json.Unmarshal(raw, &pretty)
		printJSON(pretty)
		return nil
	case "cancel":
		id, err := f.need(2, "operation ID")
		if err != nil {
			return err
		}
		if err := c.do("DELETE", bankPath(bank, "operations", url.PathEscape(id)), nil, nil); err != nil {
			return notYet(err, "operations")
		}
		fmt.Printf("cancelled %s\n", id)
		return nil
	}
	return fmt.Errorf("ops takes ls, show or cancel")
}

// ---- templates ----------------------------------------------------------------

type templateModel struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	SourceQuery string         `json:"source_query"`
	Tags        []string       `json:"tags,omitempty"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	Trigger     map[string]any `json:"trigger,omitempty"`
}

type templateBank struct {
	Name          string            `json:"name,omitempty"`
	Mission       string            `json:"mission,omitempty"`
	RetainMission string            `json:"retain_mission,omitempty"`
	Disposition   *disposition      `json:"disposition,omitempty"`
	Directives    []directive       `json:"directives,omitempty"`
	Config        map[string]string `json:"config,omitempty"`
}

// fields is the template as POST /api/banks profile fields.
func (b templateBank) fields() map[string]any {
	out := map[string]any{}
	if b.Name != "" {
		out["name"] = b.Name
	}
	if b.Mission != "" {
		out["mission"] = b.Mission
	}
	if b.RetainMission != "" {
		out["retain_mission"] = b.RetainMission
	}
	if b.Disposition != nil {
		out["disposition"] = b.Disposition
	}
	if len(b.Directives) > 0 {
		out["directives"] = b.Directives
	}
	if len(b.Config) > 0 {
		out["config"] = b.Config
	}
	return out
}

type bankTemplate struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Manifest    struct {
		Bank         templateBank    `json:"bank"`
		MentalModels []templateModel `json:"mental_models"`
	} `json:"manifest"`
	builtin bool
}

func refreshAfterConsolidation() map[string]any {
	return map[string]any{"refresh_after_consolidation": true}
}

// builtinTemplates are used when the server offers none. They only set
// profile fields the server already accepts, so a bank made from one works on
// any server; their mental models are created where the server supports them.
// Keep in step with frontend/src/bankTemplates.ts.
func builtinTemplates() []bankTemplate {
	mk := func(id, name, desc string, b templateBank, models ...templateModel) bankTemplate {
		t := bankTemplate{ID: id, Name: name, Description: desc, builtin: true}
		t.Manifest.Bank, t.Manifest.MentalModels = b, models
		return t
	}
	return []bankTemplate{
		mk("assistant", "Personal assistant", "Remembers a person's preferences, plans, people and history.",
			templateBank{
				Mission:       "Remember what matters about the user — preferences, plans, commitments, relationships and history — so future conversations pick up where the last one left off.",
				RetainMission: "Keep durable facts about the user and the people and plans in their life. Skip greetings, small talk and anything only true for the moment.",
				Disposition:   &disposition{3, 3, 4},
			}),
		mk("coding-agent", "Coding agent", "One bank per repository: decisions, conventions, history and pitfalls.",
			templateBank{
				Mission:       "Record what a coding agent working in this repository needs to know: decisions and their reasons, conventions, history, and pitfalls.",
				RetainMission: "Extract technical decisions, architectural choices, conventions, gotchas, and the developer's preferences, with the reasons behind them. Ignore routine chatter and step-by-step tool output.",
				Disposition:   &disposition{3, 5, 2},
			},
			templateModel{ID: "project-context", Name: "Project context", SourceQuery: "What is this project, how is it structured, and what are its key technical decisions and constraints?", Trigger: refreshAfterConsolidation()},
			templateModel{ID: "developer-preferences", Name: "Developer preferences", SourceQuery: "What does the developer prefer and dislike in code style, tools, workflow and communication?", Trigger: refreshAfterConsolidation()},
			templateModel{ID: "review-patterns", Name: "Review patterns", SourceQuery: "What kinds of mistakes and review feedback come up repeatedly in this repository?", Trigger: refreshAfterConsolidation()}),
		mk("support", "Customer support", "Issues, resolutions and how each customer feels.",
			templateBank{
				Mission:       "Help support conversations go well: each customer's issues, what resolved them, and how they feel about it.",
				RetainMission: "Keep reported problems, their resolutions, product facts and the customer's sentiment and preferences.",
				Disposition:   &disposition{3, 3, 5},
			}),
		mk("research", "Research assistant", "Sourced, durable knowledge and the user's interests.",
			templateBank{
				Mission:       "Build up sourced, durable knowledge on the user's research interests and keep track of where each claim came from.",
				RetainMission: "Keep claims with their sources, findings, open questions and the user's interests. Note when sources disagree.",
				Disposition:   &disposition{4, 4, 3},
			}),
		mk("plain-retrieval", "Plain retrieval", "No model at retain: stores each chunk as written and recalls by meaning and words.",
			templateBank{
				Config: map[string]string{"retain_extraction_mode": "chunks", "enable_graph": "false",
					"enable_temporal": "false", "enable_reranking": "false"},
			}),
	}
}

// listTemplates asks the server, and falls back to the built-ins when it has
// no template route.
func listTemplates(c *bankClient) ([]bankTemplate, error) {
	var out struct {
		Templates []bankTemplate `json:"templates"`
	}
	err := c.do("GET", "/api/bank-templates", nil, &out)
	if routeMissing(err) {
		return builtinTemplates(), nil
	}
	if err != nil {
		return nil, err
	}
	return out.Templates, nil
}

func findTemplate(c *bankClient, id string) (*bankTemplate, error) {
	all, err := listTemplates(c)
	if err != nil {
		return nil, err
	}
	var names []string
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
		names = append(names, all[i].ID)
	}
	return nil, fmt.Errorf("no template %q (have: %s)", id, strings.Join(names, ", "))
}

func bankTemplates(c *bankClient, f *bankFlags) error {
	all, err := listTemplates(c)
	if err != nil {
		return err
	}
	if f.on["--json"] {
		printJSON(all)
		return nil
	}
	for _, t := range all {
		src := ""
		if t.builtin {
			src = "  (built in)"
		}
		fmt.Printf("%-16s %s — %s%s\n", t.ID, t.Name, t.Description, src)
	}
	return nil
}
