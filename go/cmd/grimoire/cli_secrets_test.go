package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// ------------------------------------------------------------- argument parsing

func TestFlagParsingHandlesTheFormsPeopleActuallyType(t *testing.T) {
	rest, flags := parseSecretFlags([]string{
		"MYKEY", "--note", "billing api", "--expires=2027-01-01", "--all", "extra"})
	if len(rest) != 2 || rest[0] != "MYKEY" || rest[1] != "extra" {
		t.Errorf("positional args = %v, want [MYKEY extra]", rest)
	}
	for k, want := range map[string]string{
		"note": "billing api", "expires": "2027-01-01", "all": "true",
	} {
		if flags[k] != want {
			t.Errorf("--%s = %q, want %q", k, flags[k], want)
		}
	}
}

// A bare flag followed by another flag must not swallow it as its value, or
// `--all --verbose` silently means `--all=--verbose` and --verbose is lost.
func TestABareFlagDoesNotEatTheNextFlag(t *testing.T) {
	_, flags := parseSecretFlags([]string{"--all", "--reveal"})
	if flags["all"] != "true" || flags["reveal"] != "true" {
		t.Errorf("flags = %v, want both boolean", flags)
	}
}

// The case that made the boolean set necessary: `--all` must not consume the
// argument after it, or naming secrets alongside it silently loses them.
func TestABooleanFlagNeverConsumesAPositional(t *testing.T) {
	rest, flags := parseSecretFlags([]string{"--all", "DEMO"})
	if flags["all"] != "true" {
		t.Errorf("--all = %q, want true", flags["all"])
	}
	if len(rest) != 1 || rest[0] != "DEMO" {
		t.Errorf("positional = %v, want [DEMO] — a boolean flag that ate it "+
			"would leave the caller with an empty list and no error", rest)
	}
}

func TestSplitAtDoubleDash(t *testing.T) {
	before, after := splitAtDoubleDash([]string{"A,B", "--", "sh", "-c", "echo hi"})
	if len(before) != 1 || before[0] != "A,B" {
		t.Errorf("before = %v", before)
	}
	if len(after) != 3 || after[0] != "sh" {
		t.Errorf("after = %v", after)
	}
	// No separator means no command, which the caller reports as usage rather
	// than running the first argument.
	if _, after := splitAtDoubleDash([]string{"A", "B"}); after != nil {
		t.Errorf("after = %v, want nil with no separator", after)
	}
}

func TestEnvNameIsAConventionalVariable(t *testing.T) {
	for in, want := range map[string]string{
		"stripe":         "STRIPE",
		"github-token":   "GITHUB_TOKEN",
		"my.api/key":     "MY_API_KEY",
		"already_fine_2": "ALREADY_FINE_2",
	} {
		if got := envName(in); got != want {
			t.Errorf("envName(%q) = %q, want %q", in, got, want)
		}
	}
}

// An exported value goes back into a shell, so a quote in it must not end the
// string it is in.
func TestShellQuoteSurvivesAQuoteInTheValue(t *testing.T) {
	got := shellQuote(`pa'ss`)
	if got != `'pa'"'"'ss'` {
		t.Errorf("shellQuote = %s", got)
	}
}

// ------------------------------------------------------------- dotenv import

func openCLIVault(t *testing.T) *secrets.Vault {
	t.Helper()
	dir := t.TempDir()
	v := secrets.New(dir)
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDotenvImportTakesTheShapesRealFilesHave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := strings.Join([]string{
		"# a comment",
		"",
		"PLAIN=value1",
		`QUOTED="value 2"`,
		"SINGLE='value3'",
		"export EXPORTED=value4",
		"  SPACED = value5 ",
		"NOEQUALS",
		"EMPTY=",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	v := openCLIVault(t)
	if code := importDotenv(v, path); code != 0 {
		t.Fatalf("import exited %d", code)
	}
	for name, want := range map[string]string{
		"PLAIN": "value1", "QUOTED": "value 2", "SINGLE": "value3",
		"EXPORTED": "value4", "SPACED": "value5",
	} {
		got, err := v.Get(name)
		if err != nil {
			t.Errorf("%s missing: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// A line with no '=' and one with no value are skipped rather than stored
	// as empty secrets, which would look like a working credential.
	if _, err := v.Get("NOEQUALS"); err == nil {
		t.Error("a line with no '=' was stored")
	}
	if _, err := v.Get("EMPTY"); err == nil {
		t.Error("an empty value was stored as a secret")
	}
}

func TestImportedSecretsAreVersionedLikeAnyOther(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	v := openCLIVault(t)

	os.WriteFile(path, []byte("K=first\n"), 0o600)
	importDotenv(v, path)
	os.WriteFile(path, []byte("K=second\n"), 0o600)
	importDotenv(v, path)

	if got, _ := v.Get("K"); got != "second" {
		t.Errorf("value = %q, want second", got)
	}
	vers, err := v.Versions("K")
	if err != nil {
		t.Fatal(err)
	}
	if len(vers) != 1 {
		t.Errorf("%d versions after re-import, want 1 — a bulk import that "+
			"overwrote without history would be the worst place to lose it", len(vers))
	}
}

func TestImportingAMissingFileFailsClearly(t *testing.T) {
	v := openCLIVault(t)
	if code := importDotenv(v, filepath.Join(t.TempDir(), "nope.env")); code == 0 {
		t.Error("importing a file that does not exist reported success")
	}
}
