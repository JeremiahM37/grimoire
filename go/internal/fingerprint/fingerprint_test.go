package fingerprint

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

const vectorsPath = "../../../clients/hooks/tests/fingerprint_vectors.json"

type vector struct {
	Text       string   `json:"text"`
	Candidates []string `json:"candidates"`
}

var vectorInputs = []string{
	"Run ~/tailscale-helpers/snapshot.sh first, then POST /tailnet/-/keys with expirySeconds:600.",
	"curl https://user:pw@Docs.Example.com:8443/a/b/Config.yaml?x=1 --max-time=30 -s",
	"export GRIMOIRE_URL=http://127.0.0.1:9111 and read $GRIMOIRE_AUTH_TOKEN, not ${HOME}",
	"ssh root@100.104.116.16 \"pct exec 200 -- bash -c 'docker compose up -d'\"",
	"Pin ollama v0.20.5, not 0.34.0; qwen3.6:35b-a3b stays; `OLLAMA_KEEP_ALIVE=5m` (see /etc/default/ollama).",
	"Naïve CAFÉ ünïcode: K (Kelvin) and ß are separators; tag:claude host:port a.b",
	"..trailing.. ::lead -- --flag-name -x ab /a /ab /abc",
	"",
}

func TestVectorsMatchBothImplementations(t *testing.T) {
	got := make([]vector, len(vectorInputs))
	for i, in := range vectorInputs {
		c := Candidates(in)
		if c == nil {
			c = []string{}
		}
		got[i] = vector{in, c}
	}
	if os.Getenv("UPDATE_FINGERPRINT_VECTORS") == "1" {
		raw, _ := json.MarshalIndent(got, "", " ")
		if err := os.WriteFile(vectorsPath, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var want []vector
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates changed; this is the wire spec shared with grimoire_outcome.py. If intended, bump SpecVersion and rerun with UPDATE_FINGERPRINT_VECTORS=1\n got: %v\nwant: %v", got, want)
	}
}

func has(c []string, s string) bool {
	for _, x := range c {
		if x == s {
			return true
		}
	}
	return false
}

func TestCandidatesExpandPathsURLsAndAssignments(t *testing.T) {
	c := Candidates("run ~/tailscale-helpers/snapshot.sh and --model=llama3 at https://u:p@Host.example.com:8443/x/Y.json")
	for _, want := range []string{"~/tailscale-helpers/snapshot.sh", "tailscale-helpers", "snapshot.sh", "--model=llama3",
		"--model", "llama3", "host.example.com:8443", "host.example.com", "y.json"} {
		if !has(c, want) {
			t.Errorf("missing %q in %v", want, c)
		}
	}
	if has(c, "u:p@host.example.com:8443") {
		t.Error("userinfo must not survive in the host")
	}
}

// store builds a memory store of n unrelated memories plus the ones given.
func store(n int, extra ...string) []string {
	out := append([]string{}, extra...)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("Unrelated memory number %d about topic%d: remember to keep notes tidy and short.", i, i))
	}
	return out
}

func tokens(f []Fingerprint) []string {
	var out []string
	for _, x := range f {
		out = append(out, x.Token)
	}
	return out
}

func TestSelectPicksRareDistinctiveTokensAndSkipsTheSituation(t *testing.T) {
	mem := "Snapshot first with `~/tailscale-helpers/snapshot.sh <label>`, then POST /tailnet/-/keys with expirySeconds. " +
		"Never run docker compose down on the kestrel host 100.96.103.31:9111, set KESTREL_API_TOKEN."
	corpus := store(200, mem, "Docker compose is how services start on this box.")
	df := BuildDF(corpus)
	got := Select(mem, df, nil, Options{})
	if len(got) == 0 || len(got) > MaxPerMemory {
		t.Fatalf("got %v", tokens(got))
	}
	for _, want := range []string{"snapshot.sh", "kestrel_api_token", "expiryseconds"} {
		if !has(tokens(got), want) {
			t.Errorf("want %q among %v", want, tokens(got))
		}
	}
	for _, bad := range []string{"docker", "compose", "never", "first", "label"} {
		if has(tokens(got), bad) {
			t.Errorf("%q is not distinctive: %v", bad, tokens(got))
		}
	}
	// What the prompt already contained proves nothing when the answer repeats it.
	sit := Set("please run snapshot.sh before touching KESTREL_API_TOKEN")
	for _, f := range Select(mem, df, sit, Options{}) {
		if f.Token == "snapshot.sh" || f.Token == "kestrel_api_token" {
			t.Errorf("%q is in the situation", f.Token)
		}
	}
}

func TestSelectIsHonestAboutUnfingerprintableMemories(t *testing.T) {
	mem := "Prefer small focused commits and tell the user what changed in plain words."
	df := BuildDF(store(200, mem))
	if got := Select(mem, df, nil, Options{}); got != nil {
		t.Fatalf("guessing: %v", got)
	}
	// A token that is in many memories is not evidence, however it is spelled.
	common := "Use `git rebase` and check ~/projects/common-repo/notes.md."
	many := store(100)
	for i := 0; i < 40; i++ {
		many = append(many, common)
	}
	if got := Select(common, BuildDF(many), nil, Options{}); got != nil {
		t.Fatalf("common tokens became fingerprints: %v", tokens(got))
	}
}

func TestSelectLimitsAndRunQuota(t *testing.T) {
	mem := "Files: /srv/aa1/bb2/cc3/dd4/ee5 /srv/ff6/gg7/hh8 --alpha-flag --beta-flag --gamma-flag --delta-flag VERSION_A VERSION_B"
	got := Select(mem, BuildDF(store(300, mem)), nil, Options{})
	if len(got) > MaxPerMemory {
		t.Fatalf("%d fingerprints", len(got))
	}
	perRun := map[string]int{}
	for _, f := range got {
		perRun[f.Kind]++
	}
	if perRun["path"] > 4 { // two runs x PerRun 2
		t.Errorf("one run filled the quota: %v", tokens(got))
	}
}

func TestHashIsSaltedAndTruncated(t *testing.T) {
	a, b := Hash("salt-a", "snapshot.sh"), Hash("salt-b", "snapshot.sh")
	if len(a) != HashHex || a == b || strings.Contains(a, "snapshot") || a != Hash("salt-a", "snapshot.sh") {
		t.Fatalf("%s %s", a, b)
	}
}
