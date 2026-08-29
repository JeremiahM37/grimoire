package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/JeremiahM37/grimoire/go/internal/build"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/mcp"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// The terminal half of grimoire: quick capture, daily, search, ingest, export.
//
// Port of cli/grimoire.py. These work directly against the vault with no server
// running, which is what makes them fast and scriptable — `grimoire capture`
// from a shell alias has to be instant, and a background service is not a
// prerequisite for writing a note down.
//
// Commands that are really HTTP surfaces (search, export) run the SAME handlers
// the server does, through an in-process request, so the CLI can never drift
// from the API.

const usage = `grimoire — local-first AI-native notes

  grimoire new "Title" [body...]      create a note (body from args or stdin)
  grimoire daily [text...]            append to today's daily note (or open it)
  grimoire capture [text...]          quick capture → inbox + daily link
  grimoire search QUERY               full-text search the vault
  grimoire remember TEXT [--topic T] [--session S] [--category C]
                    [--expires-in 72h] [--immutable] [--verbatim] [--human]
                                      record a fact, reconciled against what is known
  grimoire recall [QUERY] [--agent A] [--session S] [--category C]
                  [--limit N] [--all] [--as-of RFC3339] [--why]
                                      what is currently believed (--all: and what was)
  grimoire forget PATH ID [--hard]    retract one fact (ids come from recall)
  grimoire challenges [--note P --uphold ID | --concede ID]
                                      facts your agents dispute, and how to settle them
  grimoire ls [--tag TAG]             list notes
  grimoire open PATH                  print a note
  grimoire doctor                     diagnose why an agent cannot see your notes
  grimoire reindex                    rebuild the search index
  grimoire ingest PATH [--into DIR]   bulk-import a folder of markdown/text
  grimoire import PATH [--into DIR] [--dry-run]
                                      import a ChatGPT or Claude conversations.json
  grimoire seed-demo                  write a small sample vault (first-run demo)
  grimoire fetch-model                pre-download the local embedding model
  grimoire export [--out DIR] [--published]
                                      static HTML export (--published: only
                                      notes marked publish: true)
  grimoire sync PEER_URL [--watch] [--interval N] [--token T]
  grimoire agent-setup [API_URL]      print MCP + agent-context setup
  grimoire serve [--port N]           run the web app + API (the default)
  grimoire user add NAME [--admin]    create an account (prompts for a password)
  grimoire user list                  list accounts
  grimoire user map BACKEND SUBJECT USER   a verified network identity signs in as USER
  grimoire user unmap BACKEND SUBJECT      remove that mapping
  grimoire user identities            list identity mappings
  grimoire user passwd NAME           change an account's password
  grimoire space add NAME PREFIX      create a shared space
  grimoire space list                 list spaces and their prefixes
  grimoire space member SPACE USER [--read]   grant access to a space
  grimoire backup [--out FILE]        archive the vault (notes, secrets, sync state)
  grimoire restore FILE [--into DIR]  restore an archive and rebuild the index
  grimoire audit [--denied] [--path P] [--user U] [--limit N]
                                      who opened which restricted document
  grimoire eval build|run|compare     measure retrieval on your own vault
  grimoire version                    print the build version

Env: GRIMOIRE_VAULT (default ~/notes)`

// commands is the table that turns a word into a call.
//
// A function, not a literal inside runCLI, so a test can compare its keys with
// the usage text without running anything: `grimoire backup` once shipped in a
// release as an unreachable function — written, tested by calling it directly,
// documented in the help — and simply never added here.
func commands() map[string]func([]string) int {
	return map[string]func([]string) int{
		"new": cmdNew, "daily": cmdDaily, "capture": cmdCapture,
		"search": cmdSearch, "ls": cmdLs, "open": cmdOpen,
		"remember": cmdRemember, "recall": cmdRecall, "forget": cmdForget,
		"challenges": cmdChallenges,
		"doctor":     cmdDoctor, "reindex": cmdReindex, "import": cmdImport, "ingest": cmdIngest, "seed-demo": cmdSeedDemo,
		"export": cmdExport, "sync": cmdSync, "agent-setup": cmdAgentSetup,
		"fetch-model": cmdFetchModel,
		"user":        cmdUser, "space": cmdSpace,
		"backup": cmdBackup, "restore": cmdRestore,
		"audit": cmdAudit, "eval": cmdEval,
	}
}

