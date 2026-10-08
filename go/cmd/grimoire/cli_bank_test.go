package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The bank commands are an HTTP client of a running server, so these run the
// real API handler on a temp vault behind an httptest server and point
// GRIMOIRE_URL at it — the command under test cannot tell it from `serve`.

func bankServer(t *testing.T) string {
	t.Helper()
	return bankServerWith(t, nil)
}

// bankServerWith is bankServer with the handler wrapped, to play an older
// server.
func bankServerWith(t *testing.T, wrap func(http.Handler) http.Handler) string {
	t.Helper()
	vaultDir(t)
	for _, k := range []string{"GRIMOIRE_LLM", "GRIMOIRE_LLM_MODEL", "GRIMOIRE_OLLAMA_URL", "GRIMOIRE_AUTH_TOKEN", "GRIMOIRE_SESSION"} {
		t.Setenv(k, "")
	}
	e, err := newEnv(false)
	if err != nil {
		t.Fatal(err)
	}
	h := e.handler
	if wrap != nil {
		h = wrap(h)
	}
	// Operations (async retain) run on the server's workers, as under serve.
	if err := e.server.Banks.StartWorkers(2); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); e.server.Banks.StopWorkers(); e.close() })
	t.Setenv("GRIMOIRE_URL", srv.URL)
	return srv.URL
}

// runBank runs `grimoire bank ARGS` and returns stdout, stderr and the code.
func runBank(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	oldErr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	out, code := runCmd(t, append([]string{"bank"}, args...)...)
	w.Close()
	os.Stderr = oldErr
	errOut, _ := io.ReadAll(r)
	return out, string(errOut), code
}

