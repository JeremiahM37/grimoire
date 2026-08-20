package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CLI is the third way into agent memory, after the console and MCP — the
// one a person uses to check what an agent believes and correct it. These
// drive the commands and then assert on the note file, the way someone would.

func memoryNote(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "memory", name))
	if err != nil {
		t.Fatalf("reading memory note: %v", err)
	}
	return string(raw)
}

func TestRememberWritesAFact(t *testing.T) {
	dir := vaultDir(t)
	out, code := runCmd(t, "remember", "the deploy needs a VPN reset", "--topic", "ops")
	if code != 0 {
		t.Fatalf("remember = %d: %s", code, out)
	}
	if !strings.Contains(out, "remembered") {
		t.Errorf("output = %q", out)
	}
	if body := memoryNote(t, dir, "ops.md"); !strings.Contains(body, "the deploy needs a VPN reset") {
		t.Errorf("fact not written:\n%s", body)
	}
}

func TestRememberDoesNotSwallowFlagsIntoTheFact(t *testing.T) {
	// `grimoire remember the box is fast --topic ops` must not remember
	// "--topic ops" as part of what it learned.
	dir := vaultDir(t)
	runCmd(t, "remember", "the box", "is", "fast", "--topic", "ops", "--session", "run-1")
	body := memoryNote(t, dir, "ops.md")
	if strings.Contains(body, "--topic") || strings.Contains(body, "run-1 ") {
		t.Errorf("a flag leaked into the fact:\n%s", body)
	}
	if !strings.Contains(body, "the box is fast") {
		t.Errorf("fact mangled:\n%s", body)
	}
	if !strings.Contains(body, "session=run-1") {
		t.Errorf("session flag was not applied:\n%s", body)
	}
}

func TestRememberReportsSupersession(t *testing.T) {
	// A write that changed an existing belief must not read as a plain
	// success — that is the whole difference from an append-only store.
	dir := vaultDir(t)
	runCmd(t, "remember", "the user prefers spaces", "--topic", "prefs")
	out, code := runCmd(t, "remember", "the user prefers tabs", "--topic", "prefs")
	if code != 0 {
		t.Fatalf("remember = %d: %s", code, out)
	}
	if !strings.Contains(out, "superseded") {
		t.Errorf("output did not report the supersession: %q", out)
	}
	if !strings.Contains(memoryNote(t, dir, "prefs.md"), "~~") {
		t.Error("the old belief was not struck through")
	}
}

func TestRememberReportsANoOp(t *testing.T) {
	vaultDir(t)
	runCmd(t, "remember", "the deploy needs a VPN reset", "--topic", "ops")
	out, _ := runCmd(t, "remember", "The deploy needs a VPN reset.", "--topic", "ops")
	if !strings.Contains(out, "unchanged") {
		t.Errorf("a restatement did not report as unchanged: %q", out)
	}
}

func TestRememberVerbatimSkipsReconciliation(t *testing.T) {
	vaultDir(t)
	runCmd(t, "remember", "the user prefers spaces", "--topic", "raw")
	out, _ := runCmd(t, "remember", "the user prefers tabs", "--topic", "raw", "--verbatim")
	if !strings.Contains(out, "remembered") {
		t.Errorf("--verbatim reconciled anyway: %q", out)
	}
}

func TestRecallShowsWhatIsBelieved(t *testing.T) {
	vaultDir(t)
	runCmd(t, "remember", "the user prefers spaces", "--topic", "prefs")
	runCmd(t, "remember", "the user prefers tabs", "--topic", "prefs")

	out, code := runCmd(t, "recall", "indentation")
	if code != 0 {
		t.Fatalf("recall = %d: %s", code, out)
	}
	if !strings.Contains(out, "prefers tabs") {
		t.Errorf("current belief missing: %q", out)
	}
	if strings.Contains(out, "prefers spaces") {
		t.Errorf("a superseded belief was listed: %q", out)
	}
	// …and --all shows the history, marked.
	out, _ = runCmd(t, "recall", "--all")
	if !strings.Contains(out, "prefers spaces") || !strings.Contains(out, "×") {
		t.Errorf("--all did not show the replaced belief: %q", out)
	}
}