// runCLI handles a subcommand. It reports whether the arguments were a
// subcommand at all: anything else means "serve", which stays the default so
// an existing unit file keeps working untouched.
func runCLI(args []string) (handled bool, code int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Println(usage)
		return true, 0
	case "version", "--version", "-v":
		fmt.Println("grimoire " + build.String())
		return true, 0
	case "serve":
		return false, 0
	}
	// The ONE table. It used to be copied here, and the copy is how "backup"
	// shipped with a help line and no way to run it.
	cmds := commands()
	fn, ok := cmds[args[0]]
	if !ok {
		names := make([]string, 0, len(cmds))
		for k := range cmds {
			names = append(names, k)
		}
		sort.Strings(names)
		fmt.Fprintf(os.Stderr, "unknown command %q. Try: %s, serve\n",
			args[0], strings.Join(names, ", "))
		return true, 2
	}
	return true, fn(args[1:])
}

// ---- shared plumbing --------------------------------------------------------

// stdinOrArgs takes the text from the arguments, or from a pipe when there are
// none — so both `grimoire capture "note"` and `… | grimoire capture` work.
func stdinOrArgs(args []string) string {
	if len(args) > 0 {
		return strings.Join(args, " ")
	}
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
		if b, err := io.ReadAll(os.Stdin); err == nil {
			return string(b)
		}
	}
	return ""
}

func flagValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	return 2
}

// recorder captures a handler's response so a CLI command can reuse the exact
// handler the HTTP API serves.
type recorder struct {
	status int
	body   bytes.Buffer
	header http.Header
}

func (r *recorder) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(code int)        { r.status = code }

// call runs one in-process request against the server's own mux.
func (e *env) call(method, path string) (int, string) {
	return e.callBody(method, path, nil)
}

// callBody is call with a JSON request body, for the write surfaces.
func (e *env) callBody(method, path string, body any) (int, string) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 500, err.Error()
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, path, reader)
	if err != nil {
		return 500, err.Error()
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := &recorder{status: 200}
	e.handler.ServeHTTP(rec, req)
	return rec.status, rec.body.String()
}

// ---- commands ---------------------------------------------------------------

