package bank

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

const reflectMarker = "You answer questions from what one memory bank holds"

// scripted answers reflect turns from a list, one per turn, and records each
// turn's prompt.
type scripted struct {
	mu      sync.Mutex
	turns   []string
	prompts []string
	systems []string
	other   func(sys, user string) (string, string)
}

func (s *scripted) route(next func(string, string) (string, string)) func(string, string) (string, string) {
	return func(sys, user string) (string, string) {
		if strings.HasPrefix(sys, reflectMarker) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.prompts = append(s.prompts, user)
			s.systems = append(s.systems, sys)
			i := len(s.prompts) - 1
			if i < len(s.turns) {
				return s.turns[i], "stop"
			}
			return s.turns[len(s.turns)-1], "stop"
		}
		if s.other != nil {
			if out, fin := s.other(sys, user); out != "" {
				return out, fin
			}
		}
		return next(sys, user)
	}
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func humanBank(t *testing.T, h *harness) string {
	t.Helper()
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon and play chess.", DocumentID: "d1"})
	h.editFile(t, FactsPath("b", "d1"), "Alice lives in Lyon <!--f", "Alice lives in Paris <!--f")
	u := unitByText(t, h, "b", "Alice lives in Paris")
	if u == nil || !u.Human {
		t.Fatal("setup: expected a human fact")
	}
	return u.ID
}

