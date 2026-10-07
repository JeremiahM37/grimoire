package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// The bank surfaces built on retain and recall: reflect, observations and
// consolidation, mental models, directives, operations, templates, stats and
// export/import. The routes are documented in docs/MEMORY_BANKS.md.
//
// An older server (before these routes) answers an unknown route with a
// plain-text 404; each call turns that into errNotAvailable, so it reads as
// "this server does not have it", never as "your bank is missing".

func notYet(err error, what string) error {
	if routeMissing(err) {
		return fmt.Errorf("%s: %w", what, errNotAvailable)
	}
	if modelRequired(err) {
		return fmt.Errorf("%s needs a language model and none is configured on the server (set GRIMOIRE_LLM)", what)
	}
	return err
}

func optInt(f *bankFlags, flag string, body map[string]any, key string) error {
	if _, ok := f.get(flag); ok {
		n, err := f.int(flag, 0)
		if err != nil {
			return err
		}
		body[key] = n
	}
	return nil
}

// ---- reflect ------------------------------------------------------------------

type modelRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Text string `json:"text"`
}

type reflectResult struct {
	Text    string `json:"text"`
	Mode    string `json:"mode"`
	BasedOn struct {
		Memories     []recallFact `json:"memories"`
		Observations []recallFact `json:"observations"`
		MentalModels []modelRef   `json:"mental_models"`
		Directives   []directive  `json:"directives"`
	} `json:"based_on"`
	Structured      json.RawMessage `json:"structured_output"`
	StructuredError string          `json:"structured_output_error"`
	Usage           struct {
		Total int `json:"total_tokens"`
	} `json:"usage"`
	Iterations int `json:"iterations"`
	Trace      *struct {
		Levels    []string `json:"levels"`
		ToolCalls []struct {
			Tool       string         `json:"tool"`
			Input      map[string]any `json:"input"`
			DurationMS float64        `json:"duration_ms"`
			Iteration  int            `json:"iteration"`
			Forced     bool           `json:"forced"`
			Error      string         `json:"error"`
		} `json:"tool_calls"`
		Rejected []string `json:"rejected_citations"`
	} `json:"trace"`
}

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
	include := map[string]any{"facts": map[string]any{}}
	body := map[string]any{"query": query, "budget": f.str("--budget", "low"), "include": include}
	if err := optInt(f, "--max-tokens", body, "max_tokens"); err != nil {
		return err
	}
	if v, ok := f.get("--context"); ok {
		body["context"] = v
	}
	if t := f.list("--tags"); t != nil {
		body["tags"] = t
		body["tags_match"] = f.str("--tags-match", "any")
	}
	if t := f.list("--fact-types"); t != nil {
		body["fact_types"] = t
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
		include["tool_calls"] = map[string]any{}
	}
	raw, err := c.doRaw("POST", bankPath(bank, "reflect"), body)
	if err != nil {
		return notYet(err, "reflect")
	}
	if f.on["--json"] {
		fmt.Println(string(raw))
		return nil
	}
	var out reflectResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	if out.Mode == "extractive" {
		fmt.Println("(no language model configured: quoting what recall found)")
	}
	fmt.Println(strings.TrimSpace(out.Text))
	if len(out.Structured) > 0 && string(out.Structured) != "null" {
		fmt.Printf("\nstructured: %s\n", out.Structured)
	}
	if out.StructuredError != "" {
		fmt.Printf("\nstructured output failed: %s\n", out.StructuredError)
	}
	b := out.BasedOn
	if len(b.Memories)+len(b.Observations)+len(b.MentalModels) > 0 {
		fmt.Println("\nbased on:")
		for _, m := range b.MentalModels {
			fmt.Printf("  [model %s] %s\n", m.ID, firstLine(orDash(m.Name+": "+m.Text)))
		}
		for _, o := range b.Observations {
			fmt.Printf("  [observation %s] %s\n", o.ID, firstLine(o.Text))
		}
		for _, m := range b.Memories {
			fmt.Printf("  [%s %s] %s\n", m.Type, m.ID, firstLine(m.Text))
		}
	}
	if len(b.Directives) > 0 {
		fmt.Println("directives applied:")
		for _, d := range b.Directives {
			printDirective(d)
		}
	}
	if t := out.Trace; t != nil {
		fmt.Printf("\ntrace: levels %s · %d iteration(s)\n", strings.Join(t.Levels, " → "), out.Iterations)
		for _, tc := range t.ToolCalls {
			forced := ""
			if tc.Forced {
				forced = " (forced)"
			}
			in, _ := json.Marshal(tc.Input)
			fmt.Printf("  %d. %s%s %s · %.0f ms", tc.Iteration, tc.Tool, forced, firstLine(string(in)), tc.DurationMS)
			if tc.Error != "" {
				fmt.Printf(" · error: %s", tc.Error)
			}
			fmt.Println()
		}
		if len(t.Rejected) > 0 {
			fmt.Printf("  rejected citations (cited but never retrieved): %s\n", strings.Join(t.Rejected, ", "))
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

type observation struct {
	ID            string   `json:"id"`
	Text          string   `json:"text"`
	Authority     string   `json:"authority"`
	SourceFactIDs []string `json:"source_fact_ids"`
	ProofCount    int      `json:"proof_count"`
	Evidence      []struct {
		FactID string `json:"fact_id"`
		Quote  string `json:"quote"`
	} `json:"evidence"`
	Tags         []string `json:"tags"`
	UpdatedAt    string   `json:"updated_at"`
	Challenges   string   `json:"challenges"`
	Of           string   `json:"of"`
	SupersededAt string   `json:"superseded_at"`
	Deleted      bool     `json:"deleted"`
}

func (o observation) line() string {
	var meta []string
	if o.Authority == "human" {
		meta = append(meta, "yours")
	}
	if o.Challenges != "" {
		meta = append(meta, "challenges "+o.Challenges)
	}
	meta = append(meta, fmt.Sprintf("proof %d", o.ProofCount), fmt.Sprintf("%d source fact(s)", len(o.SourceFactIDs)))
	if len(o.Tags) > 0 {
		meta = append(meta, "tags "+strings.Join(o.Tags, ","))
	}
	if o.UpdatedAt != "" {
		meta = append(meta, "updated "+o.UpdatedAt)
	}
	return fmt.Sprintf("- %s\n    %s · %s", o.Text, o.ID, strings.Join(meta, " · "))
}

func consolidate(c *bankClient, bank string) error {
	var out struct {
		OperationID string `json:"operation_id"`
		Dedup       bool   `json:"deduplicated"`
	}
	if err := c.do("POST", bankPath(bank, "consolidate"), map[string]any{}, &out); err != nil {
		return notYet(err, "consolidation")
	}
	if out.Dedup {
		fmt.Printf("consolidation already queued: operation %s\n", out.OperationID)
	} else {
		fmt.Printf("consolidation queued: operation %s\n", out.OperationID)
	}
	return nil
}

func bankObservations(c *bankClient, f *bankFlags) error {
	// observations BANK | observations SUB BANK [ID]; the older form
	// "observations BANK consolidate" still works.
	first, err := f.need(0, "BANK")
	if err != nil {
		return err
	}
	switch first {
	case "consolidate", "show", "edit", "rm", "delete", "ls", "list":
		bank, err := f.need(1, "BANK")
		if err != nil {
			return err
		}
		switch first {
		case "consolidate":
			return consolidate(c, bank)
		case "show":
			id, err := f.need(2, "observation ID")
			if err != nil {
				return err
			}
			var out struct {
				Observation observation   `json:"observation"`
				History     []observation `json:"history"`
			}
			if err := c.do("GET", bankPath(bank, "observations", url.PathEscape(id)), nil, &out); err != nil {
				return notYet(err, "observations")
			}
			if f.on["--json"] {
				printJSON(out)
				return nil
			}
			fmt.Println(out.Observation.line())
			for _, ev := range out.Observation.Evidence {
				fmt.Printf("    evidence %s: %q\n", ev.FactID, ev.Quote)
			}
			if len(out.History) > 0 {
				fmt.Println("history:")
				for _, h := range out.History {
					fmt.Printf("  ~ %s (%s)\n", h.Text, orDash(h.SupersededAt))
				}
			}
			return nil
		case "edit":
			id, err := f.need(2, "observation ID")
			if err != nil {
				return err
			}
			text, err := f.need(3, "new text")
			if err != nil {
				return err
			}
			if err := c.do("PATCH", bankPath(bank, "observations", url.PathEscape(id)), map[string]any{"text": text}, nil); err != nil {
				return notYet(err, "observations")
			}
			fmt.Printf("edited %s; it is now yours, so consolidation will not rewrite it\n", id)
			return nil
		case "rm", "delete":
			id, err := f.need(2, "observation ID")
			if err != nil {
				return err
			}
			path := bankPath(bank, "observations", url.PathEscape(id))
			if f.on["--force"] {
				path += "?force=true"
			}
			if err := c.do("DELETE", path, nil, nil); err != nil {
				return notYet(err, "observations")
			}
			fmt.Printf("retired %s into history\n", id)
			return nil
		}
		first = bank
	}
	bank := first
	if len(f.pos) > 1 && f.pos[1] == "consolidate" {
		return consolidate(c, bank)
	}
	q := url.Values{}
	if v, ok := f.get("--q"); ok {
		q.Set("q", v)
	}
	if f.on["--human"] {
		q.Set("authority", "human")
	}
	if f.on["--history"] {
		q.Set("include_history", "1")
	}
	if v, ok := f.get("--limit"); ok {
		q.Set("limit", v)
	}
	path := bankPath(bank, "observations")
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out struct {
		Items   []observation `json:"items"`
		Total   int           `json:"total"`
		History []observation `json:"history,omitempty"`
	}
	if err := c.do("GET", path, nil, &out); err != nil {
		return notYet(err, "observations")
	}
	if f.on["--json"] {
		printJSON(out)
		return nil
	}
	for _, o := range out.Items {
		fmt.Println(o.line())
	}
	fmt.Printf("%d observation(s)\n", out.Total)
	if len(out.History) > 0 {
		fmt.Println("history:")
		for _, h := range out.History {
			state := "revised"
			if h.Deleted {
				state = "retired"
			}
			fmt.Printf("  ~ %s (%s %s, of %s)\n", h.Text, state, orDash(h.SupersededAt), h.Of)
		}
	}
	return nil
}

// ---- mental models ------------------------------------------------------------

type mentalModel struct {
	ID            string   `json:"id"`
	BankID        string   `json:"bank_id"`
	Name          string   `json:"name"`
	Question      string   `json:"question"`
	Folder        string   `json:"folder"`
	Path          string   `json:"path"`
	Tags          []string `json:"tags"`
	Refresh       string   `json:"refresh"`
	MaxTokens     int      `json:"max_tokens"`
	Budget        string   `json:"budget"`
	FactTypes     []string `json:"fact_types,omitempty"`
	Body          string   `json:"body"`
	Version       int      `json:"version"`
	LastRefreshed string   `json:"last_refreshed"`
	BasedOn       []string `json:"based_on"`
	Authority     string   `json:"authority"`
	IsStale       bool     `json:"is_stale"`
	StaleReason   string   `json:"stale_reason"`
	Pending       *struct {
		Content     string   `json:"content"`
		BasedOn     []string `json:"based_on"`
		CreatedAt   string   `json:"created_at"`
		BaseVersion int      `json:"base_version"`
	} `json:"pending_proposal"`
}

func (m mentalModel) state() string {
	parts := []string{"refreshed " + orDash(m.LastRefreshed)}
	if m.Authority == "human" {
		parts = append(parts, "edited by a person")
	}
	if m.IsStale {
		parts = append(parts, "stale ("+orDash(m.StaleReason)+")")
	}
	if m.Pending != nil {
		parts = append(parts, "refresh proposal waiting for you")
	}
	return strings.Join(parts, " · ")
}

func modelPath(bank, id string, rest ...string) string {
	return bankPath(bank, append([]string{"mental-models", url.PathEscape(id)}, rest...)...)
}

// bodyFlag reads --body or --body-file.
func bodyFlag(f *bankFlags) (string, bool, error) {
	if v, ok := f.get("--body"); ok {
		return v, true, nil
	}
	if p, ok := f.get("--body-file"); ok {
		raw, err := os.ReadFile(p)
		if err != nil {
			return "", false, err
		}
		return string(raw), true, nil
	}
	return "", false, nil
}

type modelNode struct {
	Kind     string       `json:"kind"`
	Name     string       `json:"name"`
	Path     string       `json:"path"`
	Model    *mentalModel `json:"model"`
	Children []modelNode  `json:"children"`
}

func printTree(nodes []modelNode, depth int) {
	for _, n := range nodes {
		pad := strings.Repeat("  ", depth)
		if n.Kind == "folder" {
			fmt.Printf("%s%s/\n", pad, n.Name)
			printTree(n.Children, depth+1)
			continue
		}
		label, mark := n.Name, ""
		if n.Model != nil {
			label = n.Model.Name + "  (" + n.Model.ID + ")"
			if n.Model.Pending != nil {
				mark += "  [proposal]"
			}
			if n.Model.IsStale {
				mark += "  [stale]"
			}
		}
		fmt.Printf("%s%s%s\n", pad, label, mark)
		printTree(n.Children, depth+1)
	}
}

func bankModels(c *bankClient, f *bankFlags) error {
	sub, err := f.need(0, "ls|tree|show|create|refresh|…")
	if err != nil {
		return err
	}
	bank, err := f.need(1, "BANK")
	if err != nil {
		return err
	}
	needID := func() (string, error) { return f.need(2, "model ID") }
	show := func(m *mentalModel) {
		if f.on["--json"] {
			printJSON(m)
			return
		}
		fmt.Printf("%s — %s\nquestion: %s\nversion %d · %s\n\n%s\n", m.ID, m.Name, m.Question, m.Version, m.state(), orDash(m.Body))
		if m.Pending != nil {
			fmt.Printf("\n--- proposed refresh (%s), not applied because a person edited this model; `grimoire bank models accept|reject %s %s`:\n%s\n",
				m.Pending.CreatedAt, bank, m.ID, m.Pending.Content)
		}
	}
	switch sub {
	case "ls", "list":
		path := bankPath(bank, "mental-models")
		if v, ok := f.get("--folder"); ok {
			path += "?folder=" + url.QueryEscape(v)
		}
		var out struct {
			Items []mentalModel `json:"items"`
			Total int           `json:"total"`
		}
		if err := c.do("GET", path, nil, &out); err != nil {
			return notYet(err, "mental models")
		}
		if f.on["--json"] {
			printJSON(out)
			return nil
		}
		for _, m := range out.Items {
			fmt.Printf("%-32s %s\n    %s · %s\n", m.ID, m.Name, firstLine(m.Question), m.state())
		}
		fmt.Printf("%d mental model(s)\n", out.Total)
		return nil
	case "tree":
		var out struct {
			Roots []modelNode `json:"roots"`
		}
		if err := c.do("GET", bankPath(bank, "mental-models-tree"), nil, &out); err != nil {
			return notYet(err, "mental models")
		}
		if f.on["--json"] {
			printJSON(out)
			return nil
		}
		if len(out.Roots) == 0 {
			fmt.Println("no mental models yet")
		}
		printTree(out.Roots, 0)
		return nil
	case "export":
		if f.on["--markdown"] {
			raw, err := c.doRaw("GET", bankPath(bank, "mental-models-export")+"?format=markdown", nil)
			if err != nil {
				return notYet(err, "mental models")
			}
			fmt.Println(strings.TrimRight(string(raw), "\n"))
			return nil
		}
		var out struct {
			Files []struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			} `json:"files"`
		}
		if err := c.do("GET", bankPath(bank, "mental-models-export"), nil, &out); err != nil {
			return notYet(err, "mental models")
		}
		printJSON(out)
		return nil
	case "show":
		id, err := needID()
		if err != nil {
			return err
		}
		var m mentalModel
		if err := c.do("GET", modelPath(bank, id), nil, &m); err != nil {
			return notYet(err, "mental models")
		}
		show(&m)
		return nil
	case "history":
		id, err := needID()
		if err != nil {
			return err
		}
		var out struct {
			Version  int `json:"version"`
			Versions []struct {
				ID   string `json:"id"`
				TS   any    `json:"ts"`
				Size int    `json:"size"`
			} `json:"versions"`
		}
		if err := c.do("GET", modelPath(bank, id, "history"), nil, &out); err != nil {
			return notYet(err, "mental models")
		}
		if f.on["--json"] {
			printJSON(out)
			return nil
		}
		fmt.Printf("current version %d\n", out.Version)
		for _, v := range out.Versions {
			fmt.Printf("  %s  %v  %d bytes\n", v.ID, v.TS, v.Size)
		}
		return nil
	case "create":
		name := strings.TrimSpace(strings.Join(f.pos[2:], " "))
		query := f.str("--query", "")
		if name == "" || query == "" {
			return fmt.Errorf("usage: grimoire bank models create BANK NAME --query QUESTION [--id ID] [--folder F] [--tags a,b] [--body TEXT]")
		}
		body := map[string]any{"name": name, "question": query, "tags": f.list("--tags")}
		if body["tags"] == nil {
			body["tags"] = []string{}
		}
		for flag, key := range map[string]string{"--id": "id", "--folder": "folder", "--budget": "budget"} {
			if v, ok := f.get(flag); ok {
				body[key] = v
			}
		}
		if err := optInt(f, "--max-tokens", body, "max_tokens"); err != nil {
			return err
		}
		text, has, err := bodyFlag(f)
		if err != nil {
			return err
		}
		if has {
			body["body"] = text
		}
		var out struct {
			Model       mentalModel `json:"mental_model"`
			ID          string      `json:"mental_model_id"`
			OperationID *string     `json:"operation_id"`
		}
		if err := c.do("POST", bankPath(bank, "mental-models"), body, &out); err != nil {
			return notYet(err, "mental models")
		}
		if f.on["--json"] {
			printJSON(out)
			return nil
		}
		switch {
		case out.OperationID != nil && *out.OperationID != "":
			fmt.Printf("created mental model %s; its first answer is being written (operation %s)\n", out.ID, *out.OperationID)
		case has:
			fmt.Printf("created mental model %s with your text\n", out.ID)
		default:
			fmt.Printf("created mental model %s (empty)\nnote: no language model is configured on the server; add a body with `models edit`, or configure one and run `models refresh`\n", out.ID)
		}
		return nil
	case "refresh":
		id, err := needID()
		if err != nil {
			return err
		}
		var out struct {
			OperationID string `json:"operation_id"`
			Status      string `json:"status"`
			Dedup       bool   `json:"deduplicated"`
		}
		if err := c.do("POST", modelPath(bank, id, "refresh"), map[string]any{}, &out); err != nil {
			if modelRequired(err) {
				return fmt.Errorf("refresh needs a language model and none is configured on the server")
			}
			return notYet(err, "mental models")
		}
		again := ""
		if out.Dedup {
			again = " (already queued)"
		}
		fmt.Printf("refresh %s%s: operation %s\n", orDash(out.Status), again, orDash(out.OperationID))
		return nil
	case "accept", "reject":
		id, err := needID()
		if err != nil {
			return err
		}
		var m mentalModel
		out := any(&m)
		if sub == "reject" {
			out = nil
		}
		if err := c.do("POST", modelPath(bank, id, "proposal", sub), map[string]any{}, out); err != nil {
			return notYet(err, "mental models")
		}
		if sub == "accept" {
			fmt.Printf("accepted the proposal: %s is version %d and the model writes it again from now on\n", m.ID, m.Version)
		} else {
			fmt.Printf("rejected the proposal; your text of %s stays\n", id)
		}
		return nil
	case "rm", "delete":
		id, err := needID()
		if err != nil {
			return err
		}
		if err := c.do("DELETE", modelPath(bank, id), nil, nil); err != nil {
			return notYet(err, "mental models")
		}
		fmt.Printf("deleted mental model %s\n", id)
		return nil
	case "edit", "move", "update":
		id, err := needID()
		if err != nil {
			return err
		}
		patch := map[string]any{}
		text, has, err := bodyFlag(f)
		if err != nil {
			return err
		}
		if has {
			patch["body"] = text
		}
		for flag, key := range map[string]string{"--folder": "folder", "--name": "name", "--query": "question", "--budget": "budget"} {
			if v, ok := f.get(flag); ok {
				patch[key] = v
			}
		}
		if t := f.list("--tags"); t != nil {
			patch["tags"] = t
		}
		if err := optInt(f, "--max-tokens", patch, "max_tokens"); err != nil {
			return err
		}
		if sub == "move" {
			if _, ok := patch["folder"]; !ok {
				return fmt.Errorf("usage: grimoire bank models move BANK ID --folder FOLDER (empty for the top level)")
			}
		}
		if len(patch) == 0 {
			return fmt.Errorf("nothing to change: pass --body/--body-file, --folder, --name, --query, --tags, --budget or --max-tokens")
		}
		var m mentalModel
		if err := c.do("PATCH", modelPath(bank, id), patch, &m); err != nil {
			return notYet(err, "mental models")
		}
		if f.on["--json"] {
			printJSON(m)
			return nil
		}
		if m.ID != id {
			fmt.Printf("moved %s → %s (the id follows the folder)\n", id, m.ID)
		} else {
			fmt.Printf("updated %s (version %d)\n", m.ID, m.Version)
		}
		return nil
	}
	return fmt.Errorf("models takes ls, tree, show, history, export, create, refresh, accept, reject, edit, move or rm")
}

// ---- directives ---------------------------------------------------------------

func printDirective(d directive) {
	var meta []string
	if d.Name != "" {
		meta = append(meta, d.Name)
	}
	if d.Priority != 0 {
		meta = append(meta, "priority "+strconv.Itoa(d.Priority))
	}
	if len(d.Tags) > 0 {
		meta = append(meta, "tags "+strings.Join(d.Tags, ","))
	}
	if d.Inactive {
		meta = append(meta, "inactive")
	}
	extra := ""
	if len(meta) > 0 {
		extra = "  (" + strings.Join(meta, " · ") + ")"
	}
	fmt.Printf("  [%s] %s%s\n", d.ID, d.Text, extra)
}

func bankDirectives(c *bankClient, f *bankFlags) error {
	sub, err := f.need(0, "ls|add|set|rm")
	if err != nil {
		return err
	}
	bank, err := f.need(1, "BANK")
	if err != nil {
		return err
	}
	spec := func(body map[string]any) error {
		for flag, key := range map[string]string{"--name": "name", "--text": "text"} {
			if v, ok := f.get(flag); ok {
				body[key] = v
			}
		}
		if t := f.list("--tags"); t != nil {
			body["tags"] = t
		}
		if f.on["--inactive"] {
			body["is_active"] = false
		}
		if f.on["--active"] {
			body["is_active"] = true
		}
		return optInt(f, "--priority", body, "priority")
	}
	switch sub {
	case "ls", "list":
		path := bankPath(bank, "directives")
		if f.on["--all"] {
			path += "?active_only=false"
		}
		var out struct {
			Items []directive `json:"items"`
			Total int         `json:"total"`
		}
		if err := c.do("GET", path, nil, &out); err != nil {
			return notYet(err, "directives")
		}
		if f.on["--json"] {
			printJSON(out)
			return nil
		}
		for _, d := range out.Items {
			printDirective(d)
		}
		fmt.Printf("%d directive(s)\n", out.Total)
		return nil
	case "add", "create":
		body := map[string]any{"text": strings.TrimSpace(strings.Join(f.pos[2:], " "))}
		if err := spec(body); err != nil {
			return err
		}
		if body["text"] == "" {
			return fmt.Errorf("usage: grimoire bank directives add BANK TEXT [--name N] [--tags a,b] [--priority N]")
		}
		var d directive
		if err := c.do("POST", bankPath(bank, "directives"), body, &d); err != nil {
			return notYet(err, "directives")
		}
		if f.on["--json"] {
			printJSON(d)
			return nil
		}
		fmt.Print("added")
		printDirective(d)
		return nil
	case "set", "update":
		id, err := f.need(2, "directive ID")
		if err != nil {
			return err
		}
		body := map[string]any{}
		if err := spec(body); err != nil {
			return err
		}
		if len(body) == 0 {
			return fmt.Errorf("nothing to change: pass --text, --name, --tags, --priority, --active or --inactive")
		}
		var d directive
		if err := c.do("PATCH", bankPath(bank, "directives", url.PathEscape(id)), body, &d); err != nil {
			return notYet(err, "directives")
		}
		if f.on["--json"] {
			printJSON(d)
			return nil
		}
		fmt.Print("updated")
		printDirective(d)
		return nil
	case "rm", "delete":
		id, err := f.need(2, "directive ID")
		if err != nil {
			return err
		}
		if err := c.do("DELETE", bankPath(bank, "directives", url.PathEscape(id)), nil, nil); err != nil {
			return notYet(err, "directives")
		}
		fmt.Printf("deleted directive %s\n", id)
		return nil
	}
	return fmt.Errorf("directives takes ls, add, set or rm")
}

// ---- operations ---------------------------------------------------------------

type operation struct {
	ID              string          `json:"id"`
	BankID          string          `json:"bank_id"`
	Kind            string          `json:"kind"`
	Type            string          `json:"type"`
	Status          string          `json:"status"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           string          `json:"error,omitempty"`
	Attempts        int             `json:"attempts"`
	CancelRequested bool            `json:"cancel_requested,omitempty"`
	Progress        string          `json:"progress,omitempty"`
	Created         string          `json:"created_at"`
	Started         string          `json:"started_at,omitempty"`
	Finished        string          `json:"finished_at,omitempty"`
	Updated         string          `json:"updated_at,omitempty"`
}

func (o operation) kind() string {
	if o.Kind != "" {
		return o.Kind
	}
	return o.Type
}

func terminal(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

func waitTimeout(f *bankFlags) (time.Duration, error) {
	v := f.str("--timeout", "10m")
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("--timeout takes a duration such as 90s or 5m")
	}
	return d, nil
}

// waitOp polls one operation until it finishes or the timeout passes.
func waitOp(c *bankClient, bank, id string, timeout time.Duration) (*operation, error) {
	deadline := time.Now().Add(timeout)
	delay := 100 * time.Millisecond
	for {
		var op operation
		if err := c.do("GET", bankPath(bank, "operations", url.PathEscape(id)), nil, &op); err != nil {
			return nil, notYet(err, "operations")
		}
		if terminal(op.Status) {
			return &op, nil
		}
		if time.Now().After(deadline) {
			return &op, fmt.Errorf("operation %s still %s after %s", id, op.Status, timeout)
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

func bankOps(c *bankClient, f *bankFlags) error {
	sub, err := f.need(0, "ls|show|wait|cancel")
	if err != nil {
		return err
	}
	bank, err := f.need(1, "BANK")
	if err != nil {
		return err
	}
	switch sub {
	case "ls", "list":
		q := url.Values{}
		for flag, key := range map[string]string{"--status": "status", "--type": "type", "--limit": "limit", "--offset": "offset"} {
			if v, ok := f.get(flag); ok {
				q.Set(key, v)
			}
		}
		path := bankPath(bank, "operations")
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		var out struct {
			BankID     string      `json:"bank_id"`
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
			fmt.Printf("%-24s %-20s %-10s %s", o.ID, o.kind(), o.Status, o.Created)
			if o.Progress != "" {
				fmt.Printf("  %s", o.Progress)
			}
			fmt.Println()
			if o.Error != "" {
				fmt.Printf("    %s\n", firstLine(o.Error))
			}
		}
		fmt.Printf("%d operation(s)\n", out.Total)
		return nil
	case "show", "wait":
		id, err := f.need(2, "operation ID")
		if err != nil {
			return err
		}
		var op *operation
		if sub == "wait" {
			timeout, err := waitTimeout(f)
			if err != nil {
				return err
			}
			if op, err = waitOp(c, bank, id, timeout); err != nil {
				return err
			}
		} else {
			op = &operation{}
			if err := c.do("GET", bankPath(bank, "operations", url.PathEscape(id)), nil, op); err != nil {
				return notYet(err, "operations")
			}
		}
		printJSON(op)
		if sub == "wait" && op.Status != "completed" {
			return fmt.Errorf("operation %s %s: %s", id, op.Status, orDash(op.Error))
		}
		return nil
	case "cancel":
		id, err := f.need(2, "operation ID")
		if err != nil {
			return err
		}
		var out struct {
			Status string `json:"status"`
		}
		if err := c.do("DELETE", bankPath(bank, "operations", url.PathEscape(id)), nil, &out); err != nil {
			return notYet(err, "operations")
		}
		fmt.Printf("cancelled %s (%s)\n", id, orDash(out.Status))
		return nil
	}
	return fmt.Errorf("ops takes ls, show, wait or cancel")
}

// ---- stats, export, import ------------------------------------------------------

func bankStats(c *bankClient, f *bankFlags) error {
	bank, err := f.need(0, "BANK")
	if err != nil {
		return err
	}
	var st struct {
		Facts        int            `json:"facts"`
		ByType       map[string]int `json:"facts_by_type"`
		Human        int            `json:"human_facts"`
		Observations int            `json:"observations"`
		Documents    int            `json:"documents"`
		Entities     int            `json:"entities"`
		Models       int            `json:"mental_models"`
		Pending      int            `json:"pending_consolidation"`
		Ops          map[string]int `json:"operations_by_status"`
		Consolidate  string         `json:"consolidation"`
		HaveModel    bool           `json:"model_available"`
	}
	raw, err := c.doRaw("GET", bankPath(bank, "stats"), nil)
	if err != nil {
		return notYet(err, "stats")
	}
	if f.on["--json"] {
		fmt.Println(string(raw))
		return nil
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	fmt.Printf("facts:          %d (world %d, experience %d; %d yours)\n", st.Facts, st.ByType["world"], st.ByType["experience"], st.Human)
	fmt.Printf("observations:   %d (%d fact(s) waiting for consolidation, consolidation %s)\n", st.Observations, st.Pending, orDash(st.Consolidate))
	fmt.Printf("documents:      %d\nentities:       %d\nmental models:  %d\n", st.Documents, st.Entities, st.Models)
	var ops []string
	for _, s := range []string{"queued", "running", "completed", "failed", "cancelled"} {
		if n := st.Ops[s]; n > 0 {
			ops = append(ops, fmt.Sprintf("%d %s", n, s))
		}
	}
	fmt.Printf("operations:     %s\nlanguage model: %v\n", orDash(strings.Join(ops, ", ")), st.HaveModel)
	return nil
}

func bankExport(c *bankClient, f *bankFlags) error {
	bank, err := f.need(0, "BANK")
	if err != nil {
		return err
	}
	raw, err := c.doRaw("GET", bankPath(bank, "export"), nil)
	if err != nil {
		return notYet(err, "export")
	}
	var pretty any
	if err := json.Unmarshal(raw, &pretty); err != nil {
		return err
	}
	printJSON(pretty)
	return nil
}

type importResult struct {
	BankID            string   `json:"bank_id"`
	BankCreated       bool     `json:"bank_created"`
	ConfigApplied     []string `json:"config_applied"`
	ModelsCreated     []string `json:"mental_models_created"`
	ModelsUpdated     []string `json:"mental_models_updated"`
	DirectivesCreated []string `json:"directives_created"`
	DirectivesUpdated []string `json:"directives_updated"`
	OperationIDs      []string `json:"operation_ids"`
	DryRun            bool     `json:"dry_run"`
}

func importManifest(c *bankClient, bank string, body any, dry bool) (*importResult, error) {
	path := bankPath(bank, "import")
	if dry {
		path += "?dry_run=1"
	}
	var res importResult
	if err := c.do("POST", path, body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func bankImport(c *bankClient, f *bankFlags) error {
	bank, err := f.need(0, "BANK")
	if err != nil {
		return err
	}
	var body any
	if t, ok := f.get("--template"); ok {
		body = map[string]any{"template": t}
	} else {
		file, err := f.need(1, "FILE (or --template T)")
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("%s: %v", file, err)
		}
		body = m
	}
	res, err := importManifest(c, bank, body, f.on["--dry-run"])
	if err != nil {
		return notYet(err, "import")
	}
	if f.on["--json"] {
		printJSON(res)
		return nil
	}
	verb := "imported into"
	if res.DryRun {
		verb = "would import into"
	}
	created := ""
	if res.BankCreated {
		created = " (new bank)"
	}
	fmt.Printf("%s %s%s\n", verb, res.BankID, created)
	for _, row := range []struct {
		what string
		ids  []string
	}{{"settings applied", res.ConfigApplied}, {"mental models created", res.ModelsCreated},
		{"mental models updated", res.ModelsUpdated}, {"directives created", res.DirectivesCreated},
		{"directives updated", res.DirectivesUpdated}, {"operations queued", res.OperationIDs}} {
		if len(row.ids) > 0 {
			fmt.Printf("  %s: %s\n", row.what, strings.Join(row.ids, ", "))
		}
	}
	return nil
}

// ---- templates ----------------------------------------------------------------

type templateModel struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Question  string   `json:"question"`
	Tags      []string `json:"tags,omitempty"`
	Refresh   string   `json:"refresh,omitempty"`
	MaxTokens int      `json:"max_tokens,omitempty"`
	Budget    string   `json:"budget,omitempty"`
	FactTypes []string `json:"fact_types,omitempty"`
}

type templateDirective struct {
	Name     string   `json:"name,omitempty"`
	Text     string   `json:"text"`
	Tags     []string `json:"tags,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Inactive bool     `json:"inactive,omitempty"`
}

type templateBank struct {
	Name          string            `json:"name,omitempty"`
	Mission       string            `json:"mission,omitempty"`
	RetainMission string            `json:"retain_mission,omitempty"`
	Disposition   *disposition      `json:"disposition,omitempty"`
	Tags          []string          `json:"tags,omitempty"`
	Config        map[string]string `json:"config,omitempty"`
}

type templateManifest struct {
	Version      string              `json:"version"`
	Bank         *templateBank       `json:"bank,omitempty"`
	MentalModels []templateModel     `json:"mental_models,omitempty"`
	Directives   []templateDirective `json:"directives,omitempty"`
}

type bankTemplate struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Manifest    templateManifest `json:"manifest"`
	builtin     bool
}

