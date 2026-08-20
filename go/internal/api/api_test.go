package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/auth"
	"github.com/JeremiahM37/grimoire/go/internal/crdtstore"
	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/history"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
	"github.com/JeremiahM37/grimoire/go/internal/settings"
	gsync "github.com/JeremiahM37/grimoire/go/internal/sync"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

func testServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	root := t.TempDir()
	v, err := vault.New(root)
	if err != nil {
		t.Fatal(err)
	}
	gdir := filepath.Join(root, ".grimoire")
	database, err := db.Open(filepath.Join(gdir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	old := vault.Now
	vault.Now = func() time.Time { return time.Date(2026, 8, 14, 9, 0, 0, 0, time.Local) }
	t.Cleanup(func() { vault.Now = old })

	vaultSecrets := secrets.New(gdir)
	st := settings.New(gdir)
	ix := index.New(database, v, embed.Hash{})
	crdt := crdtstore.New(gdir)
	s := &Server{
		Index:    ix,
		Vault:    v,
		Settings: st,
		History:  history.New(gdir),
		Secrets:  vaultSecrets,
		Broker:   secrets.NewBroker(vaultSecrets, database),
		CRDT:     crdt,
		// no LLM configured: every AI path takes its deterministic fallback,
		// which is what keeps these tests hermetic
		AI:       ai.New(st, vaultSecrets.Get),
		Auth:     auth.New(database),
		Sync:     gsync.New(ix, v, crdt),
		DailyDir: "journal",
		InboxDir: "inbox",
	}
	ix.Spaces = s
	return s, s.Routes()
}

func do(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding %s: %v", w.Body.String(), err)
	}
}

func TestCreateReadUpdateDelete(t *testing.T) {
	_, h := testServer(t)

	w := do(t, h, "POST", "/api/notes", map[string]any{
		"title": "First Note", "body": "# First Note\n\nhello #tag\n"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	var created map[string]any
	decode(t, w, &created)
	path, _ := created["path"].(string)
	if path == "" {
		t.Fatal("no path returned")
	}

	w = do(t, h, "GET", "/api/notes/"+path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	decode(t, w, &got)
	if got["title"] != "First Note" {
		t.Errorf("title = %v", got["title"])
	}

	w = do(t, h, "PUT", "/api/notes/"+path, map[string]any{"body": "# First Note\n\nedited\n"})
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", w.Code, w.Body)
	}

	w = do(t, h, "DELETE", "/api/notes/"+path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	if w = do(t, h, "GET", "/api/notes/"+path, nil); w.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", w.Code)
	}
}

// An explicit path collision must be reported, not silently overwrite.
func TestCreateRejectsDuplicatePath(t *testing.T) {
	_, h := testServer(t)
	body := map[string]any{"path": "dup.md", "body": "one"}
	if w := do(t, h, "POST", "/api/notes", body); w.Code != http.StatusCreated {
		t.Fatalf("first create = %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/notes", body); w.Code != http.StatusConflict {
		t.Errorf("second create = %d, want 409", w.Code)
	}
}

// A title-derived slug collision auto-suffixes instead of failing.
func TestCreateAutoSuffixesTitleCollision(t *testing.T) {
	_, h := testServer(t)
	for i := 0; i < 3; i++ {
		w := do(t, h, "POST", "/api/notes", map[string]any{"title": "Meeting"})
		if w.Code != http.StatusCreated {
			t.Fatalf("create %d = %d: %s", i, w.Code, w.Body)
		}
	}
	w := do(t, h, "GET", "/api/notes", nil)
	var list []map[string]any
	decode(t, w, &list)
	if len(list) != 3 {
		t.Fatalf("got %d notes, want 3", len(list))
	}
	seen := map[string]bool{}
	for _, n := range list {
		p := n["path"].(string)
		if seen[p] {
			t.Errorf("duplicate path %s", p)
		}
		seen[p] = true
	}
}

func TestPinTogglesAndFloatsToTop(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{"path": "a.md", "body": "a"})
	do(t, h, "POST", "/api/notes", map[string]any{"path": "b.md", "body": "b"})

	w := do(t, h, "POST", "/api/notes/b.md/pin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("pin = %d: %s", w.Code, w.Body)
	}
	var pin map[string]any
	decode(t, w, &pin)
	if pin["pinned"] != true {
		t.Fatalf("pinned = %v", pin["pinned"])
	}

	w = do(t, h, "GET", "/api/notes", nil)
	var list []map[string]any
	decode(t, w, &list)
	if list[0]["path"] != "b.md" {
		t.Errorf("pinned note did not float to the top: %v", list)
	}

	w = do(t, h, "POST", "/api/notes/b.md/pin", nil)
	decode(t, w, &pin)
	if pin["pinned"] != false {
		t.Errorf("second pin should unpin, got %v", pin["pinned"])
	}
}

func TestRenameAndDuplicate(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{"path": "orig.md", "body": "# Orig\n\nbody"})

	w := do(t, h, "POST", "/api/notes/orig.md/rename", map[string]any{"new_path": "moved.md"})
	if w.Code != http.StatusOK {
		t.Fatalf("rename = %d: %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/api/notes/orig.md", nil); w.Code != http.StatusNotFound {
		t.Errorf("old path still resolves: %d", w.Code)
	}
	if w := do(t, h, "GET", "/api/notes/moved.md", nil); w.Code != http.StatusOK {
		t.Errorf("new path missing: %d", w.Code)
	}

	w = do(t, h, "POST", "/api/notes/moved.md/duplicate", nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("duplicate = %d: %s", w.Code, w.Body)
	}
}

func TestSearchAndTagsAndGraph(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "gw.md", "body": "# Gateway\n\nthe api gateway listens on port 8443 #infra"})
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "hub.md", "body": "# Hub\n\nsee [[Gateway]] #infra"})

	w := do(t, h, "GET", "/api/search?q=gateway", nil)
	var hits []map[string]any
	decode(t, w, &hits)
	if len(hits) == 0 {
		t.Error("search returned nothing")
	}

	w = do(t, h, "GET", "/api/tags", nil)
	var tags []map[string]any
	decode(t, w, &tags)
	if len(tags) == 0 || tags[0]["tag"] != "infra" {
		t.Errorf("tags = %v", tags)
	}

	w = do(t, h, "GET", "/api/graph", nil)
	var g map[string]any
	decode(t, w, &g)
	edges := g["edges"].([]any)
	if len(edges) != 1 {
		t.Errorf("graph edges = %v", edges)
	}
}

