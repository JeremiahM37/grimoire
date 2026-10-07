package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeOpenAI answers /chat/completions and records each request body.
func fakeOpenAI(t *testing.T, content, finish string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var seen []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content},
				"finish_reason": finish}},
			"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestCompleteWithSendsSystemJSONTemperatureAndBudget(t *testing.T) {
	srv, seen := fakeOpenAI(t, "```json\n{\"ok\":true}\n```", "stop")
	c := New(mapSettings{"llm": "openai", "llm_base_url": srv.URL, "llm_model": "m"}, nil)
	got, err := c.CompleteWith(context.Background(), "hello", CompleteOpts{
		System: "be terse", Temperature: Temp(0.1), MaxTokens: 500, JSON: true})
	if err != nil {
		t.Fatal(err)
	}
	body := (*seen)[0]
	msgs := body["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v, want system then user", msgs)
	}
	if body["temperature"] != 0.1 || body["max_tokens"] != float64(500) {
		t.Errorf("temperature/max_tokens = %v/%v", body["temperature"], body["max_tokens"])
	}
	if rf, _ := body["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Errorf("response_format = %v", body["response_format"])
	}
	if _, has := body["reasoning_effort"]; has {
		t.Error("no effort configured, so none may be sent")
	}
	var v struct{ OK bool }
	if err := DecodeJSON(got.Text, &v); err != nil || !v.OK {
		t.Errorf("decode %q: %v %v", got.Text, v, err)
	}
	if got.Usage.Input != 11 || got.Usage.Output != 7 {
		t.Errorf("usage = %+v", got.Usage)
	}
}

func TestEffortOffDisablesThinkingTheDocumentedWay(t *testing.T) {
	srv, seen := fakeOpenAI(t, "{}", "stop")
	c := New(mapSettings{"llm": "openai", "llm_base_url": srv.URL,
		"llm_reasoning_effort": "off"}, nil)
	if _, err := c.CompleteWith(context.Background(), "x", CompleteOpts{}); err != nil {
		t.Fatal(err)
	}
	th, _ := (*seen)[0]["thinking"].(map[string]any)
	if th["type"] != "disabled" {
		t.Errorf("thinking = %v, want {type: disabled}", (*seen)[0]["thinking"])
	}
	// An explicit per-call effort wins over the setting.
	if _, err := c.CompleteWith(context.Background(), "x", CompleteOpts{Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	if (*seen)[1]["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", (*seen)[1]["reasoning_effort"])
	}
}

func TestExtraBodyIsMergedButExplicitFieldsWin(t *testing.T) {
	srv, seen := fakeOpenAI(t, "{}", "stop")
	c := New(mapSettings{"llm": "openai", "llm_base_url": srv.URL,
		"llm_extra_body": `{"thinking":{"type":"disabled"},"max_tokens":3}`}, nil)
	if _, err := c.CompleteWith(context.Background(), "x", CompleteOpts{MaxTokens: 900}); err != nil {
		t.Fatal(err)
	}
	if (*seen)[0]["max_tokens"] != float64(900) {
		t.Errorf("max_tokens = %v; the call's own budget must beat the blanket setting", (*seen)[0]["max_tokens"])
	}
	if (*seen)[0]["thinking"] == nil {
		t.Error("extra body was not merged")
	}
	// The plain Complete path gets it too, without losing its own fields.
	if _, err := c.Complete("y", ""); err != nil {
		t.Fatal(err)
	}
	if (*seen)[1]["thinking"] == nil || (*seen)[1]["temperature"] != 0.2 {
		t.Errorf("plain complete body = %v", (*seen)[1])
	}
}

func TestPlainCompleteIsUnchangedWithNoNewSettings(t *testing.T) {
	srv, seen := fakeOpenAI(t, "hi", "stop")
	c := New(mapSettings{"llm": "openai", "llm_base_url": srv.URL}, nil)
	if _, err := c.Complete("y", ""); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"model": true, "stream": true, "temperature": true, "messages": true}
	for k := range (*seen)[0] {
		if !want[k] {
			t.Errorf("plain complete grew a field %q", k)
		}
	}
}

func TestTruncationIsReported(t *testing.T) {
	srv, _ := fakeOpenAI(t, `{"facts":[`, "length")
	c := New(mapSettings{"llm": "openai", "llm_base_url": srv.URL}, nil)
	got, err := c.CompleteWith(context.Background(), "x", CompleteOpts{JSON: true})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated() {
		t.Error("a length stop must read as truncated")
	}
}

func TestOllamaOptionsCarrySystemFormatAndBudget(t *testing.T) {
	srv, seen := fakeOllama(t, `{"a":1}`)
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	if _, err := c.CompleteWith(context.Background(), "x", CompleteOpts{
		System: "sys", JSON: true, MaxTokens: 77, Temperature: Temp(0)}); err != nil {
		t.Fatal(err)
	}
	b := (*seen)[0]
	if b["system"] != "sys" || b["format"] != "json" || b["think"] != false {
		t.Errorf("body = %v", b)
	}
	opts := b["options"].(map[string]any)
	if opts["num_predict"] != float64(77) || opts["temperature"] != float64(0) {
		t.Errorf("options = %v", opts)
	}
}

func TestDecodeJSONTolerance(t *testing.T) {
	cases := []string{
		`{"x": 1}`,
		"```json\n{\"x\": 1}\n```",
		"Here you go:\n{\"x\": 1}\nHope that helps! {\"y\":2}",
		"<think>let me see {not json</think>{\"x\": 1}",
		"  \n```\n{\"x\": 1}\n```  trailing",
	}
	for _, c := range cases {
		var v struct{ X int }
		if err := DecodeJSON(c, &v); err != nil || v.X != 1 {
			t.Errorf("DecodeJSON(%q) = %v, %v", c, v, err)
		}
	}
	var v any
	if err := DecodeJSON("no json here", &v); err == nil {
		t.Error("text with no JSON must fail")
	}
	var arr []int
	if err := DecodeJSON("result: [1,2,3] done", &arr); err != nil || len(arr) != 3 {
		t.Errorf("array = %v %v", arr, err)
	}
}

func TestNoBackendIsAnError(t *testing.T) {
	if _, err := New(mapSettings{}, nil).CompleteWith(context.Background(), "x", CompleteOpts{}); err != ErrNoBackend {
		t.Errorf("err = %v", err)
	}
}
