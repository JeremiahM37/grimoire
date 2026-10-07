package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/mcp"
)

// `grimoire bank …` — memory banks from the shell.
//
// Unlike the vault commands, these talk to a RUNNING server over HTTP. A bank
// retain may call the configured model once per chunk and a recall reads the
// server's in-memory bank cache; opening a second engine on the same vault from
// a CLI process would duplicate both and race the server's writes. So the bank
// commands are an HTTP client, addressed the same way the MCP server is:
// GRIMOIRE_URL (or --url), GRIMOIRE_AUTH_TOKEN (or --token), and
// GRIMOIRE_AGENT_NAME (or --agent) for provenance.

const bankUsage = `usage: grimoire bank COMMAND [args] [--url URL] [--token T] [--json]

  list                                   banks you can read
  create BANK [--template T] [--name N] [--mission M] [--retain-mission M]
  show BANK                              profile: mission, disposition, directives, config
  stats BANK                             counts: facts, observations, models, pending work
  update BANK [--name N] [--mission M] [--retain-mission M]
              [--disposition S,L,E] [--skepticism N] [--literalism N] [--empathy N]
              [--directive TEXT]... [--remove-directive ID] [--clear-directives]
              [--config KEY=VALUE]...
  delete BANK --yes
  export BANK                            the bank's manifest (profile, settings, models, directives)
  import BANK FILE | --template T [--dry-run]   apply a manifest; additive
  retain BANK [TEXT...] [--file F | --dir D] [--document-id ID] [--timestamp T|mtime]
              [--context C] [--tags a,b] [--mode concise|verbatim|chunks]
              [--update-mode replace|append] [--async [--wait]]   (stdin when no text/file/dir)
  recall BANK QUERY [--budget low|mid|high] [--max-tokens N] [--types a,b]
              [--tags a,b] [--tags-match any|all|any_strict|all_strict] [--chunks] [--trace]
  reflect BANK QUERY [--budget B] [--max-tokens N] [--context C] [--schema FILE]
              [--tags a,b] [--fact-types a,b] [--trace]
  memories ls BANK [--type T] [--document-id D] [--q TEXT] [--human] [--limit N] [--offset N]
  memories rm BANK ID [--force]
  entities BANK [NAME]                   entities, or one entity with its facts
  documents BANK [ID] [--rm] [--force]   documents, one document, or delete it
  observations BANK [--q TEXT] [--human] [--history]
  observations show BANK ID | rm BANK ID [--force] | consolidate BANK
  models ls BANK | tree BANK | show BANK ID | history BANK ID | export BANK [--markdown]
  models create BANK NAME --query Q [--id ID] [--folder F] [--tags a,b] [--body TEXT|--body-file F]
  models refresh BANK ID | accept BANK ID | reject BANK ID | rm BANK ID
  models edit BANK ID --body TEXT|--body-file F | move BANK ID --folder F
  directives ls BANK [--all] | add BANK TEXT [--name N] [--tags a,b] [--priority N]
  directives set BANK ID [--text T] [--name N] [--tags a,b] [--priority N] [--active|--inactive]
  directives rm BANK ID
  ops ls BANK [--status S] [--type T] | show BANK ID | wait BANK ID [--timeout 2m] | cancel BANK ID
  templates [show ID]                    bank templates (server, else built-in)
  import-git REPO [--bank B] [--limit 300] [--diffs] [--max-diff-bytes N]
              [--force] [--dry-run]      retain recent commits into coding-agent:<repo>

Env: GRIMOIRE_URL (default http://127.0.0.1:9111), GRIMOIRE_AUTH_TOKEN, GRIMOIRE_AGENT_NAME`

// bankValued are the flags that take a value. Everything else starting with
// "--" is a switch.
var bankValued = map[string]bool{
	"--url": true, "--token": true, "--agent": true,
	"--template": true, "--name": true, "--mission": true, "--retain-mission": true,
	"--disposition": true, "--skepticism": true, "--literalism": true, "--empathy": true,
	"--directive": true, "--remove-directive": true, "--config": true,
	"--file": true, "--dir": true, "--document-id": true, "--timestamp": true,
	"--context": true, "--tags": true, "--mode": true, "--update-mode": true,
	"--budget": true, "--max-tokens": true, "--types": true, "--tags-match": true,
	"--schema": true, "--type": true, "--q": true, "--limit": true, "--offset": true,
	"--query": true, "--id": true, "--status": true, "--bank": true,
	"--max-diff-bytes": true, "--max-files": true, "--batch": true,
	"--fact-types": true, "--folder": true, "--body": true, "--body-file": true,
	"--text": true, "--priority": true, "--timeout": true,
}