func TestFactsAndTasksAndComplete(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "ops.md",
		"body": "# Ops\n\nport:: 8443\n\n- [ ] open task\n- [x] done task\n"})

	w := do(t, h, "GET", "/api/facts?key=port", nil)
	var facts []map[string]string
	decode(t, w, &facts)
	if len(facts) != 1 || facts[0]["value"] != "8443" {
		t.Errorf("facts = %v", facts)
	}

	w = do(t, h, "GET", "/api/tasks", nil)
	var tasks []map[string]any
	decode(t, w, &tasks)
	if len(tasks) != 1 || tasks[0]["done"] != false {
		t.Errorf("open tasks = %v", tasks)
	}
	w = do(t, h, "GET", "/api/tasks?include_done=1", nil)
	decode(t, w, &tasks)
	if len(tasks) != 2 {
		t.Errorf("all tasks = %v", tasks)
	}

	w = do(t, h, "GET", "/api/complete?q=ops", nil)
	var comp []map[string]string
	decode(t, w, &comp)
	if len(comp) != 1 || comp[0]["stem"] != "ops" {
		t.Errorf("complete = %v", comp)
	}
}

func TestTagRenameAcrossNotes(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{"path": "x.md", "body": "# X\n\n#old and #older"})
	do(t, h, "POST", "/api/notes", map[string]any{"path": "y.md", "body": "# Y\n\n#old"})

	w := do(t, h, "POST", "/api/tags/rename", map[string]any{"old": "old", "new": "new"})
	if w.Code != http.StatusOK {
		t.Fatalf("rename = %d: %s", w.Code, w.Body)
	}
	var res map[string]any
	decode(t, w, &res)
	if res["notes"].(float64) != 2 {
		t.Errorf("renamed in %v notes, want 2", res["notes"])
	}

	w = do(t, h, "GET", "/api/notes/x.md", nil)
	var note map[string]any
	decode(t, w, &note)
	body := note["body"].(string)
	if !strings.Contains(body, "#new") {
		t.Errorf("tag not renamed: %q", body)
	}
	// the whole-tag boundary: #older must be untouched
	if !strings.Contains(body, "#older") {
		t.Errorf("#older was clobbered by the #old rename: %q", body)
	}
}

