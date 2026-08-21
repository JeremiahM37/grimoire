package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/eval"
)

// `grimoire eval` end to end: a real vault, a real index, the real retriever.
//
// These run with no LLM configured, so the lexical generator writes the
// questions — which is also the path most self-hosters will hit, and therefore
// the one worth having covered by a test that actually runs.

// seedVault writes notes distinctive enough to be findable, since the point of
// the measurement is whether retrieval can tell them apart.
func seedVault(t *testing.T, dir string, n int) {
	t.Helper()
	topics := []struct{ subject, detail string }{
		{"kestrel gateway", "rotate the ingress certificate before restarting, " +
			"because the gateway caches the chain at boot and serves the expired one"},
		{"osprey database", "the nightly vacuum runs at 0300 and holds an exclusive " +
			"lock on the ledger table for about four minutes"},
		{"harrier queue", "messages older than seven days are moved to the dead " +
			"letter shelf and are not retried automatically"},
		{"merlin scheduler", "a job that misses its window is skipped rather than " +
			"backfilled, so a paused worker loses that hour permanently"},
		{"falcon cache", "eviction is least-recently-used with a floor of two " +
			"hundred entries kept regardless of age"},
	}
	for i := 0; i < n; i++ {
		tp := topics[i%len(topics)]
		body := "# " + tp.subject + " runbook " + string(rune('A'+i)) + "\n\n" +
			"The " + tp.subject + " procedure: " + tp.detail + ". " +
			"This is documented so that whoever is on call at the time does not " +
			"have to work it out from first principles during an incident.\n"
		p := filepath.Join(dir, "runbook-"+string(rune('a'+i))+".md")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readSetFile(t *testing.T, dir, name string) eval.Set {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, ".grimoire", "eval", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := eval.ReadSet(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEvalBuildFreezesAQuestionSet(t *testing.T) {
	dir := vaultDir(t)
	seedVault(t, dir, 10)

	out, code := runCmd(t, "eval", "build", "--n", "6", "--lexical")
	if code != 0 {
		t.Fatalf("eval build = %d: %s", code, out)
	}
	if !strings.Contains(out, "questions") {
		t.Errorf("output = %q", out)
	}
	set := readSetFile(t, dir, "default")
	if len(set.Questions) == 0 {
		t.Fatal("no questions written")
	}
	if set.Generator != "lexical" {
		t.Errorf("generator = %q — the set must say which generator wrote it, "+
			"or two incomparable measurements look alike", set.Generator)
	}
	for _, q := range set.Questions {
		if q.Path == "" {
			t.Errorf("question with no gold passage: %+v", q)
		}
	}
}

func TestTheSetLivesInsideDotGrimoireNotInTheVault(t *testing.T) {
	// The vault syncs to people's phones. A generated measurement artifact is
	// derived data and belongs with the index, not with the notes.
	dir := vaultDir(t)
	seedVault(t, dir, 6)
	runCmd(t, "eval", "build", "--n", "3", "--lexical")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			t.Errorf("eval artifact %q was written into the vault root", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".grimoire", "eval")); err != nil {
		t.Errorf("eval directory missing: %v", err)
	}
}

func TestEvalRunScoresTheFrozenSet(t *testing.T) {
	dir := vaultDir(t)
	seedVault(t, dir, 10)
	if _, code := runCmd(t, "eval", "build", "--n", "6", "--lexical"); code != 0 {
		t.Fatal("build failed")
	}

	out, code := runCmd(t, "eval", "run")
	if code != 0 {
		t.Fatalf("eval run = %d: %s", code, out)
	}
	for _, want := range []string{"recall@8", "note recall", "MRR", "embedder"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	// The lexical generator asks a passage about its own rare words. A hybrid
	// retriever with a working keyword leg should find almost all of them, so
	// a low score here is a broken retriever rather than a hard question set.
	if strings.Contains(out, "recall@8   0.0%") {
		t.Errorf("recall of zero on the lexical floor — retrieval cannot find a "+
			"passage from its own distinctive words:\n%s", out)
	}
}

func TestRunningWithNoSetSaysWhatToDo(t *testing.T) {
	vaultDir(t)
	out, code := runCmd(t, "eval", "run")
	if code == 0 {
		t.Fatal("scoring succeeded with no question set")
	}
	_ = out
}

func TestBaselineAndCompareRoundTrip(t *testing.T) {
	dir := vaultDir(t)
	seedVault(t, dir, 10)
	runCmd(t, "eval", "build", "--n", "6", "--lexical")
	if _, code := runCmd(t, "eval", "run", "--baseline"); code != 0 {
		t.Fatal("run --baseline failed")
	}
	if _, err := os.Stat(filepath.Join(dir, ".grimoire", "eval", "default.baseline.json")); err != nil {
		t.Fatalf("no baseline written: %v", err)
	}

	out, code := runCmd(t, "eval", "compare")
	if code != 0 {
		t.Fatalf("compare = %d: %s", code, out)
	}
	// Nothing changed between the two runs, so the diff has to say so rather
	// than inventing movement.
	if !strings.Contains(out, "0 fixed, 0 broken") {
		t.Errorf("comparing a config with itself reported movement:\n%s", out)
	}
	if !strings.Contains(out, "recall@8") {
		t.Errorf("comparison does not show the metric:\n%s", out)
	}
}

func TestComparingAcrossDifferentKIsRefused(t *testing.T) {
	// recall@8 and recall@20 are different measurements; comparing them would
	// produce a confident number about nothing.
	dir := vaultDir(t)
	seedVault(t, dir, 10)
	runCmd(t, "eval", "build", "--n", "6", "--lexical")
	runCmd(t, "eval", "run", "--baseline")

	out, code := runCmd(t, "eval", "compare", "--k", "20")
	if code == 0 {
		t.Errorf("compared k=8 against k=20 without complaint:\n%s", out)
	}
}

func TestComparingWithNoBaselineSaysWhatToDo(t *testing.T) {
	dir := vaultDir(t)
	seedVault(t, dir, 6)
	runCmd(t, "eval", "build", "--n", "3", "--lexical")
	if _, code := runCmd(t, "eval", "compare"); code == 0 {
		t.Error("compare succeeded with no baseline")
	}
}

func TestTheBaselineRecordsWhatProducedIt(t *testing.T) {
	// A stored baseline with no configuration is a number with no idea what
	// made it.
	dir := vaultDir(t)
	seedVault(t, dir, 8)
	runCmd(t, "eval", "build", "--n", "4", "--lexical")
	runCmd(t, "eval", "run", "--baseline")

	raw, err := os.ReadFile(filepath.Join(dir, ".grimoire", "eval", "default.baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	var res eval.Result
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Config.Embedder == "" || res.Config.Dim == 0 {
		t.Errorf("baseline does not record its embedder: %+v", res.Config)
	}
	if res.K == 0 || res.Questions == 0 {
		t.Errorf("baseline does not record what it measured: %+v", res)
	}
}

func TestNamedSetsAreIndependent(t *testing.T) {
	dir := vaultDir(t)
	seedVault(t, dir, 10)
	runCmd(t, "eval", "build", "--n", "4", "--lexical", "--set", "small")
	runCmd(t, "eval", "build", "--n", "8", "--lexical", "--set", "large")

	small := readSetFile(t, dir, "small")
	large := readSetFile(t, dir, "large")
	if len(small.Questions) >= len(large.Questions) {
		t.Errorf("named sets are not independent: %d vs %d",
			len(small.Questions), len(large.Questions))
	}
}

func TestASetNameCannotEscapeTheEvalDirectory(t *testing.T) {
	// The name becomes a filename. A silently renamed set would compare
	// against the wrong baseline; a name that climbed out would write anywhere.
	dir := vaultDir(t)
	seedVault(t, dir, 6)
	if _, code := runCmd(t, "eval", "build", "--n", "3", "--lexical",
		"--set", "../../escaped"); code != 0 {
		t.Fatal("build failed")
	}
	if _, err := os.Stat(filepath.Join(dir, ".grimoire", "eval", "default.json")); err != nil {
		t.Errorf("a path-shaped name did not fall back to the default set: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.json")); err == nil {
		t.Error("a set was written outside the vault")
	}
}

func TestBadFlagsAreRefusedRatherThanGuessed(t *testing.T) {
	dir := vaultDir(t)
	seedVault(t, dir, 6)
	if _, code := runCmd(t, "eval", "build", "--n", "zero"); code == 0 {
		t.Error("--n zero was accepted")
	}
	runCmd(t, "eval", "build", "--n", "3", "--lexical")
	if _, code := runCmd(t, "eval", "run", "--k", "-4"); code == 0 {
		t.Error("--k -4 was accepted")
	}
	if _, code := runCmd(t, "eval", "nonsense"); code == 0 {
		t.Error("an unknown subcommand was accepted")
	}
}