// bankFlags is a parsed command line: positional words, the last value of
// each valued flag, every value of a repeatable one, and the switches.
type bankFlags struct {
	pos    []string
	values map[string][]string
	on     map[string]bool
}

func parseBankFlags(args []string) (*bankFlags, error) {
	f := &bankFlags{values: map[string][]string{}, on: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			f.pos = append(f.pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "--") || a == "-" {
			f.pos = append(f.pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		if bankValued[name] {
			if !hasVal {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("%s needs a value", name)
				}
				i++
				val = args[i]
			}
			f.values[name] = append(f.values[name], val)
			continue
		}
		f.on[name] = true
	}
	return f, nil
}

func (f *bankFlags) get(name string) (string, bool) {
	v := f.values[name]
	if len(v) == 0 {
		return "", false
	}
	return v[len(v)-1], true
}

func (f *bankFlags) str(name, def string) string {
	if v, ok := f.get(name); ok {
		return v
	}
	return def
}

func (f *bankFlags) list(name string) []string {
	v, ok := f.get(name)
	if !ok {
		return nil
	}
	return splitCSV(v)
}

func (f *bankFlags) int(name string, def int) (int, error) {
	v, ok := f.get(name)
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number", name)
	}
	return n, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---- HTTP client ------------------------------------------------------------

type bankClient struct {
	base, token, admin, agent string
	http                      *http.Client
}

func newBankClient(f *bankFlags) *bankClient {
	base := f.str("--url", envOr(mcp.EnvURL, "http://127.0.0.1:9111"))
	return &bankClient{
		base:  strings.TrimRight(base, "/"),
		token: f.str("--token", os.Getenv("GRIMOIRE_AUTH_TOKEN")),
		admin: os.Getenv("GRIMOIRE_ADMIN_TOKEN"),
		agent: f.str("--agent", envOr(mcp.EnvAgentName, "cli")),
		// A synchronous retain makes one model call per chunk; a large
		// document legitimately takes minutes.
		http: &http.Client{Timeout: 15 * time.Minute},
	}
}

// apiError is a refusal from the server, with its status kept so callers can
// tell "no such bank" from "this server has no such route".
type apiError struct {
	status int
	detail string
	code   string // machine-readable reason, e.g. "model_required"
	json   bool
}

func (e *apiError) Error() string { return fmt.Sprintf("%d: %s", e.status, e.detail) }

// errNotAvailable marks a feature the server does not have yet.
var errNotAvailable = errors.New("not available on this server")

// routeMissing reports whether err means the route does not exist on this
// server: Go's mux answers an unknown path with a plain-text 404 (and a known
// path with the wrong method with 405), while a missing bank or fact is a
// JSON {"detail": …} 404.
func routeMissing(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && (ae.status == http.StatusMethodNotAllowed ||
		ae.status == http.StatusNotFound && !ae.json)
}

// modelRequired reports whether err is the server saying the call needs a
// language model and none is configured.
func modelRequired(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status == http.StatusConflict &&
		(ae.code == "model_required" || strings.HasPrefix(ae.detail, "model_required"))
}

// do sends one request and decodes a JSON reply into out (when non-nil).
func (c *bankClient) do(method, path string, body, out any) error {
	raw, err := c.doRaw(method, path, body)
	if err != nil || out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return err
	}
	return json.Unmarshal(raw, out)
}

// doRaw sends one request and returns the reply body as it came.
func (c *bankClient) doRaw(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.admin != "" {
		req.Header.Set("X-Grimoire-Admin", c.admin)
	}
	if c.agent != "" {
		req.Header.Set("X-Grimoire-Agent", c.agent)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grimoire unreachable at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		ae := &apiError{status: resp.StatusCode, detail: strings.TrimSpace(string(raw))}
		var payload struct {
			Detail string `json:"detail"`
			Code   string `json:"code"`
		}
		if json.Unmarshal(raw, &payload) == nil && payload.Detail != "" {
			ae.detail, ae.code, ae.json = payload.Detail, payload.Code, true
		}
		return nil, ae
	}
	return raw, nil
}

func bankPath(id string, rest ...string) string {
	p := "/api/banks/" + url.PathEscape(id)
	for _, r := range rest {
		p += "/" + r
	}
	return p
}

func printJSON(v any) {
	raw, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(raw))
}

// ---- dispatch -----------------------------------------------------------------

type bankCmd func(c *bankClient, f *bankFlags) error