func TestHistorySnapshotsAndRestores(t *testing.T) {
	s, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{"path": "h.md", "body": "version one"})
	// the update path snapshots the previous body automatically — that is what
	// makes version history work without the user opting in
	do(t, h, "PUT", "/api/notes/h.md", map[string]any{"body": "version two"})
	_ = s

	w := do(t, h, "GET", "/api/notes/h.md/history", nil)
	var versions []map[string]any
	decode(t, w, &versions)
	if len(versions) == 0 {
		t.Fatal("no versions recorded")
	}
	id := versions[0]["id"].(string)

	w = do(t, h, "GET", "/api/notes/h.md/history/"+id, nil)
	var v map[string]string
	decode(t, w, &v)
	// bodies are compared modulo the trailing newline the serializer guarantees
	if strings.TrimRight(v["body"], "\n") != "version one" {
		t.Errorf("version body = %q", v["body"])
	}

	w = do(t, h, "POST", "/api/notes/h.md/history/"+id+"/restore", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("restore = %d: %s", w.Code, w.Body)
	}
	w = do(t, h, "GET", "/api/notes/h.md", nil)
	var note map[string]any
	decode(t, w, &note)
	if !strings.Contains(note["body"].(string), "version one") {
		t.Errorf("restore did not take: %v", note["body"])
	}
}

// A version id becomes part of a filesystem path, so it must be validated
// strictly rather than cleaned.
func TestHistoryRejectsPathTraversalInVersionID(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{"path": "h.md", "body": "x"})
	// The property that matters: a traversal must never reach the filesystem
	// and never return content. ServeMux path-cleans "../" into a redirect
	// before routing, which is equally safe, so accept either.
	for _, id := range []string{"..", "../../etc/passwd", "abc", "12", "1e5", "-1"} {
		w := do(t, h, "GET", "/api/notes/h.md/history/"+id, nil)
		if w.Code == http.StatusOK {
			t.Errorf("version id %q returned 200 with body %s", id, w.Body)
			continue
		}
		if w.Code != http.StatusNotFound && (w.Code < 300 || w.Code > 399) {
			t.Errorf("version id %q returned %d, want 404 or a redirect", id, w.Code)
		}
	}
}

func TestMemoryRememberAndRecall(t *testing.T) {
	_, h := testServer(t)
	w := do(t, h, "POST", "/api/memory", map[string]any{
		"text":  "the deploy service is owned by the platform team",
		"topic": "ownership", "agent": "claude", "task": "onboarding"})
	if w.Code != http.StatusCreated {
		t.Fatalf("remember = %d: %s", w.Code, w.Body)
	}
	var res map[string]any
	decode(t, w, &res)
	if res["created"] != true || res["path"] != "memory/ownership.md" {
		t.Errorf("remember = %v", res)
	}

	// a second memory on the same topic appends rather than replacing
	w = do(t, h, "POST", "/api/memory", map[string]any{
		"text": "it pages the on-call rota", "topic": "ownership", "agent": "claude"})
	decode(t, w, &res)
	if res["created"] != false {
		t.Errorf("second memory should append, got created=%v", res["created"])
	}

	w = do(t, h, "GET", "/api/memory?q=platform+team", nil)
	var mems []map[string]any
	decode(t, w, &mems)
	if len(mems) == 0 {
		t.Fatal("recall found nothing")
	}
	body := mems[0]["body"].(string)
	if !strings.Contains(body, "platform team") || !strings.Contains(body, "on-call rota") {
		t.Errorf("both memories should be in one note: %q", body)
	}
	if !strings.Contains(body, "claude") {
		t.Errorf("provenance attribution missing: %q", body)
	}
}