func cmdNew(args []string) int {
	if len(args) == 0 {
		return fail(`usage: grimoire new "Title" [body...]`)
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	title := args[0]
	body := stdinOrArgs(args[1:])
	if body == "" {
		body = "# " + title + "\n\n"
	}
	rel := vault.Slugify(title) + ".md"
	fm := markdown.NewFrontmatter()
	fm.Set("title", title)
	if _, err := e.vault.Write(rel, body, fm); err != nil {
		return fail("%v", err)
	}
	if _, err := e.index.Upsert(rel); err != nil {
		return fail("%v", err)
	}
	fmt.Println(rel)
	return 0
}

func cmdDaily(args []string) int {
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	day := vault.Now().Format("2006-01-02")
	rel := e.dailyDir + "/" + day + ".md"
	if p, err := e.vault.SafePath(rel); err == nil {
		if _, statErr := os.Stat(p); statErr != nil {
			fm := markdown.NewFrontmatter()
			fm.Set("title", day)
			fm.Set("tags", []markdown.Value{"daily"})
			if _, err := e.vault.Write(rel, "# "+day+"\n\n", fm); err != nil {
				return fail("%v", err)
			}
		}
	}
	text := stdinOrArgs(args)
	if text == "" {
		p, _ := e.vault.SafePath(rel)
		fmt.Println(p)
		return 0
	}
	note, err := e.vault.Read(rel)
	if err != nil {
		return fail("%v", err)
	}
	body := strings.TrimRight(note.Body, " \t\r\n") + "\n- " + text + "\n"
	if _, err := e.vault.Write(rel, body, note.Frontmatter); err != nil {
		return fail("%v", err)
	}
	if _, err := e.index.Upsert(rel); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("appended to %s\n", rel)
	return 0
}

func cmdCapture(args []string) int {
	text := stdinOrArgs(args)
	if strings.TrimSpace(text) == "" {
		return fail("nothing to capture")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	stamp := vault.Now().Format("20060102-150405")
	rel := e.inboxDir + "/" + stamp + ".md"
	fm := markdown.NewFrontmatter()
	fm.Set("title", "capture "+stamp)
	fm.Set("tags", []markdown.Value{"capture"})
	if _, err := e.vault.Write(rel, text, fm); err != nil {
		return fail("%v", err)
	}
	if _, err := e.index.Upsert(rel); err != nil {
		return fail("%v", err)
	}
	fmt.Println(rel)
	return 0
}

func cmdSearch(args []string) int {
	if len(args) == 0 {
		return fail("usage: grimoire search QUERY")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	// the server's own handler, so operators (tag:, is:pinned, path:) and the
	// any-term fallback behave identically here
	status, body := e.call("GET", "/api/search?q="+url.QueryEscape(strings.Join(args, " ")))
	if status != http.StatusOK {
		return fail("search failed: %s", body)
	}
	var hits []struct{ Path, Title string }
	if err := json.Unmarshal([]byte(body), &hits); err != nil {
		return fail("%v", err)
	}
	for _, h := range hits {
		fmt.Printf("%-40s  %s\n", h.Path, h.Title)
	}
	return 0
}

func cmdLs(args []string) int {
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	query := "SELECT path, title FROM notes ORDER BY updated DESC"
	var params []any
	if tag, ok := flagValue(args, "--tag"); ok {
		query = "SELECT n.path, n.title FROM notes n JOIN tags t ON t.note=n.path " +
			"WHERE t.tag=? ORDER BY n.updated DESC"
		params = append(params, tag)
	}
	rows, err := e.index.DB.Query(query, params...)
	if err != nil {
		return fail("%v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var path, title string
		if rows.Scan(&path, &title) == nil {
			fmt.Printf("%-40s  %s\n", path, title)
		}
	}
	if err := rows.Err(); err != nil {
		return fail("%v", err)
	}
	return 0
}

func cmdOpen(args []string) int {
	if len(args) == 0 {
		return fail("usage: grimoire open PATH")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	note, err := e.vault.Read(args[0])
	if err != nil {
		return fail("%v", err)
	}
	fmt.Print(note.Raw)
	return 0
}

func cmdReindex([]string) int {
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	// `reindex` stays a FULL rebuild: it is the escape hatch you reach for
	// when you believe the index is wrong, and an incremental pass that
	// believes the same stale rows would be no escape at all. Startup syncs
	// incrementally instead.
	n, err := e.index.Reindex()
	if err != nil {
		return fail("%v", err)
	}
	if err := e.index.RecordSignature(); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("indexed %d notes\n", n)
	return 0
}

// cmdIngest bulk-imports an existing folder of markdown/text — the cold-start
// fix, so a new user's agent has something to retrieve on day one.
func cmdIngest(args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		return fail("usage: grimoire ingest PATH [--into SUBDIR]")
	}
	src := args[0]
	if strings.HasPrefix(src, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			src = filepath.Join(home, src[2:])
		}
	}
	into := ""
	if v, ok := flagValue(args, "--into"); ok {
		into = strings.Trim(v, "/")
	}
	info, err := os.Stat(src)
	if err != nil {
		return fail("no such path: %s", src)
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	exts := map[string]bool{".md": true, ".markdown": true, ".mdown": true, ".txt": true}
	var files []string
	if info.IsDir() {
		_ = filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				files = append(files, p)
			}
			return nil
		})
		sort.Strings(files)
	} else {
		files = []string{src}
	}

	n := 0
	for _, p := range files {
		if !exts[strings.ToLower(filepath.Ext(p))] || hasHiddenSegment(p) {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		base := filepath.Base(p)
		if info.IsDir() {
			if r, err := filepath.Rel(src, p); err == nil {
				base = r
			}
		}
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		segs := strings.Split(filepath.ToSlash(stem), "/")
		for i, s := range segs {
			segs[i] = vault.Slugify(s)
		}
		rel := strings.Join(segs, "/") + ".md"
		if into != "" {
			rel = into + "/" + rel
		}
		note := vault.NoteFromText(rel, string(raw), 0)
		fm := note.Frontmatter
		if fm == nil {
			fm = markdown.NewFrontmatter()
		}
		if _, ok := fm.Get("title"); !ok {
			plain := strings.TrimSpace(strings.NewReplacer("-", " ", "_", " ").Replace(
				strings.TrimSuffix(filepath.Base(base), filepath.Ext(base))))
			fm.Set("title", plain)
		}
		if _, err := e.vault.Write(rel, note.Body, fm); err != nil {
			continue
		}
		if _, err := e.index.Upsert(rel); err != nil {
			continue
		}
		n++
	}
	fmt.Printf("ingested %d file(s) into %s\n", n, e.vault.Root)
	return 0
}

func hasHiddenSegment(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if strings.HasPrefix(seg, ".") && seg != "." && seg != ".." {
			return true
		}
	}
	return false
}

// demoNote is one sample note in the first-run vault.
type demoNote struct {
	rel, title, body string
	extra            map[string]markdown.Value
}

// demo shows the agent loop — retrieval, a fact, a provenance-stamped memory —
// with zero setup, because an empty vault demonstrates nothing.
var demo = []demoNote{
	{"deployment-runbook.md", "Deployment Runbook",
		"# Deployment Runbook\n\nport:: 8443\nowner:: platform-team\n\n" +
			"## Rolling back a bad deploy\n1. `docker compose rollback` pins the previous image.\n" +
			"2. If the proxy still 502s, the namespaces are stale — do a full " +
			"`--force-recreate`, not a plain restart.\n3. Confirm on [[Monitoring]].\n",
		map[string]markdown.Value{"pinned": true, "tags": []markdown.Value{"ops"}}},
	{"monitoring.md", "Monitoring",
		"# Monitoring\n\nGrafana fronts Prometheus. Alerts page on error rate " +
			"> 2% for 5 minutes. The VPN tunnel MTU is pinned to 1280.\n",
		map[string]markdown.Value{"tags": []markdown.Value{"ops"}}},
	{"team-onboarding.md", "Team Onboarding",
		"# Team Onboarding\n\nCopy `.env.example` to `.env`, add the CA cert, and " +
			"ask the knowledge base before pinging the team.\n",
		map[string]markdown.Value{"tags": []markdown.Value{"onboarding"}}},
	{"memory/deploy-quirks.md", "Memory: deploy quirks",
		"The staging deploy needs `--force-recreate` after any VPN change — a " +
			"plain restart leaves stale namespaces and outbound calls black-hole.\n",
		map[string]markdown.Value{"memory": true, "agent": "claude-code",
			"task": "debug-session", "tags": []markdown.Value{"ops"}}},
}

func cmdSeedDemo([]string) int {
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	for _, d := range demo {
		fm := markdown.NewFrontmatter()
		fm.Set("title", d.title)
		for k, v := range d.extra {
			fm.Set(k, v)
		}
		if _, err := e.vault.Write(d.rel, d.body, fm); err != nil {
			return fail("%v", err)
		}
		if _, err := e.index.Upsert(d.rel); err != nil {
			return fail("%v", err)
		}
	}
	fmt.Printf("seeded %d demo notes into %s — try: grimoire search deploy\n",
		len(demo), e.vault.Root)
	return 0
}

// cmdFetchModel pre-downloads the local embedding model. Serving does this on
// first start, so this exists for the cases where that is the wrong moment: a
// container image built ahead of time, or a host that will be offline later.
func cmdFetchModel(args []string) int {
	name := "minishlab/potion-base-8M"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = args[0]
	}
	if dir := embed.FindModel(name); dir != "" {
		fmt.Printf("%s already present at %s\n", name, dir)
		return 0
	}
	fmt.Printf("downloading %s …\n", name)
	dir, err := embed.FetchModel(name)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("%s ready at %s\n", name, dir)
	return 0
}