func bankCommands() map[string]bankCmd {
	return map[string]bankCmd{
		"list": bankList, "ls": bankList, "create": bankCreate, "show": bankShow,
		"update": bankUpdate, "delete": bankDelete, "retain": bankRetain,
		"recall": bankRecall, "reflect": bankReflect, "memories": bankMemories,
		"entities": bankEntities, "documents": bankDocuments,
		"observations": bankObservations, "models": bankModels, "ops": bankOps,
		"directives": bankDirectives, "stats": bankStats, "export": bankExport, "import": bankImport,
		"templates": bankTemplates, "import-git": bankImportGit,
	}
}

func cmdBank(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(bankUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	fn, ok := bankCommands()[args[0]]
	if !ok {
		return fail("unknown bank command %q\n\n%s", args[0], bankUsage)
	}
	f, err := parseBankFlags(args[1:])
	if err != nil {
		return fail("%v", err)
	}
	if err := fn(newBankClient(f), f); err != nil {
		if errors.Is(err, errNotAvailable) {
			return fail("%v", err)
		}
		return fail("bank %s: %v", args[0], err)
	}
	return 0
}

// need returns the n-th positional word or a usage error naming it.
func (f *bankFlags) need(i int, what string) (string, error) {
	if i < len(f.pos) && strings.TrimSpace(f.pos[i]) != "" {
		return f.pos[i], nil
	}
	return "", fmt.Errorf("missing %s (see `grimoire bank help`)", what)
}

// ---- profile commands ---------------------------------------------------------

type bankSummary struct {
	ID        string `json:"bank_id"`
	Name      string `json:"name"`
	Facts     int    `json:"facts"`
	Documents int    `json:"documents"`
	Updated   string `json:"updated"`
}

type disposition struct {
	Skepticism int `json:"skepticism"`
	Literalism int `json:"literalism"`
	Empathy    int `json:"empathy"`
}

type directive struct {
	ID       string   `json:"id,omitempty"`
	Name     string   `json:"name,omitempty"`
	Text     string   `json:"text"`
	Tags     []string `json:"tags,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Inactive bool     `json:"inactive,omitempty"`
}

type bankProfile struct {
	ID            string            `json:"bank_id"`
	Name          string            `json:"name"`
	Mission       string            `json:"mission"`
	RetainMission string            `json:"retain_mission"`
	Disposition   disposition       `json:"disposition"`
	Tags          []string          `json:"tags"`
	Directives    []directive       `json:"directives"`
	Config        map[string]string `json:"config"`
	Created       string            `json:"created"`
	Updated       string            `json:"updated"`
}

func bankList(c *bankClient, f *bankFlags) error {
	var out struct {
		Banks []bankSummary `json:"banks"`
	}
	if err := c.do("GET", "/api/banks", nil, &out); err != nil {
		return err
	}
	if f.on["--json"] {
		printJSON(out)
		return nil
	}
	if len(out.Banks) == 0 {
		fmt.Println("no banks yet — `grimoire bank create NAME` or retain into one")
		return nil
	}
	for _, b := range out.Banks {
		name := ""
		if b.Name != "" && b.Name != b.ID {
			name = "  (" + b.Name + ")"
		}
		fmt.Printf("%-32s %6d facts %5d documents%s\n", b.ID, b.Facts, b.Documents, name)
	}
	return nil
}

func bankCreate(c *bankClient, f *bankFlags) error {
	id, err := f.need(0, "BANK")
	if err != nil {
		return err
	}
	overrides := map[string]string{}
	for flag, key := range map[string]string{"--name": "name", "--mission": "mission", "--retain-mission": "retain_mission"} {
		if v, ok := f.get(flag); ok {
			overrides[key] = v
		}
	}
	var tpl *bankTemplate
	if t, ok := f.get("--template"); ok {
		if tpl, err = findTemplate(c, t); err != nil {
			return err
		}
	}
	p, imp, err := createBank(c, id, tpl, overrides)
	if err != nil {
		return err
	}
	if f.on["--json"] {
		printJSON(map[string]any{"bank": p, "import": imp})
		return nil
	}
	fmt.Printf("created bank %s", p.ID)
	if imp != nil {
		var parts []string
		if n := len(imp.ModelsCreated); n > 0 {
			parts = append(parts, fmt.Sprintf("%d mental model(s)", n))
		}
		if n := len(imp.DirectivesCreated); n > 0 {
			parts = append(parts, fmt.Sprintf("%d directive(s)", n))
		}
		if len(parts) > 0 {
			fmt.Printf(" with %s", strings.Join(parts, " and "))
		}
	}
	fmt.Println()
	if imp != nil && len(imp.ModelsCreated) > 0 && len(imp.OperationIDs) == 0 {
		fmt.Println("note: the server has no language model configured, so the mental models stay empty until one is and they are refreshed")
	}
	return nil
}

func getProfile(c *bankClient, id string) (*bankProfile, error) {
	var p bankProfile
	if err := c.do("GET", bankPath(id), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func bankShow(c *bankClient, f *bankFlags) error {
	id, err := f.need(0, "BANK")
	if err != nil {
		return err
	}
	p, err := getProfile(c, id)
	if err != nil {
		return err
	}
	if f.on["--json"] {
		printJSON(p)
		return nil
	}
	printProfile(p)
	return nil
}

func printProfile(p *bankProfile) {
	fmt.Printf("bank:         %s\nname:         %s\n", p.ID, p.Name)
	fmt.Printf("mission:      %s\n", orDash(p.Mission))
	fmt.Printf("retain:       %s\n", orDash(p.RetainMission))
	d := p.Disposition
	fmt.Printf("disposition:  skepticism %d · literalism %d · empathy %d\n", d.Skepticism, d.Literalism, d.Empathy)
	if len(p.Tags) > 0 {
		fmt.Printf("tags:         %s\n", strings.Join(p.Tags, ", "))
	}
	fmt.Println("directives:")
	if len(p.Directives) == 0 {
		fmt.Println("  —")
	}
	for _, d := range p.Directives {
		printDirective(d)
	}
	if len(p.Config) > 0 {
		keys := make([]string, 0, len(p.Config))
		for k := range p.Config {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Println("config:")
		for _, k := range keys {
			fmt.Printf("  %s = %s\n", k, p.Config[k])
		}
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func bankUpdate(c *bankClient, f *bankFlags) error {
	id, err := f.need(0, "BANK")
	if err != nil {
		return err
	}
	patch := map[string]any{}
	for flag, key := range map[string]string{"--name": "name", "--mission": "mission", "--retain-mission": "retain_mission"} {
		if v, ok := f.get(flag); ok {
			patch[key] = v
		}
	}
	needProfile := false
	for _, k := range []string{"--disposition", "--skepticism", "--literalism", "--empathy", "--directive", "--remove-directive"} {
		if _, ok := f.get(k); ok {
			needProfile = true
		}
	}
	if needProfile || f.on["--clear-directives"] {
		cur, err := getProfile(c, id)
		if err != nil {
			return err
		}
		d := cur.Disposition
		touched := false
		if v, ok := f.get("--disposition"); ok {
			parts := splitCSV(v)
			if len(parts) != 3 {
				return fmt.Errorf("--disposition takes three numbers: skepticism,literalism,empathy")
			}
			for i, ptr := range []*int{&d.Skepticism, &d.Literalism, &d.Empathy} {
				n, err := strconv.Atoi(parts[i])
				if err != nil {
					return fmt.Errorf("--disposition: %q is not a number", parts[i])
				}
				*ptr = n
			}
			touched = true
		}
		for flag, ptr := range map[string]*int{"--skepticism": &d.Skepticism, "--literalism": &d.Literalism, "--empathy": &d.Empathy} {
			if _, ok := f.get(flag); ok {
				n, err := f.int(flag, 3)
				if err != nil {
					return err
				}
				*ptr = n
				touched = true
			}
		}
		for _, n := range []int{d.Skepticism, d.Literalism, d.Empathy} {
			if n < 1 || n > 5 {
				return fmt.Errorf("disposition traits are 1..5")
			}
		}
		if touched {
			patch["disposition"] = d
		}
		dirs := cur.Directives
		dirChanged := false
		if f.on["--clear-directives"] {
			dirs, dirChanged = nil, true
		}
		if rm := f.values["--remove-directive"]; len(rm) > 0 {
			drop := map[string]bool{}
			for _, r := range rm {
				drop[r] = true
			}
			kept := dirs[:0:0]
			for _, d := range dirs {
				if !drop[d.ID] && !drop[d.Text] {
					kept = append(kept, d)
				}
			}
			if len(kept) == len(dirs) {
				return fmt.Errorf("no directive with id %s", strings.Join(rm, ", "))
			}
			dirs, dirChanged = kept, true
		}
		for _, text := range f.values["--directive"] {
			if text = strings.TrimSpace(text); text != "" {
				dirs = append(dirs, directive{Text: text})
				dirChanged = true
			}
		}
		if dirChanged {
			if dirs == nil {
				dirs = []directive{}
			}
			patch["directives"] = dirs
		}
	}
	if cfg := f.values["--config"]; len(cfg) > 0 {
		m := map[string]string{}
		for _, kv := range cfg {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("--config takes KEY=VALUE (an empty VALUE removes the setting)")
			}
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		patch["config"] = m
	}
	if len(patch) == 0 {
		return fmt.Errorf("nothing to change (see `grimoire bank help`)")
	}
	var p bankProfile
	if err := c.do("PATCH", bankPath(id), patch, &p); err != nil {
		return err
	}
	if f.on["--json"] {
		printJSON(p)
		return nil
	}
	printProfile(&p)
	return nil
}

func bankDelete(c *bankClient, f *bankFlags) error {
	id, err := f.need(0, "BANK")
	if err != nil {
		return err
	}
	if !f.on["--yes"] {
		return fmt.Errorf("deleting bank %s removes its documents and facts; pass --yes to confirm", id)
	}
	if err := c.do("DELETE", bankPath(id), nil, nil); err != nil {
		return err
	}
	fmt.Printf("deleted bank %s\n", id)
	return nil
}

// ---- retain ---------------------------------------------------------------------

type retainItem struct {
	Content    string            `json:"content"`
	DocumentID string            `json:"document_id,omitempty"`
	Timestamp  string            `json:"timestamp,omitempty"`
	Context    string            `json:"context,omitempty"`
	Tags       []string          `json:"tags,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	UpdateMode string            `json:"update_mode,omitempty"`
}

type retainDoc struct {
	DocumentID      string `json:"document_id"`
	Chunks          int    `json:"chunks"`
	ChunksExtracted int    `json:"chunks_extracted"`
	ChunksReused    int    `json:"chunks_reused"`
	Facts           int    `json:"facts"`
	FactsAdded      int    `json:"facts_added"`
	HumanKept       int    `json:"human_facts_kept"`
	Unchanged       bool   `json:"unchanged"`
}

type retainResult struct {
	BankID       string      `json:"bank_id"`
	ItemsCount   int         `json:"items_count"`
	Async        bool        `json:"async"`
	OperationID  string      `json:"operation_id"`
	OperationIDs []string    `json:"operation_ids"`
	Mode         string      `json:"mode"`
	Documents    []retainDoc `json:"documents"`
	BankCreated  bool        `json:"bank_created"`
	Usage        struct {
		Total int `json:"total_tokens"`
	} `json:"usage"`
}

// retainBatch sends items. With async it asks for a background operation and,
// when this server cannot queue one, says so and retains in the foreground.
func retainBatch(c *bankClient, bank string, items []retainItem, mode string, async bool) (*retainResult, error) {
	body := map[string]any{"items": items}
	if mode != "" {
		body["mode"] = mode
	}
	var res retainResult
	if async {
		body["async"] = true
		err := c.do("POST", bankPath(bank, "memories"), body, &res)
		var ae *apiError
		if err == nil || !errors.As(err, &ae) || ae.status != http.StatusBadRequest || !strings.Contains(ae.detail, "async") {
			return &res, err
		}
		fmt.Fprintln(os.Stderr, "note: this server cannot queue a retain yet; retaining in the foreground")
		delete(body, "async")
	}
	err := c.do("POST", bankPath(bank, "memories"), body, &res)
	return &res, err
}

// textFile reports whether a file looks like text worth retaining.
func textFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".txt", ".text", ".rst", ".org", ".json", ".jsonl", ".csv", ".log", ".html", ".htm", ".yaml", ".yml":
		return true
	}
	return false
}