func mustBank(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, code := runBank(t, args...)
	if code != 0 {
		t.Fatalf("bank %v = %d\nstdout: %s\nstderr: %s", args, code, out, errOut)
	}
	return out
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

func TestParseBankFlags(t *testing.T) {
	f, err := parseBankFlags([]string{"b1", "some", "--tags", "a, b", "text", "--async", "--directive=one", "--directive", "two", "--", "--literal"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.pos, "|") != "b1|some|text|--literal" {
		t.Errorf("positional = %q", f.pos)
	}
	if got := f.list("--tags"); len(got) != 2 || got[1] != "b" {
		t.Errorf("tags = %q", got)
	}
	if !f.on["--async"] || len(f.values["--directive"]) != 2 {
		t.Errorf("switches/repeats lost: %+v", f)
	}
	if _, err := parseBankFlags([]string{"--mission"}); err == nil {
		t.Error("a valued flag with no value was accepted")
	}
}

func TestBankLifecycleOverHTTP(t *testing.T) {
	base := bankServer(t)
	if out := mustBank(t, "list"); !strings.Contains(out, "no banks yet") {
		t.Errorf("empty list = %q", out)
	}
	mustBank(t, "create", "notes", "--mission", "remember support calls", "--name", "Support")
	mustBank(t, "update", "notes", "--disposition", "4,2,5", "--directive", "Never guess a date",
		"--config", "retain_extraction_mode=chunks")
	var p bankProfile
	getJSON(t, base+"/api/banks/notes", &p)
	if p.Name != "Support" || p.Mission != "remember support calls" || p.Disposition != (disposition{4, 2, 5}) {
		t.Errorf("profile = %+v", p)
	}
	if len(p.Directives) != 1 || p.Config["retain_extraction_mode"] != "chunks" {
		t.Fatalf("directive/config not applied: %+v", p)
	}
	// Removing by id leaves the others.
	mustBank(t, "update", "notes", "--directive", "Quote the customer")
	mustBank(t, "update", "notes", "--remove-directive", p.Directives[0].ID)
	getJSON(t, base+"/api/banks/notes", &p)
	if len(p.Directives) != 1 || p.Directives[0].Text != "Quote the customer" {
		t.Errorf("directives after remove = %+v", p.Directives)
	}
	if _, _, code := runBank(t, "update", "notes", "--skepticism", "9"); code == 0 {
		t.Error("an out-of-range trait was accepted")
	}

	out := mustBank(t, "retain", "notes", "Dana said the migration slipped to June 2024.",
		"--document-id", "call-1", "--tags", "support", "--context", "phone call")
	if !strings.Contains(out, "call-1: 1 facts (1 new)") {
		t.Errorf("retain output = %q", out)
	}
	// The same content again changes nothing.
	if out := mustBank(t, "retain", "notes", "Dana said the migration slipped to June 2024.",
		"--document-id", "call-1", "--tags", "support", "--context", "phone call"); !strings.Contains(out, "0 new") && !strings.Contains(out, "unchanged") {
		t.Errorf("identical re-retain = %q", out)
	}
	out = mustBank(t, "recall", "notes", "when did the migration slip", "--trace", "--tags", "support")
	if !strings.Contains(out, "June 2024") || !strings.Contains(out, "trace: budget") {
		t.Errorf("recall = %q", out)
	}
	raw := mustBank(t, "recall", "notes", "migration", "--json")
	var rec struct {
		Results []recallFact `json:"results"`
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil || len(rec.Results) != 1 {
		t.Fatalf("recall --json = %q (%v)", raw, err)
	}
	if out := mustBank(t, "memories", "ls", "notes"); !strings.Contains(out, rec.Results[0].ID) || !strings.Contains(out, "1 of 1") {
		t.Errorf("memories ls = %q", out)
	}
	if out := mustBank(t, "documents", "notes"); !strings.Contains(out, "call-1") {
		t.Errorf("documents = %q", out)
	}
	if out := mustBank(t, "documents", "notes", "call-1"); !strings.Contains(out, "phone call") {
		t.Errorf("document = %q", out)
	}
	mustBank(t, "memories", "rm", "notes", rec.Results[0].ID)
	if out := mustBank(t, "memories", "ls", "notes"); !strings.Contains(out, "0 of 0") {
		t.Errorf("fact not deleted: %q", out)
	}
	if _, _, code := runBank(t, "delete", "notes"); code == 0 {
		t.Error("delete without --yes went through")
	}
	mustBank(t, "delete", "notes", "--yes")
	if _, errOut, code := runBank(t, "show", "notes"); code == 0 || !strings.Contains(errOut, "not found") || strings.Contains(errOut, "not available") {
		t.Errorf("deleted bank still shows: %d %q", code, errOut)
	}
}

func TestBankRetainSourcesAsync(t *testing.T) {
	base := bankServer(t)
	mustBank(t, "create", "docs", "--template", "plain-retrieval")
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.md"), []byte("Alpha ships on Fridays."), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("Beta needs a VPN."), 0o644)
	os.WriteFile(filepath.Join(dir, "image.png"), []byte("\x89PNG"), 0o644)
	os.WriteFile(filepath.Join(dir, ".git", "c.md"), []byte("hidden"), 0o644)
	out, errOut, code := runBank(t, "retain", "docs", "--dir", dir, "--async", "--wait", "--timestamp", "mtime")
	if code != 0 {
		t.Fatalf("retain --dir = %d: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "finished") || !strings.Contains(out, "a.md:") || strings.Contains(errOut, "foreground") {
		t.Errorf("async retain was not queued and followed: %q %q", out, errOut)
	}
	var docs struct {
		Items []struct {
			ID string `json:"document_id"`
		} `json:"items"`
	}
	getJSON(t, base+"/api/banks/docs/documents", &docs)
	var ids []string
	for _, d := range docs.Items {
		ids = append(ids, d.ID)
	}
	got := strings.Join(ids, ",")
	if !strings.Contains(got, "a.md") || !strings.Contains(got, "sub/b.txt") || len(ids) != 2 {
		t.Errorf("documents = %q (want a.md and sub/b.txt only)", got)
	}
	file := filepath.Join(dir, "a.md")
	if out := mustBank(t, "retain", "docs", "--file", file, "--document-id", "alpha"); !strings.Contains(out, "alpha:") {
		t.Errorf("retain --file = %q", out)
	}
	if _, _, code := runBank(t, "retain", "docs"); code == 0 {
		t.Error("retain with nothing to retain succeeded")
	}
	var p bankProfile
	getJSON(t, base+"/api/banks/docs", &p)
	if p.Config["retain_extraction_mode"] != "chunks" || p.Config["enable_graph"] != "false" {
		t.Errorf("template config not applied: %+v", p.Config)
	}
}

// olderServer plays a server from before reflect, observations, mental
// models, directives, operations and templates: those routes answer the mux's
// plain-text 404, and an asynchronous retain is refused.
func olderServer(h http.Handler) http.Handler {
	newer := []string{"/reflect", "/observations", "/consolidate", "/mental-models", "/directives",
		"/operations", "/webhooks", "/stats", "/export", "/import"}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/bank-templates") {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(p, "/api/banks/") {
			for _, n := range newer {
				if strings.Contains(p, n) {
					http.NotFound(w, r)
					return
				}
			}
		}
		if r.Method == "POST" && strings.HasSuffix(p, "/memories") {
			raw, _ := io.ReadAll(r.Body)
			if strings.Contains(string(raw), `"async":true`) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				w.Write([]byte(`{"detail":"async retain is not supported yet"}`))
				return
			}
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
		}
		h.ServeHTTP(w, r)
	})
}