func TestReflectEnforcesTheLevelOrderAndChecksCitations(t *testing.T) {
	h := newHarness(t, true)
	h.llm.route = extractOnly(livesIn("Lyon"))
	humanID := humanBank(t, h)
	if _, err := h.e.CreateModel("b", ModelSpec{ID: "hobbies", Name: strPtr("Hobbies"), Question: strPtr("What are Alice's hobbies?"),
		Body: strPtr("Alice plays chess on Sundays.")}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.CreateDirective("b", DirectiveSpec{Name: strPtr("Brief"), Text: strPtr("Answer in one sentence.")}); err != nil {
		t.Fatal(err)
	}
	sc := &scripted{turns: []string{
		js(map[string]any{"tool": "recall", "args": map[string]any{"query": "Alice home"}}), // skips the first level
		js(map[string]any{"tool": "recall", "args": map[string]any{"query": "Alice home city"}}),
		js(map[string]any{"tool": "done", "args": map[string]any{"answer": "Alice lives in Paris, and she likes chess a lot.",
			"memory_ids": []any{humanID, "fdeadbeef"}, "model_ids": []any{"hobbies"}}}),
	}}
	sc.other = func(sys, user string) (string, string) {
		switch {
		case strings.HasPrefix(sys, "You check an answer against a list of rules"):
			return js(map[string]any{"complies": false, "violations": []any{"two clauses"}, "revised_answer": "Alice lives in Paris."}), "stop"
		case strings.HasPrefix(sys, "You extract information from a text"):
			return `{"city": "Paris"}`, "stop"
		}
		return "", ""
	}
	h.llm.route = sc.route(extractOnly(livesIn("Lyon")))
	res, err := h.e.Reflect(context.Background(), "b", ReflectRequest{Query: "Where does Alice live?",
		ResponseSchema: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required": []any{"city"}}})
	if err != nil {
		t.Fatal(err)
	}
	tc := res.Trace.ToolCalls
	if len(tc) < 2 || tc[0].Tool != toolSearchModels || !tc[0].Forced {
		t.Fatalf("the first level was not enforced: %+v", tc)
	}
	if res.Trace.Levels[0] != toolSearchModels || res.Trace.Levels[len(res.Trace.Levels)-1] != toolRecall {
		t.Errorf("levels = %v", res.Trace.Levels)
	}
	if res.Text != "Alice lives in Paris." || !res.DirectivesChecked {
		t.Errorf("directive check not applied: %q", res.Text)
	}
	if len(res.BasedOn.Memories) != 1 || res.BasedOn.Memories[0].ID != humanID || res.BasedOn.Memories[0].Authority != "human" {
		t.Errorf("memories = %+v", res.BasedOn.Memories)
	}
	if len(res.BasedOn.MentalModels) != 1 || len(res.BasedOn.Directives) != 1 {
		t.Errorf("based_on = %+v", res.BasedOn)
	}
	if len(res.Trace.Rejected) != 1 || res.Trace.Rejected[0] != "fdeadbeef" {
		t.Errorf("an id no tool returned must be rejected: %v", res.Trace.Rejected)
	}
	if m, ok := res.StructuredOutput.(map[string]any); !ok || m["city"] != "Paris" {
		t.Errorf("structured = %v (%s)", res.StructuredOutput, res.StructuredOutputError)
	}
	if res.Usage.Total == 0 || res.Mode != "llm" {
		t.Errorf("usage/mode = %+v %s", res.Usage, res.Mode)
	}
	sys := sc.systems[0]
	if !strings.Contains(sys, "Answer in one sentence.") || !strings.Contains(sys, `authority "human"`) {
		t.Errorf("system prompt lacks directives or the authority rule:\n%s", sys)
	}
	// The person's fact reaches the model labelled as theirs.
	last := sc.prompts[len(sc.prompts)-1]
	if !strings.Contains(last, `"authority":"human"`) || !strings.Contains(last, "Alice lives in Paris") {
		t.Errorf("tool results did not carry authority:\n%s", last)
	}
}

func TestReflectRefusesDoneBeforeAnyEvidence(t *testing.T) {
	h := newHarness(t, true)
	h.llm.route = extractOnly(livesIn("Lyon"))
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon.", DocumentID: "d1"})
	sc := &scripted{turns: []string{
		// recall is the only level, so the first turn is forced anyway;
		// afterwards the model tries to stop with nothing it can cite.
		js(map[string]any{"tool": "done", "args": map[string]any{"answer": "No idea."}}),
		js(map[string]any{"tool": "done", "args": map[string]any{"answer": "Alice lives in Lyon."}}),
	}}
	h.llm.route = sc.route(extractOnly(livesIn("Lyon")))
	res, err := h.e.Reflect(context.Background(), "b", ReflectRequest{Query: "Where does Alice live?"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Trace.ToolCalls[0].Tool != toolRecall || !res.Trace.ToolCalls[0].Forced {
		t.Errorf("recall must run before done: %+v", res.Trace.ToolCalls)
	}
	if res.Text != "Alice lives in Lyon." || len(res.BasedOn.Memories) == 0 {
		t.Errorf("res = %q %+v", res.Text, res.BasedOn)
	}
}

func TestReflectWithoutAModelIsExtractive(t *testing.T) {
	h := newHarness(t, false)
	h.retain(t, "b", Item{Content: "Alice adopted a beagle named Biscuit. She works as a nurse.", DocumentID: "d1"})
	res, err := h.e.Reflect(context.Background(), "b", ReflectRequest{Query: "What dog did Alice adopt?"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "extractive" || !strings.Contains(res.Text, "beagle") || len(res.BasedOn.Memories) == 0 {
		t.Errorf("res = %+v", res)
	}
	if _, err := h.e.Reflect(context.Background(), "b", ReflectRequest{Query: "x", RequireModel: true}); err != ErrModelRequired {
		t.Errorf("RequireModel err = %v", err)
	}
	if _, err := h.e.Reflect(context.Background(), "b", ReflectRequest{Query: "x",
		ResponseSchema: map[string]any{"type": "array"}}); err == nil {
		t.Error("a non-object schema must be refused")
	}
}

func TestDispositionAndDirectiveScoping(t *testing.T) {
	if dispositionText(Disposition{3, 3, 3}) != "" {
		t.Error("a neutral disposition adds nothing")
	}
	if s := dispositionText(Disposition{5, 3, 1}); !strings.Contains(s, "scrutinise") || !strings.Contains(s, "set feelings aside") ||
		strings.Count(s, "\n- ") != 2 {
		t.Errorf("disposition text = %q", s)
	}
	all := []Directive{{ID: "a", Text: "always"}, {ID: "b", Text: "ops only", Tags: []string{"ops"}},
		{ID: "c", Text: "off", Inactive: true}}
	if got := selectDirectives(all, nil, "any", false); len(got) != 1 || got[0].ID != "a" {
		t.Errorf("untagged reflect = %+v", got)
	}
	if got := selectDirectives(all, []string{"ops"}, "any", false); len(got) != 2 {
		t.Errorf("ops reflect = %+v", got)
	}
	if got := selectDirectives(all, nil, "any", true); len(got) != 2 {
		t.Errorf("apply all = %+v", got)
	}
}

func TestParseCallsIsLenient(t *testing.T) {
	cases := map[string]string{
		"```json\n{\"tool\":\"recall\",\"args\":{\"query\":\"x\"}}\n```":              "recall",
		`{"name": "functions.search_observations", "arguments": "{\"query\":\"y\"}"}`: "search_observations",
		`{"calls": [{"tool": "recall", "args": {}}, {"tool": "done", "args": {}}]}`:   "recall",
		`{"action": "read_mental_models", "ids": ["a"]}`:                              "read_mental_model",
	}
	for in, want := range cases {
		calls, err := parseCalls(in)
		if err != nil || calls[0].tool != want {
			t.Errorf("%s → %+v %v", in, calls, err)
		}
	}
	if _, err := parseCalls("I think the answer is Paris."); err == nil {
		t.Error("prose is not a call")
	}
}
