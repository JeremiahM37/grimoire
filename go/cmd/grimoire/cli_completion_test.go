package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestLevenshteinDistances(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"abc", "abc", 0},
		{"serach", "search", 2},
		{"kitten", "sitting", 3},
		{"", "abc", 3},
		{"ned", "new", 1},
	}
	for _, c := range cases {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestDidYouMeanSuggestsTheNearestCommand(t *testing.T) {
	var handled bool
	var code int
	stderr := captureStderr(t, func() {
		handled, code = runCLI([]string{"serach"})
	})
	if !handled || code == 0 {
		t.Fatalf("an unknown command must stay non-zero: handled=%v code=%d", handled, code)
	}
	if !strings.Contains(stderr, `unknown command "serach" — did you mean "search"?`) {
		t.Errorf("missing suggestion:\n%s", stderr)
	}
	// A near miss is answered in one line: the full command list is for a word
	// that resembles nothing.
	if strings.Contains(stderr, "Try: ") || strings.Count(strings.TrimSpace(stderr), "\n") != 0 {
		t.Errorf("a near miss should print only the suggestion:\n%s", stderr)
	}
}

func TestNoSuggestionWhenNothingIsClose(t *testing.T) {
	stderr := captureStderr(t, func() {
		if handled, code := runCLI([]string{"qqqqqqqqqq"}); !handled || code == 0 {
			t.Errorf("handled=%v code=%d", handled, code)
		}
	})
	if strings.Contains(stderr, "did you mean") {
		t.Errorf("suggested something for a word nothing resembles:\n%s", stderr)
	}
}

func TestSuggestCommandRespectsTheTwoEditLimit(t *testing.T) {
	names := []string{"ls", "search", "serve"}
	if got := suggestCommand("serach", names); got != "search" {
		t.Errorf("serach -> %q", got)
	}
	if got := suggestCommand("sxxxxxh", names); got != "" {
		t.Errorf("distance above two should give no suggestion, got %q", got)
	}
}

// Every command word must appear, as a whole word, in each shell's script.
func TestCompletionCoversEveryCommandForEachShell(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		out := completionOutput(t, shell)
		if !strings.HasPrefix(out, "# "+shell+" completion for grimoire") {
			t.Errorf("%s: missing header comment:\n%s", shell, out)
		}
		for name := range commands() {
			re := regexp.MustCompile(`(^|[\s"(])` + regexp.QuoteMeta(name) + `([\s")]|$)`)
			if !re.MatchString(out) {
				t.Errorf("%s completion does not offer %q", shell, name)
			}
		}
	}
}

func TestCompletionRejectsAnUnknownShell(t *testing.T) {
	var code int
	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			_, code = runCLI([]string{"completion", "tcsh"})
		})
	})
	if code == 0 || !strings.Contains(stderr, "usage: grimoire completion") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
}

// The scripts must at least parse in their shell.
func TestCompletionScriptsParse(t *testing.T) {
	dir := t.TempDir()
	for shell, checker := range map[string][]string{
		"bash": {"bash", "-n"},
		"zsh":  {"zsh", "-n"},
	} {
		if _, err := exec.LookPath(checker[0]); err != nil {
			t.Logf("%s not installed; skipping its syntax check", checker[0])
			continue
		}
		path := filepath.Join(dir, shell+".sh")
		if err := os.WriteFile(path, []byte(completionOutput(t, shell)), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(checker[0], checker[1], path).CombinedOutput(); err != nil {
			t.Errorf("%s -n rejects the %s script: %v\n%s", checker[0], shell, err, out)
		}
	}
}

// Runs the bash script for real: after "grimoire se", the completer must offer
// the commands that start with "se".
func TestBashCompletionOffersMatchingCommands(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	script := filepath.Join(t.TempDir(), "grimoire.bash")
	if err := os.WriteFile(script, []byte(completionOutput(t, "bash")), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := `source "$1"
COMP_WORDS=(grimoire se); COMP_CWORD=1; _grimoire_complete; printf '%s\n' "${COMPREPLY[@]}"`
	out, err := exec.Command(bash, "-c", probe, "--", script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	got := map[string]bool{}
	for _, line := range strings.Fields(string(out)) {
		got[line] = true
	}
	if !got["search"] || !got["serve"] || got["new"] {
		t.Errorf("completions for 'se': %v", got)
	}
}

func completionOutput(t *testing.T, shell string) string {
	t.Helper()
	var code int
	out := captureStdout(t, func() {
		_, code = runCLI([]string{"completion", shell})
	})
	if code != 0 {
		t.Fatalf("completion %s exit %d", shell, code)
	}
	return out
}
