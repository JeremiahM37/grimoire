package ai

import (
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

func cands(texts ...string) []memory.Entry {
	var out []memory.Entry
	for i, t := range texts {
		out = append(out, memory.Entry{ID: string(rune('a' + i)), Text: t})
	}
	return out
}

func TestDecideMemoryFallsBackToRulesWithNoLLM(t *testing.T) {
	c := New(mapSettings{}, nil)
	got := c.DecideMemory("user prefers tabs", cands("user prefers spaces"))
	if got.Op != memory.OpUpdate || got.Target != "a" {
		t.Fatalf("rule path not taken: %+v", got)
	}
}

func TestDecideMemoryUsesTheModelsVerdict(t *testing.T) {
	srv, seen := fakeOllama(t, "UPDATE 0")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	// Lexically unrelated, so the rules alone would say ADD — the model's
	// verdict has to be what decides.
	got := c.DecideMemory("the office moved to the third floor",
		cands("the team sits on the ground floor"))
	if got.Op != memory.OpUpdate || got.Target != "a" {
		t.Fatalf("model verdict ignored: %+v", got)
	}
	prompt := (*seen)[0]["prompt"].(string)
	for _, want := range []string{"NEW FACT:", "EXISTING FACTS:", "[0]", "Prefer ADD when unsure"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestDecideMemoryRejectsAnOutOfRangeTarget(t *testing.T) {
	// A model naming a candidate that does not exist must not become an edit
	// to whatever happens to be at that index.
	srv, _ := fakeOllama(t, "UPDATE 7")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	got := c.DecideMemory("a brand new unrelated fact", cands("something else entirely"))
	if got.Op != memory.OpAdd {
		t.Fatalf("out-of-range target was honoured: %+v", got)
	}
}

func TestDecideMemoryRejectsGarbage(t *testing.T) {
	for _, reply := range []string{"", "I think you should probably merge these",
		"MERGE 0", "UPDATE", "UPDATE banana"} {
		srv, _ := fakeOllama(t, reply)
		c := New(mapSettings{"ollama_url": srv.URL}, nil)
		got := c.DecideMemory("an unrelated new fact", cands("nothing like it"))
		if got.Op != memory.OpAdd {
			t.Errorf("reply %q produced %+v, want the rule's ADD", reply, got)
		}
	}
}

func TestDecideMemoryNeverOffersAnImmutableCandidate(t *testing.T) {
	// The model cannot target what it is not shown, which is what stops a
	// prompt from talking the server into overwriting a pinned fact.
	srv, seen := fakeOllama(t, "UPDATE 0")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	pinned := cands("never delete the production database")
	pinned[0].Immutable = true
	got := c.DecideMemory("delete the production database", pinned)
	if got.Op == memory.OpUpdate || got.Op == memory.OpDelete {
		t.Fatalf("immutable candidate was targeted: %+v", got)
	}
	if len(*seen) != 0 {
		t.Error("model was consulted with no eligible candidate")
	}
}

func TestDecideMemoryNeverOffersASupersededCandidate(t *testing.T) {
	srv, seen := fakeOllama(t, "UPDATE 0")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	dead := cands("an old belief", "the current belief")
	dead[0].SupersededBy = "b"
	got := c.DecideMemory("a replacement belief", dead)
	if got.Target == "a" {
		t.Fatalf("a superseded entry was targeted: %+v", got)
	}
	prompt := (*seen)[0]["prompt"].(string)
	if strings.Contains(prompt, "an old belief") {
		t.Errorf("superseded candidate was shown to the model:\n%s", prompt)
	}
}

func TestDecideMemoryHandlesEveryOperation(t *testing.T) {
	cases := map[string]memory.Op{
		"ADD":        memory.OpAdd,
		"NOOP 0":     memory.OpNoop,
		"UPDATE 0":   memory.OpUpdate,
		"DELETE 0":   memory.OpDelete,
		"update 0":   memory.OpUpdate, // case-insensitive
		"UPDATE [0]": memory.OpUpdate,
		"NOOP 0.":    memory.OpNoop,
	}
	for reply, want := range cases {
		srv, _ := fakeOllama(t, reply)
		c := New(mapSettings{"ollama_url": srv.URL}, nil)
		got := c.DecideMemory("an unrelated new fact", cands("nothing like it"))
		if got.Op != want {
			t.Errorf("reply %q = %s, want %s", reply, got.Op, want)
		}
	}
}

func TestDecideMemoryDeleteCarriesNoText(t *testing.T) {
	srv, _ := fakeOllama(t, "DELETE 0")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	got := c.DecideMemory("that is no longer true", cands("something that was true"))
	if got.Op != memory.OpDelete {
		t.Fatalf("op = %s", got.Op)
	}
	if got.Text != "" {
		t.Errorf("a retraction stored text: %q", got.Text)
	}
}

func TestExtractFactsWithNoLLM(t *testing.T) {
	c := New(mapSettings{}, nil)
	got := c.ExtractFacts("User prefers tabs. The server runs proxmox.")
	if len(got) != 2 {
		t.Fatalf("got %q, want two facts", got)
	}
}

func TestExtractFactsUsesTheModelForConjoinedClauses(t *testing.T) {
	// The case sentence splitting cannot do: one sentence, two facts, the
	// second missing its subject.
	srv, seen := fakeOllama(t,
		"the user prefers tabs\nthe user hates trailing whitespace")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	got := c.ExtractFacts("the user prefers tabs and hates trailing whitespace in every file")
	if len(got) != 2 {
		t.Fatalf("got %q, want two facts", got)
	}
	if strings.HasPrefix(got[1], "and ") {
		t.Errorf("second fact kept its conjunction: %q", got[1])
	}
	prompt := (*seen)[0]["prompt"].(string)
	if !strings.Contains(prompt, "repeat the subject") {
		t.Errorf("prompt lost the standalone-fact instruction:\n%s", prompt)
	}
}

func TestExtractFactsFallsBackOnEmptyReply(t *testing.T) {
	srv, _ := fakeOllama(t, "   ")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	got := c.ExtractFacts("the user prefers tabs and hates trailing whitespace everywhere")
	if len(got) == 0 {
		t.Fatal("an empty model reply lost the fact entirely")
	}
}

func TestExtractFactsSkipsTheModelForOneShortSentence(t *testing.T) {
	// A write on an agent's hot path should not wait on a model to be told
	// that one short sentence is one fact.
	srv, seen := fakeOllama(t, "something else")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	got := c.ExtractFacts("user prefers tabs")
	if len(got) != 1 || got[0] != "user prefers tabs" {
		t.Fatalf("got %q", got)
	}
	if len(*seen) != 0 {
		t.Error("the model was consulted for a single short sentence")
	}
}

func TestExtractFactsStripsBulletMarkers(t *testing.T) {
	srv, _ := fakeOllama(t, "1. the first fact\n2. the second fact")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	got := c.ExtractFacts("a long enough statement that asserts two separate things here")
	if len(got) != 2 || strings.HasPrefix(got[0], "1.") {
		t.Fatalf("numbering not stripped: %q", got)
	}
}

func TestCustomPromptsAreAddedNotSubstituted(t *testing.T) {
	// An operator can bias what gets recorded; they must not be able to
	// replace the instructions that define the reply format, or the setting
	// would silently turn every write into a fallback.
	srv, seen := fakeOllama(t, "ADD")
	c := New(mapSettings{
		"ollama_url":            srv.URL,
		"memory_extract_prompt": "Only record facts about infrastructure.",
		"memory_decide_prompt":  "Be conservative about superseding.",
	}, nil)

	c.ExtractFacts("a long enough statement that asserts two separate things here")
	prompt := (*seen)[0]["prompt"].(string)
	if !strings.HasPrefix(prompt, "Only record facts about infrastructure.") {
		t.Errorf("custom extraction guidance not applied:\n%s", prompt)
	}
	if !strings.Contains(prompt, "One fact per line") {
		t.Errorf("custom guidance replaced the output contract:\n%s", prompt)
	}

	c.DecideMemory("a new fact", cands("an old fact"))
	prompt = (*seen)[1]["prompt"].(string)
	if !strings.HasPrefix(prompt, "Be conservative about superseding.") {
		t.Errorf("custom decision guidance not applied:\n%s", prompt)
	}
	for _, want := range []string{"UPDATE <n>", "NEW FACT:"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("custom guidance replaced %q:\n%s", want, prompt)
		}
	}
}

func TestNoCustomPromptAddsNothing(t *testing.T) {
	srv, seen := fakeOllama(t, "ADD")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	c.DecideMemory("a new fact", cands("an old fact"))
	if prompt := (*seen)[0]["prompt"].(string); !strings.HasPrefix(prompt, "You maintain") {
		t.Errorf("an unset prompt added something:\n%s", prompt)
	}
}

// --- grounded answers -------------------------------------------------------

// The verdict is the product of benchmarks/sufficiency: no retrieval statistic
// can tell an answerable question from an unanswerable one, so the judgement
// has to come from whatever reads the context — and it has to cost nothing
// extra, which means riding in the answer's own completion.

func TestAnswerReportsGroundedWhenTheNotesSupportIt(t *testing.T) {
	srv, seen := fakeOllama(t, "SUPPORTED: yes\nThe port is 8443 [1].")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	answer, support := c.AnswerGrounded("what port", []Context{
		{Path: "ops.md", Title: "Ops", Chunk: "port:: 8443"}})

	if support != SupportGrounded {
		t.Errorf("support = %v, want grounded", support)
	}
	if answer != "The port is 8443 [1]." {
		t.Errorf("the verdict line was left in the answer: %q", answer)
	}
	prompt := (*seen)[0]["prompt"].(string)
	// The verdict is asked for FIRST. After writing three confident sentences a
	// model rates its evidence to match them; asked first it is judging the
	// notes rather than defending a paragraph it already wrote.
	if !strings.Contains(prompt, "Begin your reply with") {
		t.Errorf("prompt does not ask for the verdict first:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Being on-topic is NOT support") {
		t.Errorf("prompt does not distinguish relevance from sufficiency:\n%s", prompt)
	}
}

func TestAnswerReportsUngroundedWhenTheNotesDoNot(t *testing.T) {
	srv, _ := fakeOllama(t, "SUPPORTED: no\nThe notes discuss the deploy but never give a port.")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	answer, support := c.AnswerGrounded("what port", []Context{
		{Path: "ops.md", Title: "Ops", Chunk: "we deployed on friday"}})
	if support != SupportUngrounded {
		t.Errorf("support = %v, want ungrounded", support)
	}
	if strings.Contains(answer, "SUPPORTED") {
		t.Errorf("verdict leaked into the answer: %q", answer)
	}
}

func TestVerdictParsingIsForgivingButNotCredulous(t *testing.T) {
	cases := map[string]Support{
		"SUPPORTED: yes\nanswer":  SupportGrounded,
		"supported: YES\nanswer":  SupportGrounded,
		"SUPPORTED: true\nanswer": SupportGrounded,
		"  SUPPORTED: no  \nnope": SupportUngrounded,
		"SUPPORTED: false\nnope":  SupportUngrounded,
		"SUPPORTED: no.\nnope":    SupportUngrounded,
		// No verdict line is UNKNOWN, not grounded. A model failing to follow
		// the format is not evidence that the notes contained the answer, and
		// upgrading it would make the signal least trustworthy exactly when
		// the model is least reliable.
		"The port is 8443.":                    SupportUnknown,
		"SUPPORTEDISH: yes\nx":                 SupportUnknown,
		"the answer is SUPPORTED: yes because": SupportUnknown,
		// What models actually write. Requiring the line to end after the
		// verdict rejected 31% of real replies from a 4B model, and the
		// measurement counted every one of them as "did not refuse" — a
		// parser bug that read as a model failure.
		"SUPPORTED: no \u2014 the notes mention it but never say when": SupportUngrounded,
		"SUPPORTED: yes \u2014 the notes state what was asked":         SupportGrounded,
		"**SUPPORTED: no** the notes are about the right topic":        SupportUngrounded,
	}
	for reply, want := range cases {
		got, support := splitVerdict(reply)
		if support != want {
			t.Errorf("%q -> %v, want %v", reply, support, want)
		}
		if strings.HasPrefix(got, "SUPPORTED:") {
			t.Errorf("%q left the verdict in the answer: %q", reply, got)
		}
	}
}

func TestVerdictKeepsTheReasoningAfterIt(t *testing.T) {
	// "the notes mention X but never state Y" is the most useful sentence in
	// the reply when the answer is that the notes do not say it.
	answer, support := splitVerdict(
		"SUPPORTED: no \u2014 the notes mention the deploy but never give a port.")
	if support != SupportUngrounded {
		t.Fatalf("support = %v", support)
	}
	if !strings.Contains(answer, "never give a port") {
		t.Errorf("the reasoning was stripped with the verdict: %q", answer)
	}
	if strings.Contains(answer, "SUPPORTED") {
		t.Errorf("the verdict token survived: %q", answer)
	}
}

func TestVerdictOnlyReplyStillAnswers(t *testing.T) {
	answer, support := splitVerdict("SUPPORTED: no")
	if support != SupportUngrounded {
		t.Fatalf("support = %v", support)
	}
	if answer == "" {
		t.Error("a verdict with no prose left the caller with nothing to show")
	}
}

func TestNoContextIsUngroundedWithoutAskingAModel(t *testing.T) {
	// The one case that needs no model to judge.
	srv, seen := fakeOllama(t, "SUPPORTED: yes\nmade up")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	_, support := c.AnswerGrounded("what port", nil)
	if support != SupportUngrounded {
		t.Errorf("support = %v, want ungrounded", support)
	}
	if len(*seen) != 0 {
		t.Error("a model was consulted about an empty context")
	}
}

func TestOfflineAnswerReportsUnknownRatherThanGrounded(t *testing.T) {
	// The extractive floor quotes passages; it does not judge them. Claiming a
	// verdict it never made would be the one failure that matters here.
	c := New(mapSettings{}, nil)
	answer, support := c.AnswerGrounded("what port", []Context{
		{Path: "ops.md", Title: "Ops", Chunk: "port:: 8443"}})
	if support != SupportUnknown {
		t.Errorf("support = %v, want unknown", support)
	}
	if answer == "" {
		t.Error("the offline floor stopped answering")
	}
}

func TestAnswerKeepsItsOldSignature(t *testing.T) {
	// Answer is on the console's path and in the MCP tools; the verdict is
	// additive, not a breaking change.
	srv, _ := fakeOllama(t, "SUPPORTED: yes\nThe port is 8443 [1].")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	if got := c.Answer("what port", []Context{{Title: "Ops", Chunk: "port:: 8443"}}); got !=
		"The port is 8443 [1]." {
		t.Errorf("Answer = %q", got)
	}
}
