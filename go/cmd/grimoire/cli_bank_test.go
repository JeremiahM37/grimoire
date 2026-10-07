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
	vaultDir(t)
	for _, k := range []string{"GRIMOIRE_LLM", "GRIMOIRE_LLM_MODEL", "GRIMOIRE_OLLAMA_URL", "GRIMOIRE_AUTH_TOKEN", "GRIMOIRE_SESSION"} {
		t.Setenv(k, "")
	}
	e, err := newEnv(false)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(e.handler)
	t.Cleanup(func() { srv.Close(); e.close() })
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

func TestBankRetainSourcesAndAsyncFallback(t *testing.T) {
	base := bankServer(t)
	mustBank(t, "create", "docs", "--template", "plain-retrieval")
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.md"), []byte("Alpha ships on Fridays."), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("Beta needs a VPN."), 0o644)
	os.WriteFile(filepath.Join(dir, "image.png"), []byte("\x89PNG"), 0o644)
	os.WriteFile(filepath.Join(dir, ".git", "c.md"), []byte("hidden"), 0o644)
	out, errOut, code := runBank(t, "retain", "docs", "--dir", dir, "--async", "--timestamp", "mtime")
	if code != 0 {
		t.Fatalf("retain --dir = %d: %s %s", code, out, errOut)
	}
	if !strings.Contains(errOut, "retaining in the foreground") {
		t.Errorf("async fallback was silent: %q", errOut)
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

// The surfaces the server does not have yet must say so — and must not be
// confused with a bank that does not exist.
func TestBankNotYetSurfacesDegrade(t *testing.T) {
	bankServer(t)
	mustBank(t, "create", "b")
	for _, args := range [][]string{
		{"reflect", "b", "what happened"},
		{"observations", "b"},
		{"models", "ls", "b"},
		{"models", "refresh", "b", "project-context"},
		{"ops", "ls", "b"},
		{"ops", "cancel", "b", "op-1"},
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
	if out := mustBank(t, "templates"); !strings.Contains(out, "coding-agent") || !strings.Contains(out, "built in") {
		t.Errorf("templates = %q", out)
	}
}

// When the server does have templates and mental models, they are used.
func TestBankCreateUsesServerTemplates(t *testing.T) {
	vaultDir(t)
	var created []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/bank-templates":
			w.Write([]byte(`{"templates":[{"id":"t","name":"T","manifest":{"bank":{"mission":"server mission","config":{"enable_graph":"false"}},"mental_models":[{"id":"m1","name":"M","source_query":"q?"}]}}]}`))
		case "POST /api/banks":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["mission"] != "server mission" {
				http.Error(w, `{"detail":"template not applied"}`, 400)
				return
			}
			w.Write([]byte(`{"bank_id":"x"}`))
		case "POST /api/banks/x/mental-models":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			created = append(created, body["id"].(string))
			w.Write([]byte(`{"mental_model_id":"m1","operation_id":"op"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("GRIMOIRE_URL", srv.URL)
	if out := mustBank(t, "create", "x", "--template", "t"); !strings.Contains(out, "1 mental model") {
		t.Errorf("create = %q", out)
	}
	if strings.Join(created, ",") != "m1" {
		t.Errorf("models created = %v", created)
	}
	if _, errOut, code := runBank(t, "create", "x", "--template", "nope"); code == 0 || !strings.Contains(errOut, "have: t") {
		t.Errorf("unknown template = %q", errOut)
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