func TestMemoryRejectsBadAgentName(t *testing.T) {
	_, h := testServer(t)
	w := do(t, h, "POST", "/api/memory", map[string]any{
		"text": "x", "agent": "bad\nname<script>"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad agent name = %d, want 400", w.Code)
	}
	if w := do(t, h, "POST", "/api/memory", map[string]any{"text": ""}); w.Code != http.StatusBadRequest {
		t.Errorf("empty text = %d, want 400", w.Code)
	}
}

func TestDailyAndCapture(t *testing.T) {
	_, h := testServer(t)
	w := do(t, h, "GET", "/api/daily", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("daily = %d: %s", w.Code, w.Body)
	}
	var d map[string]string
	decode(t, w, &d)
	if !strings.HasPrefix(d["path"], "journal/") {
		t.Errorf("daily path = %q", d["path"])
	}

	w = do(t, h, "POST", "/api/capture", map[string]any{
		"text": "something worth keeping", "title": "Clip", "url": "https://example.com"})
	if w.Code != http.StatusCreated {
		t.Fatalf("capture = %d: %s", w.Code, w.Body)
	}
	var c map[string]string
	decode(t, w, &c)
	if !strings.HasPrefix(c["path"], "inbox/") {
		t.Errorf("capture path = %q", c["path"])
	}
	// the capture must be threaded into today's daily note, or it is orphaned
	w = do(t, h, "GET", "/api/notes/"+d["path"], nil)
	var daily map[string]any
	decode(t, w, &daily)
	if !strings.Contains(daily["body"].(string), "Clip") {
		t.Errorf("capture not linked from the daily note: %v", daily["body"])
	}

	w = do(t, h, "GET", "/api/daily/dates", nil)
	var dates []string
	decode(t, w, &dates)
	if len(dates) != 1 {
		t.Errorf("daily dates = %v", dates)
	}
}

func TestDailyRejectsBadDate(t *testing.T) {
	_, h := testServer(t)
	if w := do(t, h, "GET", "/api/daily?date=../escape", nil); w.Code != http.StatusBadRequest {
		t.Errorf("bad date = %d, want 400", w.Code)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	_, h := testServer(t)
	w := do(t, h, "GET", "/api/settings", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get settings = %d", w.Code)
	}
	w = do(t, h, "PUT", "/api/settings", map[string]string{"llm": "ollama"})
	if w.Code != http.StatusOK {
		t.Fatalf("put settings = %d: %s", w.Code, w.Body)
	}
	var state map[string]any
	decode(t, w, &state)
	if state["settings"].(map[string]any)["llm"] != "ollama" {
		t.Errorf("setting not persisted: %v", state)
	}
	if w := do(t, h, "PUT", "/api/settings", map[string]string{"llm": "bogus"}); w.Code != http.StatusBadRequest {
		t.Errorf("invalid llm = %d, want 400", w.Code)
	}
}

// The read surface must never serve a private note.
func TestReadExcludesPrivateNotes(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "secret.md", "body": "classified",
		"frontmatter": map[string]any{"private": true}})
	if w := do(t, h, "GET", "/read/secret", nil); w.Code != http.StatusNotFound {
		t.Errorf("private note served on /read: %d", w.Code)
	}
	do(t, h, "POST", "/api/notes", map[string]any{"path": "public.md", "body": "# Public\n\nopen"})
	w := do(t, h, "GET", "/read/public", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<h1") {
		t.Errorf("public note not rendered: %d %s", w.Code, w.Body.String()[:120])
	}
}