func TestRecallOnEmptyMemorySaysSo(t *testing.T) {
	vaultDir(t)
	out, code := runCmd(t, "recall")
	if code != 0 {
		t.Fatalf("recall = %d: %s", code, out)
	}
	if !strings.Contains(out, "nothing recorded") {
		t.Errorf("output = %q", out)
	}
}

func TestRecallExplainsItsRanking(t *testing.T) {
	vaultDir(t)
	runCmd(t, "remember", "Priya owns the deploy script", "--topic", "team")
	out, _ := runCmd(t, "recall", "who owns the deploy script", "--why")
	if !strings.Contains(out, "semantic") || !strings.Contains(out, "score") {
		t.Errorf("--why showed no breakdown: %q", out)
	}
}

func TestRecallFiltersByScope(t *testing.T) {
	vaultDir(t)
	runCmd(t, "remember", "alice owns the release", "--topic", "w", "--session", "run-1")
	runCmd(t, "remember", "the build takes nine minutes", "--topic", "w", "--session", "run-2")

	out, _ := runCmd(t, "recall", "--session", "run-2")
	if !strings.Contains(out, "nine minutes") {
		t.Errorf("session filter lost the fact: %q", out)
	}
	if strings.Contains(out, "alice owns") {
		t.Errorf("session filter leaked another run: %q", out)
	}
}

func TestForgetRetractsAndHardForgetRemoves(t *testing.T) {
	dir := vaultDir(t)
	runCmd(t, "remember", "a mistaken belief", "--topic", "prefs")
	listing, _ := runCmd(t, "recall")
	id := firstID(t, listing)

	out, code := runCmd(t, "forget", "memory/prefs.md", id)
	if code != 0 {
		t.Fatalf("forget = %d: %s", code, out)
	}
	if !strings.Contains(memoryNote(t, dir, "prefs.md"), "a mistaken belief") {
		t.Error("a soft retraction deleted the record")
	}
	if after, _ := runCmd(t, "recall"); strings.Contains(after, "a mistaken belief") {
		t.Errorf("a retracted fact is still recalled: %q", after)
	}

	runCmd(t, "remember", "something private", "--topic", "prefs")
	listing, _ = runCmd(t, "recall")
	out, code = runCmd(t, "forget", "memory/prefs.md", firstID(t, listing), "--hard")
	if code != 0 {
		t.Fatalf("hard forget = %d: %s", code, out)
	}
	if strings.Contains(memoryNote(t, dir, "prefs.md"), "something private") {
		t.Error("hard forget left the text behind")
	}
}

func TestForgetUsageAndUnknownID(t *testing.T) {
	vaultDir(t)
	runCmd(t, "remember", "a fact", "--topic", "prefs")
	if _, code := runCmd(t, "forget", "memory/prefs.md"); code == 0 {
		t.Error("forget with no id succeeded")
	}
	if _, code := runCmd(t, "forget", "memory/prefs.md", "nope"); code == 0 {
		t.Error("forget with an unknown id succeeded")
	}
}

func TestRememberWithNoTextFails(t *testing.T) {
	vaultDir(t)
	if _, code := runCmd(t, "remember"); code == 0 {
		t.Error("remember with no text succeeded")
	}
}

// firstID pulls the id out of the first line of a recall listing, which is how
// a person gets one to pass to `forget`.
func firstID(t *testing.T, listing string) string {
	t.Helper()
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		// "  <id>  <text…>" — the mark column is empty for a live fact, so the
		// id is the first field.
		if len(fields) >= 2 && len(fields[0]) == 12 {
			return fields[0]
		}
	}
	t.Fatalf("no id in listing:\n%s", listing)
	return ""
}
