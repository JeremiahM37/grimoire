package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// seedSearchVault writes two notes whose bodies contain the word "deploy".
func seedSearchVault(t *testing.T) {
	t.Helper()
	vaultDir(t)
	if _, code := runCmd(t, "new", "Deploy Runbook",
		"Rolling   restarts are unsafe.\n\nDo a full deploy with --force-recreate."); code != 0 {
		t.Fatal("new failed")
	}
	if _, code := runCmd(t, "new", "Garden Notes", "tomatoes need sun"); code != 0 {
		t.Fatal("new failed")
	}
}

func TestSearchJSONIsTheRawHitArray(t *testing.T) {
	seedSearchVault(t)
	out, code := runCmd(t, "search", "--json", "deploy")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var hits []struct {
		Path    string `json:"path"`
		Title   string `json:"title"`
		Snippet string `json:"snippet"`
	}
	if err := json.Unmarshal([]byte(out), &hits); err != nil {
		t.Fatalf("--json output is not a JSON array: %v\n%s", err, out)
	}
	if len(hits) != 1 || hits[0].Path != "deploy-runbook.md" || hits[0].Title != "Deploy Runbook" {
		t.Fatalf("unexpected hits: %+v", hits)
	}
	if !strings.Contains(hits[0].Snippet, "[deploy]") {
		t.Errorf("snippet should mark the matched term: %q", hits[0].Snippet)
	}
}

func TestSearchJSONWithNoHitsIsAnEmptyArray(t *testing.T) {
	seedSearchVault(t)
	out, code := runCmd(t, "search", "--json", "zzzqqq")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("want [], got %q", out)
	}
}

func TestSearchPrintsASnippetUnderEachHitWithoutColourWhenPiped(t *testing.T) {
	seedSearchVault(t)
	out, code := runCmd(t, "search", "deploy")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want a hit line and a snippet line, got:\n%s", out)
	}
	if !strings.HasPrefix(lines[0], "deploy-runbook.md") || !strings.HasSuffix(lines[0], "Deploy Runbook") {
		t.Errorf("hit line: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "    ") || !strings.Contains(lines[1], "[deploy]") {
		t.Errorf("snippet line should be indented and keep its match marks: %q", lines[1])
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("piped output must carry no escape codes: %q", out)
	}
}

func TestSearchLimitFlag(t *testing.T) {
	vaultDir(t)
	for _, title := range []string{"Alpha Deploy", "Beta Deploy"} {
		if _, code := runCmd(t, "new", title, "deploy steps"); code != 0 {
			t.Fatal("new failed")
		}
	}
	out, code := runCmd(t, "search", "-n", "1", "--json", "deploy")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var hits []map[string]any
	if err := json.Unmarshal([]byte(out), &hits); err != nil || len(hits) != 1 {
		t.Fatalf("-n 1 should return one hit, got %d (%v)", len(hits), err)
	}
	if _, code := runCmd(t, "search", "-n", "0", "deploy"); code == 0 {
		t.Error("-n 0 should be refused")
	}
	if _, code := runCmd(t, "search", "-n", "many", "deploy"); code == 0 {
		t.Error("a non-numeric -n should be refused")
	}
}

func TestLsJSONListsEveryNote(t *testing.T) {
	seedSearchVault(t)
	out, code := runCmd(t, "ls", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var notes []struct {
		Path  string `json:"path"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(out), &notes); err != nil {
		t.Fatalf("ls --json is not a JSON array: %v\n%s", err, out)
	}
	if len(notes) != 2 {
		t.Fatalf("want 2 notes, got %+v", notes)
	}
}

func TestRenderSnippetCollapsesWhitespaceAndTruncates(t *testing.T) {
	got := renderSnippet("one\n\n  two\tthree", false)
	if got != "one two three" {
		t.Errorf("whitespace not collapsed: %q", got)
	}
	long := strings.Repeat("word ", 60)
	cut := renderSnippet(long, false)
	if n := utf8.RuneCountInString(cut); n > snippetWidth {
		t.Errorf("snippet is %d runes, budget is %d", n, snippetWidth)
	}
	if !strings.HasSuffix(cut, "…") {
		t.Errorf("a truncated snippet should end with an ellipsis: %q", cut)
	}
}

func TestRenderSnippetMarksOnlyWhenColoured(t *testing.T) {
	const s = "the [deploy] step"
	if got := renderSnippet(s, false); got != s {
		t.Errorf("plain render should keep the brackets: %q", got)
	}
	got := renderSnippet(s, true)
	if got != "the "+ansiBold+"deploy"+ansiReset+" step" {
		t.Errorf("coloured render: %q", got)
	}
}

func TestRenderSnippetLeavesACutTermPlain(t *testing.T) {
	// A truncation that lands inside a bracketed term must not leave a stray
	// bracket or bold an unterminated span.
	s := strings.Repeat("x ", 49) + "[partial"
	got := renderSnippet(s, true)
	if strings.Contains(got, ansiBold) || strings.Contains(got, "[") {
		t.Errorf("unterminated match should print plain, without its opener: %q", got)
	}
}

func TestColourHelperHonoursNoColourAndNonTerminals(t *testing.T) {
	// A pipe is never a terminal, so colour is off whatever the environment says.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	if colourAllowed(w) {
		t.Error("colour on a pipe")
	}

	// /dev/null is a character device but not a terminal: a redirect there gets
	// plain text too.
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skip("no /dev/null:", err)
	}
	defer null.Close()
	if colourAllowed(null) {
		t.Error("colour on /dev/null")
	}
}

// The environment vetoes are checked before the terminal is consulted, so they
// hold on a real terminal as well; a test has no terminal to prove that on, so
// this pins the order instead.
func TestColourEnvironmentVetoes(t *testing.T) {
	for _, env := range [][2]string{{"NO_COLOR", "1"}, {"TERM", "dumb"}} {
		t.Setenv("NO_COLOR", "")
		t.Setenv("TERM", "xterm-256color")
		t.Setenv(env[0], env[1])
		if colourAllowed(os.Stdout) {
			t.Errorf("%s=%s did not veto colour", env[0], env[1])
		}
	}
}

func TestStyledIsANoOpWhenOff(t *testing.T) {
	if got := styled(false, ansiDim, "path"); got != "path" {
		t.Errorf("styled off: %q", got)
	}
	if got := styled(true, ansiDim, "path"); got != ansiDim+"path"+ansiReset {
		t.Errorf("styled on: %q", got)
	}
}
