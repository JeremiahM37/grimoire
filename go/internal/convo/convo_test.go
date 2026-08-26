package convo

import (
	"strings"
	"testing"
	"time"
)

// ChatGPT stores a conversation as an edit TREE, not a list. A chat where the
// user regenerated a reply contains both replies, and reading the mapping in
// map order returns them in whatever order Go iterates — a scrambled
// transcript that reads like the assistant answering itself.
const chatGPTBranched = `[{
  "title": "Deploy question",
  "create_time": 1756000000,
  "current_node": "c2",
  "mapping": {
    "root": {"id":"root","parent":"","children":["u1"],"message":null},
    "u1": {"id":"u1","parent":"root","children":["a1","c2"],
      "message":{"author":{"role":"user"},"create_time":1756000001,
        "content":{"content_type":"text","parts":["how do I roll back?"]}}},
    "a1": {"id":"a1","parent":"u1","children":[],
      "message":{"author":{"role":"assistant"},"create_time":1756000002,
        "content":{"content_type":"text","parts":["FIRST DRAFT, regenerated away"]}}},
    "c2": {"id":"c2","parent":"u1","children":[],
      "message":{"author":{"role":"assistant"},"create_time":1756000003,
        "content":{"content_type":"text","parts":["run the rollback script"]}}},
    "sys": {"id":"sys","parent":"root","children":[],
      "message":{"author":{"role":"system"},"create_time":1756000000,
        "content":{"content_type":"text","parts":["you are a helpful assistant"]}}}
  }
}]`

func TestChatGPTFollowsTheKeptBranch(t *testing.T) {
	got, err := ParseChatGPT([]byte(chatGPTBranched))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d conversations, want 1", len(got))
	}
	c := got[0]
	if c.Title != "Deploy question" {
		t.Errorf("title = %q", c.Title)
	}
	if len(c.Messages) != 2 {
		t.Fatalf("got %d messages, want 2 (the kept branch only): %+v", len(c.Messages), c.Messages)
	}
	if strings.Contains(c.Markdown(), "FIRST DRAFT") {
		t.Error("a regenerated-away reply was imported; the transcript now contains " +
			"an answer the user never kept")
	}
	if c.Messages[0].Role != "user" || c.Messages[1].Role != "assistant" {
		t.Errorf("turns out of order: %s then %s", c.Messages[0].Role, c.Messages[1].Role)
	}
	// System prompts are not the conversation.
	if strings.Contains(c.Markdown(), "helpful assistant") {
		t.Error("a system prompt was imported as a turn")
	}
}

// An older export has no current_node. Dropping those conversations would lose
// the oldest history, which is the part worth having.
func TestChatGPTFallsBackWithoutACurrentNode(t *testing.T) {
	src := strings.Replace(chatGPTBranched, `"current_node": "c2",`, "", 1)
	got, err := ParseChatGPT([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Messages) == 0 {
		t.Fatalf("conversation dropped when current_node was absent: %+v", got)
	}
}

// A cycle in parent pointers must not hang the import.
func TestChatGPTSurvivesACycle(t *testing.T) {
	src := `[{"title":"x","current_node":"a","mapping":{
      "a":{"id":"a","parent":"b","children":[],"message":{"author":{"role":"user"},
        "content":{"parts":["one"]}}},
      "b":{"id":"b","parent":"a","children":[],"message":{"author":{"role":"user"},
        "content":{"parts":["two"]}}}}}]`
	done := make(chan bool, 1)
	go func() {
		_, _ = ParseChatGPT([]byte(src))
		done <- true
	}()
	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatal("parsing a cyclic parent chain did not terminate")
	}
}

const claudeExport = `[{
  "name": "Runbook chat",
  "created_at": "2026-08-01T09:00:00Z",
  "chat_messages": [
    {"sender":"human","text":"where is the runbook?","created_at":"2026-08-01T09:00:01Z"},
    {"sender":"assistant","text":"","created_at":"2026-08-01T09:00:02Z",
     "content":[{"type":"text","text":"in the ops folder"}]},
    {"sender":"assistant","text":"","created_at":"2026-08-01T09:00:03Z","content":[]}
  ]
}]`

func TestClaudeReadsTextAndContentBlocks(t *testing.T) {
	got, err := ParseClaude([]byte(claudeExport))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d conversations", len(got))
	}
	c := got[0]
	if len(c.Messages) != 2 {
		t.Fatalf("got %d messages, want 2 (the empty one dropped): %+v", len(c.Messages), c.Messages)
	}
	if c.Messages[0].Role != "user" {
		t.Errorf("sender 'human' should map to user, got %q", c.Messages[0].Role)
	}
	// Newer exports put the body in content blocks and leave text empty.
	if !strings.Contains(c.Markdown(), "in the ops folder") {
		t.Errorf("content-block body was dropped:\n%s", c.Markdown())
	}
}

// Both products call the file conversations.json, so the format is detected
// from its shape. Getting this wrong means telling a user their export is
// unrecognised while holding it.
func TestDetectDistinguishesTheTwoExports(t *testing.T) {
	if got := Detect([]byte(chatGPTBranched)); got != "chatgpt" {
		t.Errorf("chatgpt export detected as %q", got)
	}
	if got := Detect([]byte(claudeExport)); got != "claude" {
		t.Errorf("claude export detected as %q", got)
	}
	for _, junk := range []string{`{"not":"an array"}`, ``, `[]`, `[{"unrelated":1}]`} {
		if got := Detect([]byte(junk)); got != "" {
			t.Errorf("Detect(%q) = %q, want empty", junk, got)
		}
	}
}

func TestMarkdownLabelsSpeakers(t *testing.T) {
	got, _ := ParseClaude([]byte(claudeExport))
	md := got[0].Markdown()
	if !strings.Contains(md, "**You**") || !strings.Contains(md, "**Assistant**") {
		t.Errorf("speakers are not labelled, so the transcript is unreadable:\n%s", md)
	}
}

func TestMalformedInputIsAnErrorNotAPanic(t *testing.T) {
	for _, bad := range []string{``, `not json`, `{"a":1}`, `[1,2,3]`} {
		if _, err := ParseChatGPT([]byte(bad)); err == nil {
			t.Errorf("ParseChatGPT(%q) returned no error", bad)
		}
		if _, err := ParseClaude([]byte(bad)); err == nil {
			t.Errorf("ParseClaude(%q) returned no error", bad)
		}
	}
}

func timeoutAfter() <-chan struct{} {
	ch := make(chan struct{})
	go func() { time.Sleep(3 * time.Second); close(ch) }()
	return ch
}
