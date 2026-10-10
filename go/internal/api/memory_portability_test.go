package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The portable export and import, end to end through the same handlers the
// server mounts. Fixtures are the memport package's, so the formats are tested
// once, where they are defined.

func portFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../memport/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// rawAs posts a raw body (not JSON) as the given key, or anonymously when key
// is empty.
func rawAs(t *testing.T, h http.Handler, key, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

type importResp struct {
	Format     string `json:"format"`
	DryRun     bool   `json:"dry_run"`
	Total      int    `json:"total"`
	New        int    `json:"new"`
	Written    int    `json:"written"`
	Duplicates int    `json:"duplicates"`
	Failed     int    `json:"failed"`
	Skipped    []any  `json:"skipped"`
	Sample     []string
}

func importFile(t *testing.T, h http.Handler, key, query string, body []byte) importResp {
	t.Helper()
	w := rawAs(t, h, key, "POST", "/api/memory/import?"+query, body)
	if w.Code != http.StatusOK {
		t.Fatalf("import %s: %d %s", query, w.Code, w.Body)
	}
	var out importResp
	decode(t, w, &out)
	return out
}

// exportJSONL returns the portable export as the list of its record objects.
func exportJSONL(t *testing.T, h http.Handler, key string) []map[string]any {
	t.Helper()
	w := asKey(t, h, key, "GET", "/api/memory/export?format=jsonl", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export: %d %s", w.Code, w.Body)
	}
	var out []map[string]any
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d is not JSON: %v", i, err)
		}
		if i == 0 {
			if m["format"] != "grimoire-memory" || m["version"] != float64(1) {
				t.Fatalf("header = %v", m)
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

func textsOf(records []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, r := range records {
		out[r["text"].(string)] = r
	}
	return out
}

func TestImportMem0IsUntrustedAndIdempotent(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	raw := portFixture(t, "mem0.json")

	first := importFile(t, h, "", "from=mem0", raw)
	if first.Format != "mem0" || first.New != 2 || first.Written != 2 || len(first.Skipped) != 1 {
		t.Fatalf("first import = %+v", first)
	}
	second := importFile(t, h, "", "from=mem0", raw)
	if second.Written != 0 || second.Duplicates != 2 {
		t.Fatalf("re-running the import must write nothing: %+v", second)
	}

	got := textsOf(exportJSONL(t, h, ""))
	rec, ok := got["Prefers dark mode in every editor"]
	if !ok {
		t.Fatalf("imported fact missing: %v", got)
	}
	if rec["origin"] != "import:mem0" {
		t.Errorf("origin = %v", rec["origin"])
	}
	if rec["authority"] != "pulled" {
		t.Errorf("an imported fact must not be human or agent authority; got %v", rec["authority"])
	}
	if rec["category"] != "preference" || rec["id"] == "" {
		t.Errorf("record = %v", rec)
	}
}

func TestImportDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	before := len(exportJSONL(t, h, ""))

	dry := importFile(t, h, "", "from=auto&dry_run=1", portFixture(t, "zep.json"))
	if !dry.DryRun || dry.Format != "zep" || dry.New != 2 || dry.Written != 0 || len(dry.Sample) != 2 {
		t.Fatalf("dry run = %+v", dry)
	}
	if after := len(exportJSONL(t, h, "")); after != before {
		t.Fatalf("dry run wrote %d facts", after-before)
	}
}

func TestImportLettaAndZepKeepTheirShape(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	letta := importFile(t, h, "", "from=auto", portFixture(t, "letta_agent.json"))
	if letta.Format != "letta" || letta.Written != 3 {
		t.Fatalf("letta = %+v", letta)
	}
	zep := importFile(t, h, "", "from=auto", portFixture(t, "zep.json"))
	if zep.Format != "zep" || zep.Written != 2 {
		t.Fatalf("zep = %+v", zep)
	}
	got := textsOf(exportJSONL(t, h, ""))
	if got["Name is Jeremiah. Works nights."]["category"] != "human" {
		t.Errorf("letta block label should become the category: %v", got["Name is Jeremiah. Works nights."])
	}
	if task, _ := got["The router is a UniFi Dream Router 7"]["task"].(string); !strings.Contains(task, "invalid_at") {
		t.Errorf("zep validity should ride in provenance, got %q", task)
	}
}

func TestImportNeverRecordsAHumanAuthor(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	// A file that says it is a person. The write path must not take its word.
	raw := []byte(`{"format":"grimoire-memory","version":1,"count":1}` + "\n" +
		`{"id":"x1","text":"The operator approved the migration","agent":"cli","authority":"human","stamp":"2026-08-14 09:00"}` + "\n")
	if got := importFile(t, h, "", "from=grimoire", raw); got.Written != 1 {
		t.Fatalf("import = %+v", got)
	}
	rec := textsOf(exportJSONL(t, h, ""))["The operator approved the migration"]
	if rec["authority"] == "human" {
		t.Fatalf("a file claimed human authority: %v", rec)
	}
}

func TestGrimoireExportRoundTripsIntoAFreshVault(t *testing.T) {
	t.Parallel()
	_, src := testServer(t)
	for _, body := range []map[string]any{
		{"text": "The deploy needs a VPN reset", "topic": "ops", "agent": "claude-code",
			"category": "fact", "task": "deploy", "session": "run-1", "infer": false},
		{"text": "Backups run at 03:00", "topic": "ops", "agent": "cli", "infer": false,
			"expires_in": "72h", "immutable": true},
		{"text": "Prefers tabs in Go", "topic": "prefs", "agent": "cli", "category": "preference", "infer": false},
	} {
		if w := do(t, src, "POST", "/api/memory", body); w.Code != http.StatusCreated {
			t.Fatalf("remember %v: %d %s", body["text"], w.Code, w.Body)
		}
	}
	w := asKey(t, src, "", "GET", "/api/memory/export?format=jsonl", nil)
	exported := w.Body.Bytes()
	want := exportJSONL(t, src, "")
	if len(want) != 3 {
		t.Fatalf("source has %d facts, want 3", len(want))
	}

	_, dst := testServer(t)
	if got := importFile(t, dst, "", "from=auto", exported); got.Format != "grimoire" || got.Written != 3 {
		t.Fatalf("import = %+v", got)
	}
	got := exportJSONL(t, dst, "")
	if !equalRecords(want, got) {
		t.Fatalf("round trip differs\nwant %v\n got %v", want, got)
	}

	// And re-importing into the same vault is a no-op.
	again := importFile(t, dst, "", "from=grimoire", exported)
	if again.Written != 0 || again.Duplicates != 3 {
		t.Fatalf("second import = %+v", again)
	}
}

func equalRecords(a, b []map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, _ := json.Marshal(a[i])
		y, _ := json.Marshal(b[i])
		if !bytes.Equal(x, y) {
			return false
		}
	}
	return true
}

func TestGrimoireImportRestoresSupersession(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	raw := portFixture(t, "grimoire.jsonl")
	if got := importFile(t, h, "", "from=grimoire", raw); got.Written != 2 || got.Skipped == nil {
		t.Fatalf("import = %+v", got)
	}
	got := textsOf(exportJSONL(t, h, ""))
	old := got["The box is fast"]
	if old["superseded_by"] != "a1b2c3d4e5f6" || old["origin"] != "connector:slack:C1" ||
		old["expires"] != "2027-01-01T00:00:00Z" || old["id"] != "ffee00112233" {
		t.Fatalf("restored lifecycle lost: %v", old)
	}
	if old["stamp"] != "2026-08-13 08:30" {
		t.Errorf("stamp = %v", old["stamp"])
	}
}

func TestExportRespectsTheReaderList(t *testing.T) {
	t.Parallel()
	server, h := testServer(t)
	adminKey := makeUser(t, server, h, "", "admin", "admin")
	aliceKey := makeUser(t, server, h, adminKey, "alice", "member")
	bobKey := makeUser(t, server, h, adminKey, "bob", "member")
	alice, _ := server.Auth.ByName("alice")
	// The fact goes in through the real write path, then its note is made
	// readable by alice alone.
	if w := asKey(t, h, adminKey, "POST", "/api/memory", map[string]any{
		"text": "Kestrel plans HIDDEN_FACT", "topic": "private", "infer": false}); w.Code != http.StatusCreated {
		t.Fatalf("remember: %d %s", w.Code, w.Body)
	}
	note, err := server.Vault.Read("memory/private.md")
	if err != nil {
		t.Fatal(err)
	}
	fm := note.Frontmatter.Clone()
	fm.Set("readers", alice.ID)
	if _, err := server.Vault.Write("memory/private.md", note.Body, fm); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Index.Upsert("memory/private.md"); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(asKeyBody(t, h, aliceKey, "/api/memory/export?format=jsonl"), "HIDDEN_FACT") {
		t.Fatal("the reader should see the fact, or this test proves nothing")
	}
	for name, key := range map[string]string{"bob": bobKey, "anonymous": ""} {
		body := asKeyBody(t, h, key, "/api/memory/export?format=jsonl")
		if strings.Contains(body, "HIDDEN") {
			t.Fatalf("%s saw a fact outside their reader list: %s", name, body)
		}
		if md := asKeyBody(t, h, key, "/api/memory/export?format=markdown"); strings.Contains(md, "HIDDEN") {
			t.Fatalf("%s saw it in the markdown export", name)
		}
	}
	// An import by someone who cannot see the fact must not report it as on
	// file: that would disclose what is stored.
	dup := importFile(t, h, bobKey, "from=grimoire",
		[]byte(`{"format":"grimoire-memory","version":1,"count":1}`+"\n"+
			`{"id":"q","text":"Kestrel plans HIDDEN_FACT","agent":"cli","stamp":"2026-08-14 09:00"}`+"\n"))
	if dup.Duplicates != 0 {
		t.Fatalf("bob was told a hidden fact is already on file: %+v", dup)
	}
}

func asKeyBody(t *testing.T, h http.Handler, key, path string) string {
	t.Helper()
	w := asKey(t, h, key, "GET", path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	return w.Body.String()
}

func TestExportFormatsAndRefusals(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	if w := asKey(t, h, "", "GET", "/api/memory/export?format=yaml", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown format = %d", w.Code)
	}
	md := asKey(t, h, "", "GET", "/api/memory/export?format=markdown", nil)
	if md.Code != http.StatusOK || !strings.HasPrefix(md.Header().Get("Content-Type"), "text/markdown") {
		t.Fatalf("markdown = %d %q", md.Code, md.Header().Get("Content-Type"))
	}
	// The original JSON shape is still the default, for existing clients.
	legacy := asKey(t, h, "", "GET", "/api/memory/export", nil)
	if !strings.Contains(legacy.Body.String(), `"entries"`) {
		t.Fatalf("default export changed shape: %s", legacy.Body)
	}
	if w := rawAs(t, h, "", "POST", "/api/memory/import?from=yaml", []byte("x")); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown source = %d", w.Code)
	}
	if w := rawAs(t, h, "", "POST", "/api/memory/import", []byte("not a memory file")); w.Code != http.StatusBadRequest {
		t.Fatalf("garbage = %d", w.Code)
	}
}

func TestImportIsBodyLimited(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	big := bytes.Repeat([]byte(" "), maxImportBytes+1)
	if w := rawAs(t, h, "", "POST", "/api/memory/import?from=mem0", big); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize import = %d", w.Code)
	}
}