func bankRetain(c *bankClient, f *bankFlags) error {
	bank, err := f.need(0, "BANK")
	if err != nil {
		return err
	}
	ts := f.str("--timestamp", "")
	base := retainItem{Context: f.str("--context", ""), Tags: f.list("--tags"),
		UpdateMode: f.str("--update-mode", "")}
	if ts != "mtime" {
		base.Timestamp = ts
	}
	docID := f.str("--document-id", "")
	var items []retainItem
	add := func(content, id string, mod time.Time) {
		it := base
		it.Content, it.DocumentID = content, id
		if ts == "mtime" && !mod.IsZero() {
			it.Timestamp = mod.UTC().Format(time.RFC3339)
		}
		items = append(items, it)
	}
	switch file, dir := f.str("--file", ""), f.str("--dir", ""); {
	case file != "" && dir != "":
		return fmt.Errorf("pass --file or --dir, not both")
	case file != "":
		raw, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		st, _ := os.Stat(file)
		id := docID
		if id == "" {
			id = filepath.Base(file)
		}
		add(string(raw), id, st.ModTime())
	case dir != "":
		if docID != "" {
			return fmt.Errorf("--document-id names one document; --dir names each by its path")
		}
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if p != dir && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !textFile(p) {
				return nil
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if strings.TrimSpace(string(raw)) == "" {
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			info, _ := d.Info()
			var mod time.Time
			if info != nil {
				mod = info.ModTime()
			}
			add(string(raw), filepath.ToSlash(rel), mod)
			return nil
		})
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return fmt.Errorf("no text files under %s", dir)
		}
	default:
		text := stdinOrArgs(f.pos[1:])
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("nothing to retain: pass TEXT, --file, --dir, or pipe it in")
		}
		add(text, docID, time.Time{})
	}
	mode := f.str("--mode", "")
	batch, err := f.int("--batch", 20)
	if err != nil || batch < 1 {
		batch = 20
	}
	var all []retainDoc
	var ops []string
	tokens := 0
	for start := 0; start < len(items); start += batch {
		end := min(start+batch, len(items))
		res, err := retainBatch(c, bank, items[start:end], mode, f.on["--async"])
		if err != nil {
			return err
		}
		all = append(all, res.Documents...)
		tokens += res.Usage.Total
		switch {
		case len(res.OperationIDs) > 0:
			ops = append(ops, res.OperationIDs...)
		case res.OperationID != "":
			ops = append(ops, res.OperationID)
		}
	}
	// --wait follows queued retains to the end and reports them like a
	// foreground one.
	var failed []string
	if f.on["--wait"] && len(ops) > 0 {
		timeout, err := waitTimeout(f)
		if err != nil {
			return err
		}
		for _, id := range ops {
			op, err := waitOp(c, bank, id, timeout)
			if err != nil {
				return err
			}
			if op.Status != "completed" {
				failed = append(failed, fmt.Sprintf("%s %s: %s", id, op.Status, orDash(op.Error)))
				continue
			}
			var r retainResult
			if json.Unmarshal(op.Result, &r) == nil {
				all = append(all, r.Documents...)
				tokens += r.Usage.Total
			}
		}
	}
	if f.on["--json"] {
		printJSON(map[string]any{"bank_id": bank, "documents": all, "operation_ids": ops, "total_tokens": tokens, "failed": failed})
		if len(failed) > 0 {
			return fmt.Errorf("%d operation(s) did not complete", len(failed))
		}
		return nil
	}
	for _, op := range ops {
		if f.on["--wait"] {
			fmt.Printf("operation %s finished\n", op)
		} else {
			fmt.Printf("queued operation %s (follow it with `grimoire bank ops wait %s %s`)\n", op, bank, op)
		}
	}
	for _, d := range all {
		state := fmt.Sprintf("%d facts (%d new), %d/%d chunks extracted", d.Facts, d.FactsAdded, d.ChunksExtracted, d.Chunks)
		if d.Unchanged {
			state = "unchanged"
		}
		if d.HumanKept > 0 {
			state += fmt.Sprintf(", %d of your facts kept", d.HumanKept)
		}
		fmt.Printf("%s: %s\n", orDash(d.DocumentID), state)
	}
	if tokens > 0 {
		fmt.Printf("model tokens: %d\n", tokens)
	}
	if len(failed) > 0 {
		return fmt.Errorf("retain did not complete: %s", strings.Join(failed, "; "))
	}
	return nil
}

