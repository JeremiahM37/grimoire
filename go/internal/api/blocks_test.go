package api

import (
	"net/http"
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