// cmdExport writes a static HTML copy of the vault (e-ink / offline archive).
func cmdExport(args []string) int {
	out := "grimoire-export"
	if v, ok := flagValue(args, "--out"); ok {
		out = v
	}
	// --published cuts the site rather than the vault: only the notes whose
	// author wrote publish: true, rendered against a link map of those notes
	// only. It is the same subset the /published surface serves, taken through
	// the same handlers, so a static copy cannot include something the live
	// site would refuse.
	published := hasFlag(args, "--published")
	indexPath, notePrefix := "/read", "/read/"
	query := "SELECT path FROM notes WHERE private=0 ORDER BY path"
	if published {
		indexPath, notePrefix = "/published", "/published/"
		query = "SELECT path FROM notes WHERE private=0 AND " +
			`frontmatter_json LIKE '%"publish": true%' ORDER BY path`
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	if published {
		// The surface does not exist unless publishing is on, and an export
		// that silently produced an empty site would look like "you have
		// published nothing".
		if !truthyFlag(e.settings.Get("publish")) {
			return fail("publishing is off — set GRIMOIRE_PUBLISH=1 to export the published site")
		}
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fail("%v", err)
	}
	status, body := e.call("GET", indexPath)
	if status != http.StatusOK {
		return fail("export index failed: %s", body)
	}
	if err := os.WriteFile(filepath.Join(out, "index.html"), []byte(body), 0o644); err != nil {
		return fail("%v", err)
	}

	// collect the paths BEFORE rendering: each render runs a handler that
	// queries the same database, and holding a cursor open across those
	// queries deadlocks against the index writer
	rows, err := e.index.DB.Query(query)
	if err != nil {
		return fail("%v", err)
	}
	var paths []string
	for rows.Next() {
		var path string
		if rows.Scan(&path) == nil {
			paths = append(paths, path)
		}
	}
	if err := rows.Err(); err != nil {
		return fail("%v", err)
	}
	rows.Close()

	n := 0
	for _, path := range paths {
		stem := strings.TrimSuffix(path, ".md")
		status, html := e.call("GET", notePrefix+(&url.URL{Path: stem}).EscapedPath())
		if status != http.StatusOK {
			continue
		}
		dest := filepath.Join(out, stem+".html")
		if os.MkdirAll(filepath.Dir(dest), 0o755) != nil {
			continue
		}
		if os.WriteFile(dest, []byte(html), 0o644) != nil {
			continue
		}
		n++
	}
	what := "notes"
	if published {
		what = "published notes"
	}
	fmt.Printf("exported %d %s to %s/ (open index.html)\n", n, what, out)
	return 0
}

// truthyFlag reads the on/off settings the CLI consults.
func truthyFlag(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func cmdSync(args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		return fail("usage: grimoire sync PEER_URL [--watch] [--interval N] [--token T]")
	}
	peer := args[0]
	token, _ := flagValue(args, "--token")
	interval := 60
	if v, ok := flagValue(args, "--interval"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			interval = n
		}
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	if _, err := e.index.Reindex(); err != nil {
		return fail("%v", err)
	}
	for {
		st, err := e.sync.SyncWithPeer(peer, "cli", token)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sync error: %v\n", err)
		} else {
			fmt.Printf("synced %s: pulled %d, pushed %d, conflicts %d\n",
				peer, st.Pulled, st.Pushed, st.Conflicts)
		}
		if !hasFlag(args, "--watch") {
			return 0
		}
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

const agentSnippet = `## Team knowledge base (Grimoire)

This project has a Grimoire context server: runbooks, conventions, ticket
decisions, and agent memory, exposed through the ` + "`grimoire`" + ` MCP tools.

- Call ` + "`get_briefing`" + ` once before starting work (pinned notes, onboarding
  rules, recent agent memories).
- Before assuming any project-specific fact or picking an approach, check
  ` + "`search_notes`" + ` / ` + "`ask_notes`" + ` / ` + "`recall`" + ` — the team records accepted fixes
  that are not visible in the code.
- Persist anything future agents need with ` + "`remember`" + `.
`

const reflectHook = `#!/usr/bin/env python3
# Grimoire reflection hook (Claude Code Stop hook): before an agent session
# ends, ask ONCE whether anything durable was learned — and if so, persist it
# via the grimoire ` + "`remember`" + ` tool. Idempotent: allows the stop on the second
# pass so agents are never trapped.
import json, sys
data = json.load(sys.stdin)
if data.get("stop_hook_active"):
    sys.exit(0)   # already reflected once — allow the stop
print(json.dumps({
    "decision": "block",
    "reason": ("Before finishing: did this session teach you anything a future "
               "agent would need — root causes, gotchas, decisions, environment "
               "rules? If yes, record it with the grimoire ` + "`remember`" + ` tool "
               "(topic it well). If nothing is worth keeping, just finish.")}))
`

const hookSettings = `{
  "hooks": {
    "Stop": [
      { "hooks": [ { "type": "command",
                     "command": "python3 .claude/grimoire-reflect.py" } ] }
    ]
  }
}
`

// cmdAgentSetup prints everything needed to make agents DISCOVER the knowledge
// base. Discoverability is a deployment concern: agents reliably read project
// context files, and only sometimes browse tool lists.
func cmdAgentSetup(args []string) int {
	apiURL := "http://localhost:9111"
	if len(args) > 0 {
		apiURL = args[0]
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "grimoire-mcp"
	} else {
		self = filepath.Join(filepath.Dir(self), "grimoire-mcp")
	}
	cfg := map[string]any{"mcpServers": map[string]any{"grimoire": map[string]any{
		"command": self,
		"env": map[string]string{
			mcp.EnvURL: apiURL, mcp.EnvAgentName: "my-agent"}}}}
	raw, _ := json.MarshalIndent(cfg, "", "  ")

	fmt.Println("# 1. MCP config (e.g. .mcp.json), or register at user scope for headless runs:")
	fmt.Println(string(raw))
	fmt.Println()
	fmt.Println("# 2. Add to the repo's CLAUDE.md / AGENTS.md so agents consult the KB:")
	fmt.Print(agentSnippet)
	fmt.Println("# 3. Optional but measured to matter: a reflection hook so agents")
	fmt.Println("#    RECORD what they learn before finishing (benchmarked: without it,")
	fmt.Println("#    agents solve tasks and write nothing). Save as")
	fmt.Println("#    .claude/grimoire-reflect.py + merge into .claude/settings.json:")
	fmt.Print(reflectHook)
	fmt.Print(hookSettings)
	return 0
}
