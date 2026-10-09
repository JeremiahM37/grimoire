package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNoteAuthorTellsPeopleFromAgents(t *testing.T) {
	cases := []struct{ path, fm, want string }{
		{"journal/2026-10-09.md", `{"title":"x"}`, ""},
		{"idea.md", `{"agent":"codex"}`, "codex"},
		{"idea.md", `{"agent":"codex","author":"me"}`, ""},
		{"idea.md", `{"author":"agent"}`, "agent"},
		{"memory/x.md", `{}`, "agent"},
		{"Agent Memory/feedback_x.md", `{}`, "agent"},
		{"memory/x.md", `{"author":"me"}`, ""},
		{"inbox/c.md", `{"source":"mcp"}`, "agent"},
		{"inbox/c.md", `{"source":"share-sheet"}`, ""},
		{"x.md", `{"memory":true}`, "agent"},
		{"x.md", `not json`, ""},
	}
	for _, c := range cases {
		if got := noteAuthor(c.path, c.fm); got != c.want {
			t.Errorf("noteAuthor(%q, %s) = %q, want %q", c.path, c.fm, got, c.want)
		}
	}
}

// A note an agent creates through the API is stamped with its name and listed
// as the agent's; one created from the console (no agent header) is yours.
func TestCreatedNotesCarryTheirAuthorIntoTheList(t *testing.T) {
	_, h := testServer(t)
	body, _ := json.Marshal(map[string]any{"path": "by-agent.md", "body": "# A\n"})
	req := httptest.NewRequest("POST", "/api/notes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Grimoire-Agent", "claude-code")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("agent create: %d %s", rec.Code, rec.Body.String())
	}
	if r := do(t, h, "POST", "/api/notes", map[string]any{"path": "by-me.md", "body": "# B\n"}); r.Code != http.StatusCreated {
		t.Fatalf("person create: %d %s", r.Code, r.Body.String())
	}
	var items []listItem
	if err := json.Unmarshal(do(t, h, "GET", "/api/notes", nil).Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, it := range items {
		got[it.Path] = it.Author
	}
	if got["by-agent.md"] != "claude-code" || got["by-me.md"] != "" {
		t.Fatalf("authors: %v", got)
	}
}
