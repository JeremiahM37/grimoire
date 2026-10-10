package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// contextFor asks the hybrid context endpoint for a query and returns the
// injected text.
func contextFor(t *testing.T, h http.Handler, q string, extra ...string) string {
	t.Helper()
	v := url.Values{"q": {q}, "rank": {"hybrid"}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	rec := do(t, h, "GET", "/api/memory/context?"+v.Encode(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("context: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Context string `json:"context"`
		Mode    string `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Mode != "hybrid" {
		t.Fatalf("context body %s (%v)", rec.Body, err)
	}
	return out.Context
}

const releaseFact = "Deploys switch the web directory in release.conf before restarting"

func TestRetellTeachesACueThatSurfacesTheFact(t *testing.T) {
	_, h := testServer(t)
	first := remember(t, h, map[string]any{"topic": "deploy", "text": releaseFact})
	if first["op"] != "ADD" {
		t.Fatalf("first write: %v", first)
	}
	// The agent's situation shares no words with the fact, so nothing surfaces.
	situation := "please ship the newest grimoire build to the box"
	if got := contextFor(t, h, situation); strings.Contains(got, "release.conf") {
		t.Fatalf("surfaced before any cue: %s", got)
	}
	// The user tells the agent again; the write is a NOOP, and the situation
	// is learned as a cue for the fact on file.
	again := remember(t, h, map[string]any{"topic": "deploy", "text": releaseFact,
		"context": situation})
	if again["op"] != "NOOP" {
		t.Fatalf("re-tell should be a NOOP, got %v", again)
	}
	if got := contextFor(t, h, situation); !strings.Contains(got, "release.conf") {
		t.Fatalf("learned cue did not surface the fact:\n%s", got)
	}
}

func TestRetellWithoutContextUsesTheAgentsLastPrompt(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "deploy", "text": releaseFact})
	situation := "please ship the newest grimoire build to the box"
	contextFor(t, h, situation) // the hook's query: remembered as this agent's last prompt
	remember(t, h, map[string]any{"topic": "deploy", "text": releaseFact})
	if got := contextFor(t, h, situation); !strings.Contains(got, "release.conf") {
		t.Fatalf("cue from the last prompt did not surface the fact:\n%s", got)
	}
}

func TestAgentCuesAreStoredWithANewFact(t *testing.T) {
	_, h := testServer(t)
	res := remember(t, h, map[string]any{"topic": "deploy", "infer": false, "text": releaseFact,
		"cues": []string{"ship the newest build", "/etc/systemd/system/grimoire.service.d/release.conf"}})
	rec := do(t, h, "GET", "/api/memory/cues?target=fact:"+res["id"].(string), nil)
	var out struct {
		Cues []struct{ Kind, Text, Source string } `json:"cues"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Cues) != 2 {
		t.Fatalf("cues: %d %s", rec.Code, rec.Body)
	}
	kinds := map[string]string{}
	for _, c := range out.Cues {
		kinds[c.Text] = c.Kind
		if c.Source != "agent" {
			t.Errorf("source = %q", c.Source)
		}
	}
	if kinds["ship the newest build"] != "request" || kinds["/etc/systemd/system/grimoire.service.d/release.conf"] != "action" {
		t.Errorf("kinds = %v", kinds)
	}
	// At the action stage the named path fires the fact outright.
	got := contextFor(t, h, "Edit /etc/systemd/system/grimoire.service.d/release.conf", "stage", "action")
	if !strings.Contains(got, "release.conf before restarting") {
		t.Fatalf("action trigger did not fire:\n%s", got)
	}
}

func TestUntrustedWritersDoNotTeachCues(t *testing.T) {
	_, h := testServer(t)
	res := remember(t, h, map[string]any{"topic": "deploy", "infer": false, "text": releaseFact,
		"origin": "web:example.com", "cues": []string{"ship the newest build"}})
	rec := do(t, h, "GET", "/api/memory/cues?target=fact:"+res["id"].(string), nil)
	if strings.Contains(rec.Body.String(), "ship the newest build") {
		t.Fatalf("untrusted origin stored a cue: %s", rec.Body)
	}
}

func TestCuesEndpointValidates(t *testing.T) {
	_, h := testServer(t)
	for _, body := range []map[string]any{
		{"target": "bogus", "cues": []string{"x"}},
		{"target": "fact:nope", "cues": []string{"x"}},
		{"target": "note:x.md", "cues": []string{}},
		{"target": "note:x.md", "cues": []string{"x"}, "source": "made-up"},
	} {
		if rec := do(t, h, "POST", "/api/memory/cues", body); rec.Code < 400 {
			t.Errorf("accepted %v: %d", body, rec.Code)
		}
	}
	do(t, h, "PUT", "/api/notes/runbooks/deploy.md", map[string]any{"body": "# Deploy\n\nUse the release script."})
	rec := do(t, h, "POST", "/api/memory/cues", map[string]any{"target": "note:runbooks/deploy.md",
		"cues": []string{"roll out a new version"}, "source": "learned"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"added":1`) {
		t.Fatalf("add note cue: %d %s", rec.Code, rec.Body)
	}
	if got := contextFor(t, h, "roll out a new version"); !strings.Contains(got, "release script") {
		t.Fatalf("note cue did not surface the note:\n%s", got)
	}
}

func TestCorrectionInTheSameSessionTeachesTheEarlierPrompt(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "PUT", "/api/notes/Agent%20Memory/feedback_delegation.md",
		map[string]any{"body": "Use the lead model to coordinate and a cheaper model to implement."})
	task := "redo the phone layout of the sessions page"
	contextFor(t, h, task, "session", "s1")
	if got := contextFor(t, h, task+" for the tablet", "session", "s2"); strings.Contains(got, "cheaper model") {
		t.Fatalf("surfaced before learning:\n%s", got)
	}
	// Same session: the user restates the rule. That correction matches the
	// note strongly, so the session's previous prompt becomes its cue.
	contextFor(t, h, "use the lead model to coordinate and a cheaper model to implement, I told you", "session", "s1")
	rec := do(t, h, "GET", "/api/memory/cues?target="+url.QueryEscape("note:Agent Memory/feedback_delegation.md"), nil)
	if !strings.Contains(rec.Body.String(), task) || !strings.Contains(rec.Body.String(), `"learned"`) {
		t.Fatalf("no learned cue: %s", rec.Body)
	}
	// A plain question on the topic is not a correction and teaches nothing.
	contextFor(t, h, "unrelated earlier prompt", "session", "s3")
	contextFor(t, h, "which model coordinates and which implements", "session", "s3")
	if strings.Contains(do(t, h, "GET", "/api/memory/cues?target="+url.QueryEscape("note:Agent Memory/feedback_delegation.md"), nil).Body.String(), "unrelated earlier prompt") {
		t.Fatal("a question was taken as a re-tell")
	}
}

func TestToolCallsNeverEnterRetellPairing(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "PUT", "/api/notes/Agent%20Memory/feedback_delegation.md",
		map[string]any{"body": "Use the lead model to coordinate and a cheaper model to implement."})
	task := "redo the phone layout of the sessions page"
	contextFor(t, h, task, "session", "s1")
	// A tool call between the prompt and the correction must not become the
	// "previous prompt" that the correction teaches.
	contextFor(t, h, "Bash npm run build", "session", "s1", "stage", "action")
	contextFor(t, h, "use the lead model to coordinate and a cheaper model to implement, I told you", "session", "s1")
	body := do(t, h, "GET", "/api/memory/cues?target="+url.QueryEscape("note:Agent Memory/feedback_delegation.md"), nil).Body.String()
	if strings.Contains(body, "npm run build") || !strings.Contains(body, task) {
		t.Fatalf("tool call entered the pairing: %s", body)
	}
}

func TestJudgedRetellLearnsOnlyOnAConfidentYes(t *testing.T) {
	for _, tc := range []struct {
		verdict float64
		learns  bool
	}{{0.95, true}, {0.3, false}} {
		s, h := testServer(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"answers":{"retell":{"type":"noul","noul":%v}}}`, tc.verdict)
		}))
		if err := s.Settings.Update(map[string]string{"retell_url": srv.URL}); err != nil {
			t.Fatal(err)
		}
		do(t, h, "PUT", "/api/notes/Agent%20Memory/feedback_delegation.md",
			map[string]any{"body": "Use the lead model to coordinate and a cheaper model to implement."})
		task := "redo the phone layout of the sessions page"
		contextFor(t, h, task, "session", "s1")
		// A paraphrase with no restatement marker: only the judge can tell.
		contextFor(t, h, "use the lead model to coordinate, cheaper model implements", "session", "s1")
		target := url.QueryEscape("note:Agent Memory/feedback_delegation.md")
		learned := false
		for i := 0; i < 50 && !learned; i++ {
			learned = strings.Contains(do(t, h, "GET", "/api/memory/cues?target="+target, nil).Body.String(), task)
			if !learned {
				time.Sleep(20 * time.Millisecond)
			}
		}
		srv.Close()
		if learned != tc.learns {
			t.Errorf("verdict %v: learned=%v, want %v", tc.verdict, learned, tc.learns)
		}
	}
}