// On an older server the newer surfaces must say so — and must not be
// confused with a bank that does not exist.
func TestBankOlderServerDegrades(t *testing.T) {
	base := bankServerWith(t, olderServer)
	mustBank(t, "create", "b")
	for _, args := range [][]string{
		{"reflect", "b", "what happened"},
		{"observations", "b"},
		{"observations", "consolidate", "b"},
		{"models", "ls", "b"},
		{"models", "tree", "b"},
		{"models", "refresh", "b", "project-context"},
		{"directives", "ls", "b"},
		{"ops", "ls", "b"},
		{"ops", "cancel", "b", "op-1"},
		{"stats", "b"},
		{"import", "b", "--template", "support"},
	} {
		out, errOut, code := runBank(t, args...)
		if code == 0 || !strings.Contains(errOut, "not available on this server") {
			t.Errorf("%v = %d %q %q", args, code, out, errOut)
		}
	}
	if _, errOut, code := runBank(t, "recall", "missing", "x"); code == 0 || strings.Contains(errOut, "not available") {
		t.Errorf("a missing bank read as a missing feature: %q", errOut)
	}
	out, errOut, _ := runBank(t, "create", "coder", "--template", "coding-agent")
	if !strings.Contains(out, "created bank coder") || !strings.Contains(errOut, "skipped") {
		t.Errorf("template models on an older server: %q %q", out, errOut)
	}
	var p bankProfile
	getJSON(t, base+"/api/banks/coder", &p)
	if p.Disposition.Literalism != 5 || len(p.Directives) != 1 || p.Config["consolidation"] != "auto" || p.Config["observations_mission"] != "" {
		t.Errorf("fallback template profile = %+v", p)
	}
	if out := mustBank(t, "templates"); !strings.Contains(out, "coding-agent") || !strings.Contains(out, "built in") {
		t.Errorf("templates = %q", out)
	}
	out, errOut, code := runBank(t, "retain", "b", "Alpha ships on Fridays.", "--async")
	if code != 0 || !strings.Contains(errOut, "retaining in the foreground") || !strings.Contains(out, "1 facts") {
		t.Errorf("async fallback = %d %q %q", code, out, errOut)
	}
}

