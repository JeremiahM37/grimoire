package bank

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleTurns() []SessionTurn {
	return []SessionTurn{
		{Speaker: "user", Text: "Why does the build fail on CI? <private>my token is hunter22</private>", Timestamp: "2026-10-07T10:00:00Z"},
		{Speaker: "assistant", Text: "The cause is a stale cache because the key ignores the lockfile. I fixed the cache key in ci.yml. Next we should add a regression test.", Timestamp: "2026-10-07T10:05:00Z"},
	}
}

func writeDigest(t *testing.T, h *harness, in DigestInput) *DigestResult {
	t.Helper()
	if in.SessionID == "" {
		in.SessionID = "s1"
	}
	res, err := h.e.WriteDigest(context.Background(), "b", in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRuleDigestNeedsNoModelAndHasFourParts(t *testing.T) {
	h := newHarness(t, false)
	exit := 1
	res := writeDigest(t, h, DigestInput{Turns: sampleTurns(), UseModel: true,
		Activity: SessionActivity{Files: []string{"ci.yml"}, Commands: []CommandRun{{Command: "make test", Exit: &exit}}}})
	if !res.Written || res.Method != "rules" {
		t.Fatalf("res=%+v", res)
	}
	note := h.read(t, res.Path)
	for _, want := range []string{"## Request", "Why does the build fail", "## Learned", "stale cache", "## Done", "ci.yml",
		"Command failed (exit 1): make test", "## Next", "regression test", digestStart, digestEnd, "type: session-digest"} {
		if !strings.Contains(note, want) {
			t.Errorf("digest lacks %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "hunter22") {
		t.Fatal("private text reached the digest")
	}
}

func TestModelDigestIsUsedWhenConfiguredAndFallsBackOnError(t *testing.T) {
	h := newHarness(t, true)
	h.llm.reply = func(string) (string, string) {
		return `{"request":"Fix CI","learned":["Cache key ignored the lockfile"],"done":["Changed ci.yml"],"next":["Add a test"]}`, "stop"
	}
	res := writeDigest(t, h, DigestInput{Turns: sampleTurns(), UseModel: true})
	if res.Method != "model" || !strings.Contains(h.read(t, res.Path), "Cache key ignored the lockfile") {
		t.Fatalf("res=%+v", res)
	}
	h.llm.reply = func(string) (string, string) { return "not json", "stop" }
	res = writeDigest(t, h, DigestInput{SessionID: "s2", Turns: sampleTurns(), UseModel: true})
	if res.Method != "rules" || !res.Written {
		t.Fatalf("a failing model must fall back to rules: %+v", res)
	}
	// Without UseModel there is no call at all.
	before := h.llm.calls.Load()
	writeDigest(t, h, DigestInput{SessionID: "s3", Turns: sampleTurns()})
	if h.llm.calls.Load() != before {
		t.Fatal("a digest without UseModel called the model")
	}
}

func TestDigestRegenerationKeepsAPersonsWords(t *testing.T) {
	h := newHarness(t, false)
	res := writeDigest(t, h, DigestInput{Turns: sampleTurns()})
	// Text outside the markers survives a regeneration.
	h.editFile(t, res.Path, "Your own notes go here; they are never overwritten.", "Remember to ask Dana about the cache.")
	more := append(sampleTurns(), SessionTurn{Speaker: "assistant", Text: "I also added the regression test, and the suite is passing."})
	res = writeDigest(t, h, DigestInput{Turns: more})
	note := h.read(t, res.Path)
	if !res.Written || !strings.Contains(note, "Remember to ask Dana") || !strings.Contains(note, "regression test, and the suite") {
		t.Fatalf("res=%+v\n%s", res, note)
	}
	// Text edited inside the markers pins the digest: it is left exactly as is.
	h.editFile(t, res.Path, "## Request", "## Request\n\nMY OWN WORDS")
	pinned := h.read(t, res.Path)
	res = writeDigest(t, h, DigestInput{Turns: append(more, SessionTurn{Speaker: "assistant", Text: "Something entirely new happened."})})
	if res.Written || !res.Pinned || h.read(t, res.Path) != pinned {
		t.Fatalf("a person's edit must win: %+v", res)
	}
	// Removing the markers hands the whole note to the person.
	h.editFile(t, res.Path, digestStart, "")
	res = writeDigest(t, h, DigestInput{Turns: more})
	if res.Written || !res.Pinned {
		t.Fatalf("res=%+v", res)
	}
}

func TestUnchangedInputDoesNotRewriteOrCallTheModelAgain(t *testing.T) {
	h := newHarness(t, true)
	h.llm.reply = func(string) (string, string) { return `{"request":"x","learned":[],"done":["y"],"next":[]}`, "stop" }
	writeDigest(t, h, DigestInput{Turns: sampleTurns(), UseModel: true})
	calls := h.llm.calls.Load()
	res := writeDigest(t, h, DigestInput{Turns: sampleTurns(), UseModel: true})
	if res.Written || res.Skipped != "unchanged" || h.llm.calls.Load() != calls {
		t.Fatalf("res=%+v calls %d->%d", res, calls, h.llm.calls.Load())
	}
}

func TestLatestDigestLeadsTheSessionContext(t *testing.T) {
	h := newHarness(t, false)
	h.retain(t, "b", Item{Content: "The cache is keyed by commit.", DocumentID: "d"})
	writeDigest(t, h, DigestInput{SessionID: "old", Turns: []SessionTurn{
		{Speaker: "user", Text: "Older task entirely about logging.", Timestamp: "2026-09-01T10:00:00Z"}}})
	writeDigest(t, h, DigestInput{SessionID: "new", Turns: sampleTurns()})
	for _, source := range []string{"startup", "resume", "clear", "compact"} {
		ctx, err := h.e.SessionContext("b", ContextOptions{Source: source})
		if err != nil {
			t.Fatal(err)
		}
		i, j := strings.Index(ctx.Context, "Where we left off"), strings.Index(ctx.Context, "Recent facts")
		if i < 0 || j < 0 || i > j || !strings.Contains(ctx.Context, "Why does the build fail") || strings.Contains(ctx.Context, "Older task") {
			t.Fatalf("%s:\n%s", source, ctx.Context)
		}
	}
	// Under a tight limit the digest outlives the facts.
	tight, _ := h.e.SessionContext("b", ContextOptions{MaxChars: 700})
	if !strings.Contains(tight.Context, "Where we left off") || tight.Chars > 700 {
		t.Fatalf("tight:\n%s", tight.Context)
	}
}

func TestDigestStaysInsideTheBankFolder(t *testing.T) {
	h := newHarness(t, false)
	res := writeDigest(t, h, DigestInput{SessionID: "../../escape", Turns: sampleTurns()})
	if !strings.HasPrefix(res.Path, "banks/b/sessions/") {
		t.Fatalf("path %q", res.Path)
	}
	if _, err := os.Stat(filepath.Join(h.root, "escape.md")); err == nil {
		t.Fatal("escaped the vault")
	}
}
