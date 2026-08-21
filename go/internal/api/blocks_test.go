package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The lines inside notes, over HTTP. The parsing is tested in
// internal/markdown and the selection in internal/index; these are about the
// wiring — and about the fact that a block is note content, so every rule that
// governs reading a note governs reading a line of one.

func planVault(t *testing.T, h http.Handler) {
	t.Helper()
	for _, note := range []map[string]any{
		{"title": "Rollout", "body": "# Rollout\n\n- prep the box\n- [ ] drain the queue\n" +
			"- [x] take a backup\n\n## Risks\n\n- [ ] the disk might fill\n"},
		{"title": "Other", "body": "# Other\n\n- [ ] an unrelated task\n"},
	} {
		if w := do(t, h, "POST", "/api/notes", note); w.Code != http.StatusCreated {
			t.Fatalf("seed = %d: %s", w.Code, w.Body)
		}
	}
}

func blockTexts(t *testing.T, h http.Handler, query string) []string {
	t.Helper()
	w := do(t, h, "GET", "/api/blocks"+query, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("blocks%s = %d: %s", query, w.Code, w.Body)
	}
	var got []map[string]any
	decode(t, w, &got)
	out := make([]string, len(got))
	for i, b := range got {
		out[i], _ = b["Text"].(string)
		if out[i] == "" {
			out[i], _ = b["text"].(string)
		}
	}
	return out
}

func TestBlocksEndpointFilters(t *testing.T) {
	_, h := testServer(t)
	planVault(t, h)

	cases := map[string][]string{
		"?kind=heading":            {"Rollout", "Risks", "Other"},
		"?kind=task&checked=false": {"drain the queue", "the disk might fill", "an unrelated task"},
		"?kind=task&checked=true":  {"take a backup"},
		"?kind=item":               {"prep the box"},
		"?kind=heading&level=2":    {"Risks"},
		"?section=Risks":           {"the disk might fill"},
		"?q=disk":                  {"the disk might fill"},
		"?note=other.md&kind=task": {"an unrelated task"},
	}
	for query, want := range cases {
		got := blockTexts(t, h, query)
		if len(got) != len(want) {
			t.Errorf("%s returned %v, want %v", query, got, want)
			continue
		}
		set := map[string]bool{}
		for _, g := range got {
			set[g] = true
		}
		for _, wtext := range want {
			if !set[wtext] {
				t.Errorf("%s returned %v, missing %q", query, got, wtext)
			}
		}
	}
}

func TestBlocksEndpointValidation(t *testing.T) {
	_, h := testServer(t)
	planVault(t, h)
	for _, query := range []string{"?kind=paragraph", "?level=zero", "?level=-1"} {
		if w := do(t, h, "GET", "/api/blocks"+query, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", query, w.Code)
		}
	}
}

func TestTasksComeFromTheIndex(t *testing.T) {
	_, h := testServer(t)
	planVault(t, h)

	var tasks []map[string]any
	decode(t, do(t, h, "GET", "/api/tasks", nil), &tasks)
	if len(tasks) != 3 {
		t.Fatalf("open tasks = %d: %v", len(tasks), tasks)
	}
	// The section a task sits under, which the body scan never knew.
	var risky map[string]any
	for _, task := range tasks {
		if strings.Contains(task["text"].(string), "disk") {
			risky = task
		}
	}
	if risky == nil || risky["section"] != "Risks" {
		t.Errorf("task does not carry its section: %v", risky)
	}
	if risky["path"] != "rollout.md" || risky["line"].(float64) <= 0 {
		t.Errorf("task lost its location: %v", risky)
	}

	decode(t, do(t, h, "GET", "/api/tasks?include_done=1", nil), &tasks)
	if len(tasks) != 4 {
		t.Errorf("with done = %d, want 4", len(tasks))
	}
	// Open first, which is what the console renders.
	if tasks[0]["done"] == true {
		t.Errorf("a done task sorted first: %v", tasks[0])
	}
}