// ---- recall -------------------------------------------------------------------

type recallFact struct {
	ID            string   `json:"id"`
	Text          string   `json:"text"`
	Type          string   `json:"type"`
	Entities      []string `json:"entities"`
	OccurredStart string   `json:"occurred_start"`
	OccurredEnd   string   `json:"occurred_end"`
	MentionedAt   string   `json:"mentioned_at"`
	DocumentID    string   `json:"document_id"`
	Tags          []string `json:"tags"`
	Authority     string   `json:"authority"`
	DisputedBy    string   `json:"disputed_by"`
	DocRemoved    bool     `json:"doc_removed"`
	Scores        struct {
		Final float64 `json:"final"`
	} `json:"scores"`
}

type recallTrace struct {
	Budget    int                       `json:"thinking_budget"`
	Arms      map[string][]struct{}     `json:"arms"`
	Ranks     map[string]map[string]int `json:"ranks"`
	Fused     int                       `json:"fused_candidates"`
	Reranked  bool                      `json:"reranked"`
	Tokens    int                       `json:"tokens_used"`
	TimingsMS map[string]float64        `json:"timings_ms"`
	Window    *struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"temporal_window"`
}

func bankRecall(c *bankClient, f *bankFlags) error {
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
	body := map[string]any{"query": query, "budget": f.str("--budget", "mid")}
	if _, ok := f.get("--max-tokens"); ok {
		n, err := f.int("--max-tokens", 4096)
		if err != nil {
			return err
		}
		body["max_tokens"] = n
	}
	if t := f.list("--types"); t != nil {
		body["types"] = t
	}
	if t := f.list("--tags"); t != nil {
		body["tags"] = t
		body["tags_match"] = f.str("--tags-match", "any")
	}
	if f.on["--chunks"] {
		body["include"] = map[string]any{"chunks": map[string]any{}}
	}
	trace := f.on["--trace"]
	body["trace"] = trace
	if f.on["--json"] {
		var raw json.RawMessage
		if err := c.do("POST", bankPath(bank, "memories", "recall"), body, &raw); err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	}
	var out struct {
		Results []recallFact `json:"results"`
		Chunks  map[string]struct {
			Text     string `json:"text"`
			Document string `json:"document_id"`
		} `json:"chunks"`
		Trace *recallTrace `json:"trace"`
	}
	if err := c.do("POST", bankPath(bank, "memories", "recall"), body, &out); err != nil {
		return err
	}
	if len(out.Results) == 0 {
		fmt.Println("nothing recalled")
	}
	for i, r := range out.Results {
		fmt.Printf("%2d. [%s] %s\n", i+1, strings.ToUpper(r.Type), r.Text)
		var meta []string
		if r.OccurredStart != "" {
			when := r.OccurredStart
			if r.OccurredEnd != "" && r.OccurredEnd != r.OccurredStart {
				when += " .. " + r.OccurredEnd
			}
			meta = append(meta, "occurred "+when)
		}
		if r.DocumentID != "" {
			meta = append(meta, "doc "+r.DocumentID)
		}
		if r.Authority == "human" {
			meta = append(meta, "yours")
		}
		if r.DisputedBy != "" {
			meta = append(meta, "disputed by "+r.DisputedBy)
		}
		if r.DocRemoved {
			meta = append(meta, "source removed")
		}
		meta = append(meta, fmt.Sprintf("score %.3f", r.Scores.Final), "id "+r.ID)
		fmt.Printf("    %s\n", strings.Join(meta, " · "))
	}
	for id, ch := range out.Chunks {
		fmt.Printf("\n--- chunk %s (%s)\n%s\n", id, ch.Document, ch.Text)
	}
	if t := out.Trace; t != nil {
		arms := make([]string, 0, len(t.Arms))
		for a, hits := range t.Arms {
			arms = append(arms, fmt.Sprintf("%s %d", a, len(hits)))
		}
		sort.Strings(arms)
		fmt.Printf("\ntrace: budget %d · arms %s · fused %d · reranked %v · %d tokens · %.1f ms\n",
			t.Budget, strings.Join(arms, ", "), t.Fused, t.Reranked, t.Tokens, t.TimingsMS["total"])
		if t.Window != nil {
			fmt.Printf("       time window %s .. %s\n", t.Window.Start, t.Window.End)
		}
	}
	return nil
}

// ---- memories, entities, documents ---------------------------------------------

func bankMemories(c *bankClient, f *bankFlags) error {
	sub, err := f.need(0, "ls|rm")
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
		for flag, key := range map[string]string{"--type": "type", "--document-id": "document_id", "--q": "q", "--limit": "limit", "--offset": "offset"} {
			if v, ok := f.get(flag); ok {
				q.Set(key, v)
			}
		}
		if f.on["--human"] {
			q.Set("authority", "human")
		}
		path := bankPath(bank, "memories")
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		var out struct {
			Items []recallFact `json:"items"`
			Total int          `json:"total"`
		}
		if err := c.do("GET", path, nil, &out); err != nil {
			return err
		}
		if f.on["--json"] {
			printJSON(out)
			return nil
		}
		for _, m := range out.Items {
			flag := " "
			if m.Authority == "human" {
				flag = "*"
			}
			if m.DisputedBy != "" {
				flag = "!"
			}
			fmt.Printf("%s %s  %-10s %s\n", flag, m.ID, m.Type, m.Text)
		}
		fmt.Printf("%d of %d facts  (* yours, ! disputed)\n", len(out.Items), out.Total)
		return nil
	case "rm", "delete":
		id, err := f.need(2, "fact ID")
		if err != nil {
			return err
		}
		path := bankPath(bank, "memories", url.PathEscape(id))
		if f.on["--force"] {
			path += "?force=true"
		}
		if err := c.do("DELETE", path, nil, nil); err != nil {
			return err
		}
		fmt.Printf("deleted %s\n", id)
		return nil
	}
	return fmt.Errorf("memories takes ls or rm")
}

func bankEntities(c *bankClient, f *bankFlags) error {
	bank, err := f.need(0, "BANK")
	if err != nil {
		return err
	}
	if len(f.pos) > 1 {
		name := strings.Join(f.pos[1:], " ")
		var d struct {
			ID       string `json:"entity_id"`
			Name     string `json:"canonical_name"`
			Mentions int    `json:"mention_count"`
			Related  []struct {
				Name     string `json:"canonical_name"`
				Mentions int    `json:"mention_count"`
			} `json:"related"`
			Facts []recallFact `json:"facts"`
		}
		if err := c.do("GET", bankPath(bank, "entities", url.PathEscape(name)), nil, &d); err != nil {
			return err
		}
		if f.on["--json"] {
			printJSON(d)
			return nil
		}
		fmt.Printf("%s (%s) · %d mentions\n", d.Name, d.ID, d.Mentions)
		if len(d.Related) > 0 {
			var rel []string
			for _, r := range d.Related {
				rel = append(rel, fmt.Sprintf("%s (%d)", r.Name, r.Mentions))
			}
			fmt.Printf("with: %s\n", strings.Join(rel, ", "))
		}
		for _, m := range d.Facts {
			fmt.Printf("  - %s\n", m.Text)
		}
		return nil
	}
	path := bankPath(bank, "entities")
	if q, ok := f.get("--q"); ok {
		path += "?q=" + url.QueryEscape(q)
	}
	var out struct {
		Items []struct {
			ID       string `json:"entity_id"`
			Name     string `json:"canonical_name"`
			Mentions int    `json:"mention_count"`
			LastSeen string `json:"last_seen"`
		} `json:"items"`
	}
	if err := c.do("GET", path, nil, &out); err != nil {
		return err
	}
	if f.on["--json"] {
		printJSON(out)
		return nil
	}
	for _, e := range out.Items {
		fmt.Printf("%5d  %s\n", e.Mentions, e.Name)
	}
	return nil
}

func bankDocuments(c *bankClient, f *bankFlags) error {
	bank, err := f.need(0, "BANK")
	if err != nil {
		return err
	}
	if len(f.pos) > 1 {
		id := f.pos[1]
		path := bankPath(bank, "documents", url.PathEscape(id))
		if f.on["--rm"] {
			if f.on["--force"] {
				path += "?force=true"
			}
			var out struct {
				Kept int `json:"human_facts_kept"`
			}
			if err := c.do("DELETE", path, nil, &out); err != nil {
				return err
			}
			fmt.Printf("deleted %s", id)
			if out.Kept > 0 {
				fmt.Printf(" (kept %d fact(s) a person wrote; --force removes them too)", out.Kept)
			}
			fmt.Println()
			return nil
		}
		var d struct {
			ID        string       `json:"document_id"`
			Timestamp string       `json:"timestamp"`
			Context   string       `json:"context"`
			Tags      []string     `json:"tags"`
			Content   string       `json:"content"`
			Path      string       `json:"path"`
			Facts     []recallFact `json:"facts"`
		}
		if err := c.do("GET", path, nil, &d); err != nil {
			return err
		}
		if f.on["--json"] {
			printJSON(d)
			return nil
		}
		fmt.Printf("document %s · %s · %s\ncontext: %s\ntags: %s\n\n%s\n\nfacts:\n", d.ID, d.Timestamp, d.Path,
			orDash(d.Context), orDash(strings.Join(d.Tags, ", ")), d.Content)
		for _, m := range d.Facts {
			fmt.Printf("  - %s\n", m.Text)
		}
		return nil
	}
	q := url.Values{}
	for flag, key := range map[string]string{"--limit": "limit", "--offset": "offset"} {
		if v, ok := f.get(flag); ok {
			q.Set(key, v)
		}
	}
	path := bankPath(bank, "documents")
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out struct {
		Items []struct {
			ID        string `json:"document_id"`
			Timestamp string `json:"timestamp"`
			Chunks    int    `json:"chunks"`
			Facts     int    `json:"facts"`
			Context   string `json:"context"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := c.do("GET", path, nil, &out); err != nil {
		return err
	}
	if f.on["--json"] {
		printJSON(out)
		return nil
	}
	for _, d := range out.Items {
		fmt.Printf("%-40s %-20s %3d chunks %4d facts  %s\n", d.ID, d.Timestamp, d.Chunks, d.Facts, d.Context)
	}
	fmt.Printf("%d of %d documents\n", len(out.Items), out.Total)
	return nil
}