// A template on a current server is imported: its mental models and
// directives are made by the server, and the command's overrides win.
func TestBankCreateImportsServerTemplate(t *testing.T) {
	base := bankServer(t)
	out := mustBank(t, "create", "coder", "--template", "coding-agent", "--mission", "my mission")
	if !strings.Contains(out, "created bank coder with 3 mental model(s) and 1 directive(s)") || !strings.Contains(out, "no language model") {
		t.Errorf("create = %q", out)
	}
	var p bankProfile
	getJSON(t, base+"/api/banks/coder", &p)
	if p.Mission != "my mission" || !strings.Contains(p.RetainMission, "technical decisions") || p.Config["observations_mission"] == "" {
		t.Errorf("profile = %+v", p)
	}
	if _, errOut, code := runBank(t, "create", "coder", "--template", "coding-agent"); code == 0 || !strings.Contains(errOut, "409") {
		t.Errorf("re-create = %d %q", code, errOut)
	}
	if _, errOut, code := runBank(t, "create", "x", "--template", "nope"); code == 0 || !strings.Contains(errOut, "have: assistant") {
		t.Errorf("unknown template = %q", errOut)
	}
	if out := mustBank(t, "templates", "show", "research"); !strings.Contains(out, `"question"`) || !strings.Contains(out, "Attribute claims") {
		t.Errorf("templates show = %q", out)
	}
	if out := mustBank(t, "templates"); strings.Contains(out, "built in") || !strings.Contains(out, "plain-retrieval") {
		t.Errorf("templates = %q", out)
	}
}

