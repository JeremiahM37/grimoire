package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Anchor arbitration: a write may not replace a fact whose stored anchors still
// hold. A person's correction is never blocked by it.

// portFile is a temp file holding one literal; the test edits it to make an
// anchor on the old fact pass or fail.
func portFile(t *testing.T, port string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gateway.conf")
	if err := os.WriteFile(p, []byte("KESTREL_PORT="+port+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// entriesOf returns the entries in a memory note.
func entriesOf(t *testing.T, h http.Handler, note string) []memory.Entry {
	t.Helper()
	return memory.Parse(noteBody(t, h, note))
}

func entryByText(t *testing.T, h http.Handler, note, text string) memory.Entry {
	t.Helper()
	for _, e := range entriesOf(t, h, note) {
		if e.Text == text {
			return e
		}
	}
	t.Fatalf("no entry %q in %s", text, note)
	return memory.Entry{}
}

const (
	kestrelOld = "kestrel gateway port is 9000"
	kestrelNew = "kestrel gateway port is 9001"
)

// An unanchored fact is superseded exactly as it was before arbitration existed.
func TestUnanchoredOldFactIsSupersededAsBefore(t *testing.T) {
	_, h := testServer(t)
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": kestrelOld})
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": kestrelNew})
	if out["op"] != "UPDATE" || out["kept"] != nil {
		t.Fatalf("unanchored old fact: op=%v kept=%v, want UPDATE and no kept", out["op"], out["kept"])
	}
	if old := entryByText(t, h, "memory/net.md", kestrelOld); old.SupersededBy == "" {
		t.Fatalf("old fact not superseded:\n%s", noteBody(t, h, "memory/net.md"))
	}
}

func TestAnchoredOldFactThatStillHoldsIsKeptBeside(t *testing.T) {
	_, h := testServer(t)
	lit := portFile(t, "9000")
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": kestrelOld,
		"anchors": []map[string]any{{"type": "file_contains", "path": lit, "text": "KESTREL_PORT=9000"}}})

	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": kestrelNew})
	if out["op"] != "ADD" {
		t.Fatalf("op = %v, want ADD (kept beside)", out["op"])
	}
	kept, _ := out["kept"].([]any)
	if len(kept) != 1 {
		t.Fatalf("kept = %v, want one entry", out["kept"])
	}
	k := kept[0].(map[string]any)
	old := entryByText(t, h, "memory/net.md", kestrelOld)
	if k["id"] != old.ID || k["reason"] != "its anchors still pass" {
		t.Fatalf("kept entry = %v, want id %s", k, old.ID)
	}
	want := "contradicts a stored fact whose check still passes (" + old.ID + "): check which is right, and anchor this claim if it is"
	advice, _ := out["advice"].([]any)
	found := false
	for _, a := range advice {
		if a == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("advice = %v, want the kept-contradiction line for %s", out["advice"], old.ID)
	}
	if old.SupersededBy != "" {
		t.Fatalf("old fact was superseded (by %q) although its anchor passes", old.SupersededBy)
	}
	if n := entryByText(t, h, "memory/net.md", kestrelNew); n.SupersededBy != "" || n.Challenges != "" {
		t.Fatalf("new fact should sit beside the old one unmarked: %+v", n)
	}
}

func TestAnchoredOldFactThatFailsIsSuperseded(t *testing.T) {
	_, h := testServer(t)
	lit := portFile(t, "9000")
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": kestrelOld,
		"anchors": []map[string]any{{"type": "file_contains", "path": lit, "text": "KESTREL_PORT=9000"}}})
	// The world moved on: the literal the claim pointed at is gone.
	if err := os.WriteFile(lit, []byte("KESTREL_PORT=9001\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": kestrelNew})
	if out["op"] != "UPDATE" || out["kept"] != nil {
		t.Fatalf("failing anchor: op=%v kept=%v, want UPDATE", out["op"], out["kept"])
	}
	if old := entryByText(t, h, "memory/net.md", kestrelOld); old.SupersededBy == "" {
		t.Fatalf("old fact with a failing anchor was not superseded")
	}
}

// Only automatic anchors (weak: they say a file exists) do not protect a fact.
func TestAutoOnlyAnchorsDoNotProtectAFact(t *testing.T) {
	s, h := testServer(t)
	lit := portFile(t, "9000")
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": kestrelOld})
	storeAnchor(t, s, "memory/net.md", kestrelOld, "file_exists;path="+lit+";auto=1")

	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": kestrelNew})
	if out["op"] != "UPDATE" {
		t.Fatalf("an auto-only anchor kept the old fact: op=%v", out["op"])
	}
}

// A person's correction replaces the fact even when its anchors pass.
func TestHumanCorrectionSupersedesEvenWhenAnchorsPass(t *testing.T) {
	_, h := testServer(t)
	lit := portFile(t, "9000")
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": kestrelOld,
		"anchors": []map[string]any{{"type": "file_contains", "path": lit, "text": "KESTREL_PORT=9000"}}})
	old := entryByText(t, h, "memory/net.md", kestrelOld)

	out := rememberFrom(t, h, anchorLoopback, map[string]any{
		"text": kestrelNew, "human": true,
		"target_id": old.ID, "target_path": "memory/net.md", "expected_text": kestrelOld})
	if out["op"] != "UPDATE" || out["kept"] != nil {
		t.Fatalf("human correction: op=%v kept=%v, want UPDATE", out["op"], out["kept"])
	}
	if got := entryByText(t, h, "memory/net.md", kestrelOld); got.SupersededBy == "" {
		t.Fatalf("human correction did not supersede the anchored fact")
	}
}

// The same explicit correction from an agent is arbitrated like any other write.
func TestAgentTargetedCorrectionIsArbitrated(t *testing.T) {
	_, h := testServer(t)
	lit := portFile(t, "9000")
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": kestrelOld,
		"anchors": []map[string]any{{"type": "file_contains", "path": lit, "text": "KESTREL_PORT=9000"}}})
	old := entryByText(t, h, "memory/net.md", kestrelOld)

	out := rememberFrom(t, h, anchorLoopback, map[string]any{
		"text":      kestrelNew,
		"target_id": old.ID, "target_path": "memory/net.md", "expected_text": kestrelOld})
	if out["op"] != "ADD" || out["kept"] == nil {
		t.Fatalf("agent correction of a passing anchored fact: op=%v kept=%v", out["op"], out["kept"])
	}
}

func TestArbitrationOffSwitchRestoresReplacement(t *testing.T) {
	t.Setenv("GRIMOIRE_ANCHOR_ARBITRATE", "0")
	_, h := testServer(t)
	lit := portFile(t, "9000")
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": kestrelOld,
		"anchors": []map[string]any{{"type": "file_contains", "path": lit, "text": "KESTREL_PORT=9000"}}})
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": kestrelNew})
	if out["op"] != "UPDATE" {
		t.Fatalf("GRIMOIRE_ANCHOR_ARBITRATE=0: op=%v, want UPDATE", out["op"])
	}
}