func TestTasksCanBeNarrowedNow(t *testing.T) {
	// The point of indexing them: "the open tasks in this project" no longer
	// means fetching the whole vault and throwing most of it away.
	_, h := testServer(t)
	planVault(t, h)
	var tasks []map[string]any
	decode(t, do(t, h, "GET", "/api/tasks?path=other", nil), &tasks)
	if len(tasks) != 1 || !strings.Contains(tasks[0]["text"].(string), "unrelated") {
		t.Fatalf("path filter returned %v", tasks)
	}
	decode(t, do(t, h, "GET", "/api/tasks?q=queue", nil), &tasks)
	if len(tasks) != 1 {
		t.Errorf("text filter returned %v", tasks)
	}
}

func TestBlocksRespectReaderLists(t *testing.T) {
	// A line is note content, so every rule that governs reading a note
	// governs reading a line of one.
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")
	if _, err := s.WriteNote("restricted.md",
		"# Restricted\n\n- [ ] the severance paperwork\n",
		map[string]any{"title": "Restricted", "readers": "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteNote("open.md", "# Open\n\n- [ ] the standup notes\n",
		map[string]any{"title": "Open"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/blocks?kind=task", "/api/tasks"} {
		body := asKey(t, h, bobKey, "GET", path, nil).Body.String()
		if strings.Contains(strings.ToLower(body), "severance") {
			t.Errorf("%s leaked a restricted line:\n%s", path, body)
		}
		if !strings.Contains(body, "standup") {
			t.Errorf("%s hid the open line too:\n%s", path, body)
		}
	}
	if body := asKey(t, h, aliceKey, "GET", "/api/blocks?kind=task", nil).Body.String(); !strings.Contains(
		strings.ToLower(body), "severance") {
		t.Errorf("the person named on the reader list cannot see her own line:\n%s", body)
	}
}

func TestQueryBlockOverLinesGoesThroughTheSameAccessFilter(t *testing.T) {
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")
	if _, err := s.WriteNote("restricted.md",
		"# Restricted\n\n- [ ] the severance paperwork\n",
		map[string]any{"title": "Restricted", "readers": "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
	body := asKey(t, h, bobKey, "POST", "/api/query",
		map[string]any{"block": "from: tasks"}).Body.String()
	if strings.Contains(strings.ToLower(body), "severance") {
		t.Errorf("a query block over lines bypassed the reader list:\n%s", body)
	}
}

func TestTemplateBlockRendersThroughTheAPI(t *testing.T) {
	// The console draws markdown itself, so a live template is hydrated from
	// the server — which keeps ONE definition of what the block means across
	// the console, the read surface and a published page.
	_, h := testServer(t)
	if w := do(t, h, "POST", "/api/templates", map[string]any{
		"name": "standup", "body": "## Standup for {{owner}}\n\n- what shipped\n",
	}); w.Code >= 400 {
		t.Fatalf("write template = %d: %s", w.Code, w.Body)
	}
	var out struct {
		HTML string `json:"html"`
	}
	decode(t, do(t, h, "POST", "/api/template/render",
		map[string]any{"block": "use: standup\nowner: Ana"}), &out)
	if !strings.Contains(out.HTML, "Standup for Ana") {
		t.Errorf("template not rendered: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, "<h2") {
		t.Errorf("template body was not rendered as markdown: %s", out.HTML)
	}
}

func TestTemplateRenderNeedsABlockAndAnAccount(t *testing.T) {
	s, h := testServer(t)
	if w := do(t, h, "POST", "/api/template/render",
		map[string]any{"block": ""}); w.Code != http.StatusBadRequest {
		t.Errorf("empty block = %d, want 400", w.Code)
	}
	// A template pulls in a note body, so it is a read.
	makeUser(t, s, h, "", "alice", "admin")
	if w := do(t, h, "POST", "/api/template/render",
		map[string]any{"block": "use: x"}); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", w.Code)
	}
}

func TestTemplateCannotReachANoteTheCallerCannotRead(t *testing.T) {
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")
	if _, err := s.WriteNote("templates/restricted.md",
		"the severance terms\n",
		map[string]any{"title": "restricted", "readers": "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
	body := asKey(t, h, bobKey, "POST", "/api/template/render",
		map[string]any{"block": "use: restricted"}).Body.String()
	if strings.Contains(strings.ToLower(body), "severance") {
		t.Errorf("a template block read a note bob cannot open:\n%s", body)
	}
}

// --- retrieval scores and grounding ----------------------------------------

func TestRetrieveReportsTheLegsRawScores(t *testing.T) {
	// `score` is a reciprocal-rank value: the top hit scores about the same
	// whether it answers the question exactly or is the least bad of ten poor
	// matches. Anything downstream that wants to know how good the match
	// actually is needs the legs' own magnitudes, so they travel with the hit.
	_, h := testServer(t)
	// Two notes, not one. BM25's IDF is log(N/df), which is exactly zero for a
	// term that appears in EVERY chunk — so on a single-note vault the lexical
	// leg contributes nothing and this would assert against a degenerate
	// corpus rather than against the feature.
	for _, note := range []map[string]any{
		{"title": "Ops", "body": "# Ops\n\nthe deploy script lives at /usr/local/bin/deploy.sh\n"},
		{"title": "Cats", "body": "# Cats\n\nmarmalade sleeps on the windowsill\n"},
	} {
		if w := do(t, h, "POST", "/api/notes", note); w.Code != http.StatusCreated {
			t.Fatalf("seed = %d: %s", w.Code, w.Body)
		}
	}
	var hits []map[string]any
	decode(t, do(t, h, "GET", "/api/retrieve?q=deploy+script&k=5", nil), &hits)
	if len(hits) == 0 {
		t.Fatal("nothing retrieved")
	}
	for _, field := range []string{"cosine", "lexical", "score"} {
		if _, ok := hits[0][field]; !ok {
			t.Errorf("hit does not report %q: %v", field, hits[0])
		}
	}
	if hits[0]["lexical"].(float64) <= 0 {
		t.Errorf("a hit matching two query terms reported no lexical score: %v", hits[0])
	}
}

func TestAskReportsWhetherTheNotesSupportedTheAnswer(t *testing.T) {
	// With no reader configured the answer is extractive, which quotes
	// passages rather than judging them — so the honest verdict is "unknown",
	// and a caller that reads that as "grounded" has mistaken the absence of a
	// check for a passed one.
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{
		"title": "Ops", "body": "# Ops\n\nthe deploy needs a VPN reset first\n"})

	var out map[string]any
	decode(t, do(t, h, "POST", "/api/ask", map[string]any{"q": "what does the deploy need"}), &out)
	if out["supported"] != "unknown" {
		t.Errorf("supported = %v, want unknown with no reader", out["supported"])
	}
	if out["answer"] == "" {
		t.Error("no answer")
	}

	// Nothing retrieved is the one case that needs no reader to judge.
	_, empty := testServer(t)
	decode(t, do(t, empty, "POST", "/api/ask", map[string]any{"q": "anything at all"}), &out)
	if out["supported"] != "ungrounded" {
		t.Errorf("supported = %v on an empty vault, want ungrounded", out["supported"])
	}
}

func TestAskPropagatesTheReadersVerdict(t *testing.T) {
	// The whole chain, with a stub reader: prompt in, verdict out, and the
	// verdict line stripped from the answer the caller shows a person.
	s, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{
		"title": "Ops", "body": "# Ops\n\nthe deploy needs a VPN reset first\n"})

	reply := "SUPPORTED: no\nThe notes discuss the deploy but never give a port."
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"response": reply})
	}))
	defer stub.Close()
	if err := s.Settings.Update(map[string]string{"ollama_url": stub.URL}); err != nil {
		t.Fatal(err)
	}

	var out map[string]any
	decode(t, do(t, h, "POST", "/api/ask", map[string]any{"q": "what port"}), &out)
	if out["supported"] != "ungrounded" {
		t.Errorf("supported = %v, want ungrounded", out["supported"])
	}
	answer, _ := out["answer"].(string)
	if strings.Contains(answer, "SUPPORTED") {
		t.Errorf("the verdict line reached the caller's answer: %q", answer)
	}
	if !strings.Contains(answer, "never give a port") {
		t.Errorf("the answer was lost: %q", answer)
	}

	reply = "SUPPORTED: yes\nIt needs a VPN reset [1]."
	decode(t, do(t, h, "POST", "/api/ask", map[string]any{"q": "what does the deploy need"}), &out)
	if out["supported"] != "grounded" {
		t.Errorf("supported = %v, want grounded", out["supported"])
	}
}