// phase1Config are the bank settings a server older than templates accepts;
// the fallback path sends no others, since such a server refuses unknown keys.
var phase1Config = map[string]bool{"retain_extraction_mode": true, "retain_chunk_size": true,
	"retain_extract_causal": true, "enable_text_search": true, "enable_graph": true,
	"enable_temporal": true, "enable_reranking": true, "consolidation": true}

// profileFields is the manifest as POST /api/banks fields, for a server that
// cannot import a manifest: no mental models, directives as profile bullets.
func (m templateManifest) profileFields() map[string]any {
	out := map[string]any{}
	if b := m.Bank; b != nil {
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
		cfg := map[string]string{}
		for k, v := range b.Config {
			if phase1Config[k] {
				cfg[k] = v
			}
		}
		if len(cfg) > 0 {
			out["config"] = cfg
		}
	}
	if len(m.Directives) > 0 {
		ds := make([]directive, 0, len(m.Directives))
		for _, d := range m.Directives {
			ds = append(ds, directive{Text: d.Text, Tags: d.Tags})
		}
		out["directives"] = ds
	}
	return out
}

func disp(s, l, e int) *disposition { return &disposition{s, l, e} }

// builtinTemplates are used only when the server offers none (a server older
// than bank templates). They mirror go/internal/bank/templates.go and
// frontend/src/bankTemplates.ts.
func builtinTemplates() []bankTemplate {
	mk := func(id, name, desc string, m templateManifest) bankTemplate {
		m.Version = "1"
		return bankTemplate{ID: id, Name: name, Description: desc, Manifest: m, builtin: true}
	}
	return []bankTemplate{
		mk("assistant", "Personal assistant",
			"A general assistant that remembers the user: who they are, what they prefer, what they are working on and what they asked for.",
			templateManifest{Bank: &templateBank{
				Mission:       "Help the user by remembering who they are, the people in their life, their preferences, commitments and ongoing plans.",
				RetainMission: "Keep facts about the user and the people around them, their preferences, plans, commitments, routines and decisions. Skip small talk.",
				Disposition:   disp(3, 3, 4),
				Config:        map[string]string{"consolidation": "auto"},
			}, MentalModels: []templateModel{
				{ID: "user-profile", Name: "User profile", Question: "Who is the user: background, work, relationships and what matters to them?"},
				{ID: "preferences", Name: "Preferences", Question: "What does the user prefer and dislike, in how they work, communicate and live?"},
				{ID: "open-commitments", Name: "Open commitments", Question: "What has the user committed to or planned that is not yet done, with dates?"},
			}}),
		mk("coding-agent", "Coding agent",
			"Memory for an agent working in one codebase: decisions and their reasons, conventions, the developer's preferences and recurring review feedback.",
			templateManifest{Bank: &templateBank{
				Mission:       "Support work on this codebase: recall the project's decisions, conventions and constraints, and how the developer likes things done.",
				RetainMission: "Keep technical decisions and their reasons, conventions, architecture, build and test commands, constraints, bugs found and their fixes, and the developer's stated preferences. Skip routine tool output.",
				Disposition:   disp(3, 5, 2),
				Config: map[string]string{"consolidation": "auto",
					"observations_mission": "Track project facts: architecture, conventions, decisions with reasons, known pitfalls, and the developer's preferences."},
			}, MentalModels: []templateModel{
				{ID: "project-context", Name: "Project context", Question: "What is this project, how is it built and tested, and what are its main components and constraints?"},
				{ID: "developer-preferences", Name: "Developer preferences", Question: "How does the developer want code written, reviewed and committed?"},
				{ID: "review-patterns", Name: "Review patterns", Question: "What feedback recurs in reviews of changes to this project, and what mistakes keep coming back?"},
			}, Directives: []templateDirective{
				{Name: "Cite decisions", Text: "When an answer rests on a past decision, say when it was made and why."},
			}}),
		mk("support", "Customer support",
			"Memory for a support agent: each customer's history, issues and how they were resolved, and how the customer felt about it.",
			templateManifest{Bank: &templateBank{
				Mission:       "Help support staff serve each customer well, knowing their history, open issues and what resolved similar problems before.",
				RetainMission: "Keep customer details, products and plans, reported issues with dates, what was tried, what resolved them, and how the customer felt. Skip pleasantries.",
				Disposition:   disp(2, 3, 5),
				Config:        map[string]string{"consolidation": "auto"},
			}, MentalModels: []templateModel{
				{ID: "open-issues", Name: "Open issues", Question: "Which customer issues are unresolved, since when, and what has been tried?"},
				{ID: "known-fixes", Name: "Known fixes", Question: "Which problems keep recurring and what resolved them?"},
			}, Directives: []templateDirective{
				{Name: "No internal notes", Text: "Never repeat internal-only notes or other customers' details in an answer meant for a customer."},
			}}),
		mk("research", "Research assistant",
			"Memory for research: sources, claims with where they came from, open questions, and how the user's thinking developed.",
			templateManifest{Bank: &templateBank{
				Mission:       "Support research by keeping track of sources, the claims they make, open questions and how conclusions developed.",
				RetainMission: "Keep claims with their sources, numbers with units and dates, methods, open questions, disagreements between sources, and the user's interests. Skip chatter.",
				Disposition:   disp(4, 4, 2),
				Config:        map[string]string{"consolidation": "auto"},
			}, MentalModels: []templateModel{
				{ID: "open-questions", Name: "Open questions", Question: "Which research questions are still open, and what evidence bears on each?"},
				{ID: "key-findings", Name: "Key findings", Question: "What are the main findings so far, with the sources that support them?"},
			}, Directives: []templateDirective{
				{Name: "Attribute claims", Text: "Attribute every claim to its source, and say when sources disagree."},
			}}),
		mk("plain-retrieval", "Plain retrieval",
			"A bank with no model in the loop: content is stored chunk by chunk and recalled by meaning, words and time.",
			templateManifest{Bank: &templateBank{
				Config: map[string]string{"retain_extraction_mode": "chunks", "consolidation": "off", "enable_graph": "false"},
			}}),
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

// createBank creates a bank (409 when it exists), from a template when one is
// given. overrides (name, mission, retain_mission) win over the template's.
// On a server with templates the manifest is imported, so its mental models
// and directives are made by the server; an older one gets the profile fields
// the template can express and a note that the models were skipped.
func createBank(c *bankClient, id string, tpl *bankTemplate, overrides map[string]string) (*bankProfile, *importResult, error) {
	body := map[string]any{"bank_id": id}
	for k, v := range overrides {
		body[k] = v
	}
	legacy := func() {
		for k, v := range tpl.Manifest.profileFields() {
			if _, set := body[k]; !set {
				body[k] = v
			}
		}
	}
	if tpl != nil && tpl.builtin {
		legacy()
	}
	var p bankProfile
	if err := c.do("POST", "/api/banks", body, &p); err != nil {
		return nil, nil, err
	}
	if tpl == nil {
		return &p, nil, nil
	}
	if tpl.builtin {
		if n := len(tpl.Manifest.MentalModels); n > 0 {
			fmt.Fprintf(os.Stderr, "note: this server has no mental models; the template's %d model(s) were skipped\n", n)
		}
		return &p, nil, nil
	}
	m := tpl.Manifest
	if m.Version == "" {
		m.Version = "1"
	}
	b := templateBank{}
	if m.Bank != nil {
		b = *m.Bank
	}
	if v, ok := overrides["name"]; ok {
		b.Name = v
	}
	if v, ok := overrides["mission"]; ok {
		b.Mission = v
	}
	if v, ok := overrides["retain_mission"]; ok {
		b.RetainMission = v
	}
	m.Bank = &b
	res, err := importManifest(c, id, map[string]any{"manifest": m}, false)
	if routeMissing(err) {
		// Templates but no import: set what a profile can hold.
		body = map[string]any{}
		legacy()
		for k, v := range overrides {
			body[k] = v
		}
		if err := c.do("PATCH", bankPath(id), body, &p); err != nil {
			return nil, nil, err
		}
		fmt.Fprintln(os.Stderr, "note: this server cannot import a template; its mental models were skipped")
		return &p, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("bank %s was created but the template was not applied: %w", id, err)
	}
	if fresh, err := getProfile(c, id); err == nil {
		p = *fresh
	}
	return &p, res, nil
}

func bankTemplates(c *bankClient, f *bankFlags) error {
	if len(f.pos) > 0 && f.pos[0] == "show" {
		id, err := f.need(1, "template ID")
		if err != nil {
			return err
		}
		t, err := findTemplate(c, id)
		if err != nil {
			return err
		}
		printJSON(t)
		return nil
	}
	all, err := listTemplates(c)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.status == http.StatusUnauthorized {
			return fmt.Errorf("templates need a signed-in user: pass --token or set GRIMOIRE_AUTH_TOKEN")
		}
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
		extra := ""
		if n := len(t.Manifest.MentalModels); n > 0 {
			extra = fmt.Sprintf(" [%d mental model(s)]", n)
		}
		fmt.Printf("%-16s %s — %s%s%s\n", t.ID, t.Name, t.Description, extra, src)
	}
	return nil
}