// The reasoning surfaces against the real routes, with no language model.
func TestBankReasoningSurfacesOverHTTP(t *testing.T) {
	base := bankServer(t)
	mustBank(t, "create", "kb", "--template", "plain-retrieval")
	mustBank(t, "retain", "kb", "Dana moved the launch to June because the vendor was late.", "--document-id", "d1")

	if out := mustBank(t, "reflect", "kb", "when is the launch", "--trace"); !strings.Contains(out, "no language model") ||
		!strings.Contains(out, "June") || !strings.Contains(out, "based on:") {
		t.Errorf("reflect = %q", out)
	}
	raw := mustBank(t, "reflect", "kb", "launch", "--json")
	var rr reflectResult
	if err := json.Unmarshal([]byte(raw), &rr); err != nil || rr.Mode != "extractive" || len(rr.BasedOn.Memories) == 0 || rr.Trace != nil {
		t.Errorf("reflect --json = %q (%v)", raw, err)
	}

	// Mental models: no model to write them, so refresh is refused clearly.
	if out := mustBank(t, "models", "create", "kb", "Launch", "--query", "When is the launch?", "--id", "launch"); !strings.Contains(out, "no language model") {
		t.Errorf("models create = %q", out)
	}
	if _, errOut, code := runBank(t, "models", "refresh", "kb", "launch"); code == 0 || !strings.Contains(errOut, "needs a language model") {
		t.Errorf("refresh without a model = %d %q", code, errOut)
	}
	mustBank(t, "models", "edit", "kb", "launch", "--body", "The launch is in June.")
	if out := mustBank(t, "models", "move", "kb", "launch", "--folder", "plans"); !strings.Contains(out, "launch → plans/launch") {
		t.Errorf("move = %q", out)
	}
	if out := mustBank(t, "models", "tree", "kb"); !strings.Contains(out, "plans/\n  Launch  (plans/launch)") {
		t.Errorf("tree = %q", out)
	}
	if out := mustBank(t, "models", "show", "kb", "plans/launch"); !strings.Contains(out, "The launch is in June.") || !strings.Contains(out, "edited by a person") {
		t.Errorf("show = %q", out)
	}
	if out := mustBank(t, "models", "ls", "kb"); !strings.Contains(out, "plans/launch") || !strings.Contains(out, "1 mental model") {
		t.Errorf("ls = %q", out)
	}
	if out := mustBank(t, "models", "export", "kb", "--markdown"); !strings.Contains(out, "The launch is in June.") {
		t.Errorf("export = %q", out)
	}
	mustBank(t, "models", "history", "kb", "plans/launch")
	if _, errOut, code := runBank(t, "models", "accept", "kb", "plans/launch"); code == 0 || strings.Contains(errOut, "not available") {
		t.Errorf("accept with no proposal = %d %q", code, errOut)
	}
	mustBank(t, "models", "rm", "kb", "plans/launch")

	// Directives.
	out := mustBank(t, "directives", "add", "kb", "Answer in one sentence.", "--name", "Short", "--priority", "2")
	if !strings.Contains(out, "Short") {
		t.Errorf("directive add = %q", out)
	}
	var dl struct {
		Items []directive `json:"items"`
	}
	getJSON(t, base+"/api/banks/kb/directives", &dl)
	if len(dl.Items) != 1 || dl.Items[0].Priority != 2 {
		t.Fatalf("directives = %+v", dl.Items)
	}
	did := dl.Items[0].ID
	mustBank(t, "directives", "set", "kb", did, "--inactive")
	if out := mustBank(t, "directives", "ls", "kb"); !strings.Contains(out, "0 directive") {
		t.Errorf("inactive still listed: %q", out)
	}
	if out := mustBank(t, "directives", "ls", "kb", "--all"); !strings.Contains(out, "inactive") {
		t.Errorf("--all = %q", out)
	}
	mustBank(t, "directives", "rm", "kb", did)

	// Observations, consolidation, operations, stats, export/import.
	if out := mustBank(t, "observations", "kb", "--history"); !strings.Contains(out, "0 observation(s)") {
		t.Errorf("observations = %q", out)
	}
	if _, errOut, code := runBank(t, "observations", "consolidate", "kb"); code == 0 || !strings.Contains(errOut, "language model") {
		t.Errorf("consolidate without a model = %d %q", code, errOut)
	}
	out = mustBank(t, "retain", "kb", "Bo prefers tea.", "--async")
	opID := ""
	for _, w := range strings.Fields(out) {
		if strings.HasPrefix(w, "op") && len(w) > 3 && opID == "" && w != "operation" {
			opID = w
		}
	}
	if opID == "" {
		t.Fatalf("no operation id in %q", out)
	}
	if out := mustBank(t, "ops", "wait", "kb", opID, "--timeout", "30s"); !strings.Contains(out, `"status": "completed"`) {
		t.Errorf("ops wait = %q", out)
	}
	if out := mustBank(t, "ops", "ls", "kb", "--type", "retain"); !strings.Contains(out, opID) || !strings.Contains(out, "completed") {
		t.Errorf("ops ls = %q", out)
	}
	if _, errOut, code := runBank(t, "ops", "cancel", "kb", opID); code == 0 || !strings.Contains(errOut, "already finished") {
		t.Errorf("cancel finished op = %d %q", code, errOut)
	}
	if out := mustBank(t, "stats", "kb"); !strings.Contains(out, "facts:          2") || !strings.Contains(out, "language model: false") {
		t.Errorf("stats = %q", out)
	}
	if out := mustBank(t, "export", "kb"); !strings.Contains(out, `"retain_extraction_mode": "chunks"`) {
		t.Errorf("export = %q", out)
	}
	if out := mustBank(t, "import", "kb", "--template", "support", "--dry-run"); !strings.Contains(out, "would import into kb") || !strings.Contains(out, "open-issues") {
		t.Errorf("import dry run = %q", out)
	}
	getJSON(t, base+"/api/banks/kb/directives", &dl)
	if len(dl.Items) != 0 {
		t.Errorf("a dry run wrote directives: %+v", dl.Items)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := filepath.Join(t.TempDir(), "My Repo")
	os.MkdirAll(dir, 0o755)
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com",
			"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	for i, msg := range []string{"Start the parser", "Switch to tabs because the team asked", "Cache the index"} {
		os.WriteFile(filepath.Join(dir, "f.txt"), []byte(strings.Repeat("line\n", 50*(i+1))), 0o644)
		git("add", ".")
		git("commit", "-q", "-m", msg, "--date", "2024-0"+string(rune('1'+i))+"-15T10:00:00Z")
	}
	return dir
}

