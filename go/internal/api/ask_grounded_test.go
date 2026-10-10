package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
)

// Grounded ask runs the two-step procedure, passes the evidence it gathered
// to the second step, and resolves a relative date against the note's own
// date taken from its filename.
func TestAskGroundedProcedure(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var prompts []string
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		prompts = append(prompts, in.Prompt)
		mu.Unlock()
		reply := "plain"
		switch {
		case strings.Contains(in.Prompt, "step 1 of"):
			reply = "TYPE: date or time\\n- 2023-05-22 | Ana | shipped the release"
		case strings.Contains(in.Prompt, "steps 2 and 3"):
			reply = "working\\nFINAL: 22 May 2023"
		}
		_, _ = w.Write([]byte(`{"response":"` + reply + `"}`))
	}))
	defer llm.Close()

	s, h := testServer(t)
	s.AI = ai.New(stubSettings{"llm": "ollama", "ollama_url": llm.URL}, nil)
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "daily/2023-05-23.md",
		"body": "# Standup\n\nAna shipped the release yesterday and the gateway restarted"})

	w := do(t, h, "POST", "/api/ask", map[string]any{"question": "When did Ana ship the release?", "grounded": true})
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["answer"] != "22 May 2023" || out["grounded"] != true || out["calls"].(float64) != 2 {
		t.Fatalf("%v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(prompts) != 2 || !strings.Contains(prompts[0], "yesterday [= Mon 22 May 2023]") {
		t.Fatalf("relative date not resolved against the note date: %d prompts: %q", len(prompts), prompts)
	}
	if !strings.Contains(prompts[1], "- 2023-05-22 | Ana | shipped the release") {
		t.Fatal("step 1 evidence not passed to step 2")
	}
}