func TestUnlinkedMentions(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{"path": "Gateway.md", "body": "# Gateway\n\nthe thing"})
	do(t, h, "POST", "/api/notes", map[string]any{"path": "mentions.md",
		"body": "# Mentions\n\nwe route through Gateway every day"})
	do(t, h, "POST", "/api/notes", map[string]any{"path": "linked.md",
		"body": "# Linked\n\nsee [[Gateway]]"})

	w := do(t, h, "GET", "/api/notes/Gateway.md/unlinked", nil)
	var out []map[string]string
	decode(t, w, &out)
	if len(out) != 1 || out[0]["path"] != "mentions.md" {
		t.Errorf("unlinked mentions = %v (a note that already links should be excluded)", out)
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	s, h := testServer(t)
	_ = s
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "sensitive.md", "body": "# Sensitive\n\nthe launch code is hunter2\n"})

	w := do(t, h, "POST", "/api/notes/sensitive.md/encrypt", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("encrypt = %d: %s", w.Code, w.Body)
	}
	var note map[string]any
	decode(t, w, &note)
	if note["encrypted"] != true || note["private"] != true {
		t.Errorf("encrypted note should be private: %v", note)
	}
	// while the vault is UNLOCKED the API decrypts for display, so plaintext in
	// this response is correct. The real properties are: ciphertext on disk,
	// and a blanked body once the vault is locked.
	onDisk, err := os.ReadFile(filepath.Join(s.Vault.Root, "sensitive.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "hunter2") {
		t.Error("plaintext survived encryption ON DISK")
	}
	if !strings.Contains(string(onDisk), "grimoire:enc:v1:") {
		t.Error("encrypted note is not sealed on disk")
	}
	s.Secrets.Lock()
	w = do(t, h, "GET", "/api/notes/sensitive.md", nil)
	var locked map[string]any
	decode(t, w, &locked)
	if locked["locked"] != true || locked["body"] != "" {
		t.Errorf("a locked vault must blank the body, got %v", locked["body"])
	}
	if strings.Contains(w.Body.String(), "grimoire:enc:v1:") {
		t.Error("ciphertext leaked to the UI while locked")
	}
	if err := s.Secrets.Unlock("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	// an encrypted note must not be searchable by its content
	w = do(t, h, "GET", "/api/search?q=hunter2", nil)
	var hits []map[string]any
	decode(t, w, &hits)
	if len(hits) != 0 {
		t.Errorf("ciphertext note was findable by plaintext: %v", hits)
	}

	w = do(t, h, "POST", "/api/notes/sensitive.md/decrypt", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("decrypt = %d: %s", w.Code, w.Body)
	}
	decode(t, w, &note)
	if !strings.Contains(note["body"].(string), "hunter2") {
		t.Errorf("decrypt did not restore the body: %v", note["body"])
	}
}

func TestEncryptRequiresUnlockedVault(t *testing.T) {
	s, h := testServer(t)
	s.Secrets.Initialize("correct horse battery")
	do(t, h, "POST", "/api/notes", map[string]any{"path": "x.md", "body": "x"})
	s.Secrets.Lock()
	if w := do(t, h, "POST", "/api/notes/x.md/encrypt", nil); w.Code != http.StatusLocked {
		t.Errorf("encrypt while locked = %d, want 423", w.Code)
	}
}

func TestCRDTDocAndMerge(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{"path": "shared.md", "body": "hello world"})

	w := do(t, h, "GET", "/api/crdt/doc/shared.md", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get doc = %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	decode(t, w, &got)
	docJSON, _ := got["doc"].(string)
	if docJSON == "" {
		t.Fatal("no crdt doc returned")
	}

	// merging our own document back is a no-op, and must converge, not duplicate
	w = do(t, h, "POST", "/api/crdt/merge", map[string]any{
		"path": "shared.md", "doc": docJSON})
	if w.Code != http.StatusOK {
		t.Fatalf("merge = %d: %s", w.Code, w.Body)
	}
	var merged map[string]any
	decode(t, w, &merged)
	if !strings.Contains(merged["body"].(string), "hello world") {
		t.Errorf("merge lost content: %v", merged["body"])
	}
	if strings.Count(merged["body"].(string), "hello") != 1 {
		t.Errorf("merging a document with itself duplicated content: %q", merged["body"])
	}
}

// Merging into ciphertext would destroy the note.
func TestCRDTRefusesEncryptedNotes(t *testing.T) {
	s, h := testServer(t)
	s.Secrets.Initialize("correct horse battery")
	do(t, h, "POST", "/api/notes", map[string]any{"path": "enc.md", "body": "secret stuff"})
	do(t, h, "POST", "/api/notes/enc.md/encrypt", nil)

	if w := do(t, h, "GET", "/api/crdt/doc/enc.md", nil); w.Code != http.StatusConflict {
		t.Errorf("crdt doc for an encrypted note = %d, want 409", w.Code)
	}
	w := do(t, h, "POST", "/api/crdt/merge", map[string]any{
		"path": "enc.md", "doc": `{"site":"peer","clock":0,"atoms":[],"tombs":[]}`})
	if w.Code != http.StatusConflict {
		t.Errorf("merge into an encrypted note = %d, want 409", w.Code)
	}
}

func TestVaultAndGrantRoutes(t *testing.T) {
	_, h := testServer(t)
	w := do(t, h, "GET", "/api/vault/status", nil)
	var st map[string]any
	decode(t, w, &st)
	if st["initialized"] != false {
		t.Errorf("fresh vault status = %v", st)
	}
	if w := do(t, h, "POST", "/api/vault/init", map[string]string{"passphrase": "correct horse battery"}); w.Code != http.StatusOK {
		t.Fatalf("init = %d: %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/secrets", map[string]any{"name": "api-key", "value": "v"}); w.Code != http.StatusCreated {
		t.Fatalf("add secret = %d", w.Code)
	}
	// the listing must expose names only
	w = do(t, h, "GET", "/api/secrets", nil)
	if strings.Contains(w.Body.String(), `"v"`) || strings.Contains(w.Body.String(), "value") {
		t.Errorf("secret listing leaked a value: %s", w.Body)
	}
	w = do(t, h, "POST", "/api/secrets/api-key/grant", map[string]any{
		"grantee": "agent", "scope": "https://example.com", "ttl_seconds": 60})
	if w.Code != http.StatusOK {
		t.Fatalf("grant = %d: %s", w.Code, w.Body)
	}
	do(t, h, "POST", "/api/vault/lock", nil)
	if w := do(t, h, "GET", "/api/grants", nil); w.Code != http.StatusLocked {
		t.Errorf("grants while locked = %d, want 423", w.Code)
	}
}

func TestAttachAndServeFile(t *testing.T) {
	s, h := testServer(t)
	// write the attachment directly, then check it is served back
	p, err := s.Vault.SafeRawPath("attachments/pic.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("\x89PNG\r\n\x1a\nfake"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, "GET", "/api/file/attachments/pic.png", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "PNG") {
		t.Errorf("file not served: %d", w.Code)
	}
	// and traversal must not escape the vault
	for _, bad := range []string{"/api/file/../../etc/passwd", "/api/file/.grimoire/index.db"} {
		if w := do(t, h, "GET", bad, nil); w.Code == http.StatusOK {
			t.Errorf("%s was served", bad)
		}
	}
}

func TestCanvasCRUDAndValidation(t *testing.T) {
	_, h := testServer(t)
	good := map[string]any{
		"nodes": []any{map[string]any{"id": "a", "type": "text", "text": "hi"}},
		"edges": []any{},
	}
	if w := do(t, h, "PUT", "/api/canvas/board", good); w.Code != http.StatusOK {
		t.Fatalf("put canvas = %d: %s", w.Code, w.Body)
	}
	w := do(t, h, "GET", "/api/canvas/board", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"a"`) {
		t.Errorf("get canvas = %d %s", w.Code, w.Body)
	}
	w = do(t, h, "GET", "/api/canvas", nil)
	var list []map[string]string
	decode(t, w, &list)
	if len(list) != 1 {
		t.Errorf("canvas list = %v", list)
	}
	// structural validation: a node without a string id is refused
	bad := map[string]any{"nodes": []any{map[string]any{"id": 7}}}
	if w := do(t, h, "PUT", "/api/canvas/bad", bad); w.Code != http.StatusBadRequest {
		t.Errorf("invalid canvas accepted: %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/canvas/board", nil); w.Code != http.StatusNoContent {
		t.Errorf("delete canvas = %d", w.Code)
	}
}

func TestExportVaultProducesAZip(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{"path": "a.md", "body": "# A\n\nalpha"})
	w := do(t, h, "GET", "/api/export/vault", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d", w.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	if len(zr.File) != 1 || zr.File[0].Name != "a.md" {
		t.Errorf("zip contents = %v", zr.File)
	}
}

// A zip is untrusted input: "zip slip" entries must be refused, not cleaned.
func TestImportRefusesZipSlip(t *testing.T) {
	s, h := testServer(t)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"ok.md", "../escape.md", "../../etc/evil.md", ".grimoire/x.md"} {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte("# x\n\nbody"))
	}
	zw.Close()

	req := httptest.NewRequest("POST", "/api/import/vault", bytes.NewReader(buf.Bytes()))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	_ = s
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d: %s", w.Code, w.Body)
	}
	var res map[string]int
	decode(t, w, &res)
	if res["imported"] != 1 {
		t.Errorf("imported %d entries, want only the safe one", res["imported"])
	}
	if res["skipped"] != 3 {
		t.Errorf("skipped %d, want 3 refused entries", res["skipped"])
	}
	// nothing may have been written outside the vault root
	for _, escaped := range []string{
		filepath.Join(filepath.Dir(s.Vault.Root), "escape.md"),
		filepath.Join(s.Vault.Root, ".grimoire", "x.md"),
	} {
		if _, err := os.Stat(escaped); err == nil {
			t.Errorf("an escaping entry was written to %s", escaped)
		}
	}
}