func TestBankImportGitIsIdempotent(t *testing.T) {
	base := bankServer(t)
	repo := gitRepo(t)
	out := mustBank(t, "import-git", repo, "--diffs", "--max-diff-bytes", "120", "--mode", "chunks")
	if !strings.Contains(out, "3 commits read, 0 already in coding-agent:my-repo, 3 to retain") {
		t.Errorf("first import = %q", out)
	}
	var docs struct {
		Items []struct {
			ID        string `json:"document_id"`
			Timestamp string `json:"timestamp"`
		} `json:"items"`
	}
	getJSON(t, base+"/api/banks/coding-agent:my-repo/documents", &docs)
	if len(docs.Items) != 3 {
		t.Fatalf("documents = %+v", docs.Items)
	}
	for _, d := range docs.Items {
		if !strings.HasPrefix(d.ID, "git:") || len(d.ID) != len("git:")+40 {
			t.Errorf("document id %q is not git:<sha>", d.ID)
		}
		if !strings.HasPrefix(d.Timestamp, "2024-0") {
			t.Errorf("timestamp %q is not the author date", d.Timestamp)
		}
	}
	var doc struct {
		Content string            `json:"content"`
		Meta    map[string]string `json:"metadata"`
		Tags    []string          `json:"tags"`
	}
	getJSON(t, base+"/api/banks/coding-agent:my-repo/documents/"+docs.Items[0].ID, &doc)
	for _, want := range []string{"by Ada on", "Changed files:", "f.txt", "Diff:", "[diff truncated"} {
		if !strings.Contains(doc.Content, want) {
			t.Errorf("commit document lacks %q:\n%s", want, doc.Content)
		}
	}
	if doc.Meta["author"] != "Ada" || len(doc.Tags) != 1 || doc.Tags[0] != "source:git" {
		t.Errorf("metadata/tags = %+v %v", doc.Meta, doc.Tags)
	}
	var p bankProfile
	getJSON(t, base+"/api/banks/coding-agent:my-repo", &p)
	if p.Disposition.Literalism != 5 || !strings.Contains(p.RetainMission, "technical decisions") {
		t.Errorf("new repo bank did not start from the coding-agent template: %+v", p)
	}
	if out := mustBank(t, "import-git", repo); !strings.Contains(out, "3 already in") || !strings.Contains(out, "0 to retain") {
		t.Errorf("second import retained again: %q", out)
	}
	if out := mustBank(t, "import-git", repo, "--limit", "1", "--dry-run", "--force", "--bank", "other"); !strings.Contains(out, "1 to retain") {
		t.Errorf("dry run = %q", out)
	}
	var none struct {
		Banks []bankSummary `json:"banks"`
	}
	getJSON(t, base+"/api/banks", &none)
	for _, b := range none.Banks {
		if b.ID == "other" {
			t.Error("--dry-run created a bank")
		}
	}
}

func TestRepoBankNameAndDiffCap(t *testing.T) {
	for in, want := range map[string]string{"/x/My Repo": "coding-agent:my-repo", "/x/grimoire": "coding-agent:grimoire",
		"/x/__": "coding-agent:repo", "/x/a.b_c": "coding-agent:a.b_c"} {
		if got := repoBankName(in); got != want {
			t.Errorf("repoBankName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := repoBankName("/x/" + strings.Repeat("a", 100)); len(got) > 64 {
		t.Errorf("bank id too long: %d", len(got))
	}
	diff := "line one\nline two\nline three\n"
	if got := capDiff(diff, 14); !strings.HasPrefix(got, "line one\n[diff truncated: 9 of 29") {
		t.Errorf("capDiff = %q", got)
	}
	if capDiff(diff, 0) != diff || capDiff(diff, 100) != diff {
		t.Error("capDiff changed a diff within its limit")
	}
}
