package mcp

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The README's tool list is a promise to anyone deciding whether to mount this
// server, so it is treated as a contract and checked against the code.
//
// It has been wrong before, and not visibly: the README advertised
// `consolidate_memory` and `set_fact` — with a paragraph explaining what
// consolidate_memory does for an agent — while neither was ever exposed over
// MCP. Both existed as HTTP endpoints, so nothing failed; agents simply never
// got the tools the documentation said they had. The tools/list test could not
// catch that because it compares the server against a list maintained beside
// it, which drifts in exactly the same direction.

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		// go/ has its own README, so the root is identified by a file that
		// only the repository root carries.
		_, a := os.Stat(filepath.Join(dir, "README.md"))
		_, b := os.Stat(filepath.Join(dir, "SECURITY.md"))
		if a == nil && b == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("README.md not found above the package; skipping docs-parity check")
	return ""
}

var toolNameRE = regexp.MustCompile("`([a-z_]{3,})`")

// readmeToolList returns the tool names from the README paragraph that
// enumerates what an agent gets.
func readmeToolList(t *testing.T, readme string) map[string]bool {
	t.Helper()
	// An explicit delimiter rather than a prose anchor: the first version of
	// this test keyed off a sentence, and reformatting the section silently
	// broke the check rather than the docs.
	const begin, end = "<!-- tools:begin", "<!-- tools:end -->"
	i := strings.Index(readme, begin)
	j := strings.Index(readme, end)
	if i < 0 || j < i {
		t.Fatalf("README is missing the %s ... %s block that lists the agent's "+
			"tools; restore it rather than deleting this test", begin, end)
	}
	para := readme[i:j]
	out := map[string]bool{}
	for _, m := range toolNameRE.FindAllStringSubmatch(para, -1) {
		out[m[1]] = true
	}
	if len(out) < 5 {
		t.Fatalf("only %d tool names parsed out of the README paragraph; the "+
			"format probably changed", len(out))
	}
	return out
}

func TestREADMEToolListMatchesTheServer(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join(repoRoot(t), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)
	claimed := readmeToolList(t, text)

	implemented := map[string]bool{}
	for _, tl := range Tools() {
		implemented[tl.Name] = true
	}

	var missing, undocumented []string
	for name := range claimed {
		if !implemented[name] {
			missing = append(missing, name)
		}
	}
	// A tool need not appear in that one paragraph, but it must be documented
	// somewhere — an undocumented tool is one nobody knows to ask for.
	for name := range implemented {
		if !strings.Contains(text, "`"+name+"`") {
			undocumented = append(undocumented, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(undocumented)

	if len(missing) > 0 {
		t.Errorf("README promises tools the server does not expose: %v\n"+
			"Either implement them or stop advertising them — a documented tool "+
			"that does not exist is worse than an absent one, because it gets "+
			"planned around.", missing)
	}
	if len(undocumented) > 0 {
		t.Errorf("server exposes tools the README never mentions: %v\n"+
			"Agents and humans both pick tools from the docs.", undocumented)
	}
}

// Environment variables are the other surface where documentation has silently
// outrun the code: GRIMOIRE_AUTH_TOKEN, GRIMOIRE_HOST, GRIMOIRE_FRAME_OPTIONS,
// GRIMOIRE_BROKER_ALLOW_PRIVATE and GRIMOIRE_MCP_TRANSPORT were all documented
// while doing nothing at all. Two of them were security controls.
func TestREADMEEnvVarsAreImplemented(t *testing.T) {
	root := repoRoot(t)
	// Both files, because the check has to follow the content. The config table
	// moved to docs/CONFIG.md when the README was cut down, and a test that
	// still read only README.md would have gone on passing while covering
	// almost nothing — the silent kind of regression this test exists to catch.
	envRE := regexp.MustCompile(`GRIMOIRE_[A-Z_]+`)
	documented := map[string]bool{}
	for _, rel := range []string{"README.md", "docs/CONFIG.md"} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		for _, m := range envRE.FindAllString(string(b), -1) {
			documented[m] = true
		}
	}
	if len(documented) < 20 {
		t.Fatalf("only %d env vars found across the docs; the config table has "+
			"moved again and this check is no longer looking at it", len(documented))
	}
	// GRIMOIRE_LLM_BASE_URL / _API_KEY appear in the docs as a "_BASE_URL /
	// _API_KEY" pair, which the regex reads as bare suffixes; drop those.
	delete(documented, "GRIMOIRE_")

	var code strings.Builder
	err := filepath.Walk(filepath.Join(root, "go"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		code.Write(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	src := code.String()

	var unimplemented []string
	for name := range documented {
		if !strings.Contains(src, name) {
			unimplemented = append(unimplemented, name)
		}
	}
	sort.Strings(unimplemented)
	if len(unimplemented) > 0 {
		t.Errorf("README documents settings no code reads: %v\n"+
			"Setting one of these does nothing, which is how a deployment ends up "+
			"believing it has a control it does not have.", unimplemented)
	}
}

// The README's MCP config block is the snippet people paste into their agent
// client, so every env key in it must be one this server actually reads.
//
// The existing settings check could not catch the failure this was written for.
// It asks whether the documented name appears anywhere under go/ — and
// GRIMOIRE_API did appear, in the `agent-setup` command that printed it. A name
// written twice and read nowhere passes a substring test and still does
// nothing. This one compares against the names the MCP server reads.
func TestReadmeMCPConfigUsesEnvTheServerReads(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}

	// The env keys grimoire-mcp resolves: the two constants it is launched
	// with, plus the ones read directly for transport and auth.
	reads := map[string]bool{
		EnvURL: true, EnvAgentName: true,
		"GRIMOIRE_PORT": true, "GRIMOIRE_AUTH_TOKEN": true,
		"GRIMOIRE_MCP_TRANSPORT": true, "GRIMOIRE_MCP_ADDR": true,
		"GRIMOIRE_MCP_PORT": true,
	}

	blocks := regexp.MustCompile("(?s)```jsonc?\n(.*?)```").FindAllStringSubmatch(string(b), -1)
	envKey := regexp.MustCompile(`"(GRIMOIRE_[A-Z_]+)"\s*:`)
	checked := 0
	for _, blk := range blocks {
		if !strings.Contains(blk[1], "mcpServers") {
			continue
		}
		checked++
		for _, m := range envKey.FindAllStringSubmatch(blk[1], -1) {
			if !reads[m[1]] {
				t.Errorf("README MCP config sets %s, which grimoire-mcp never reads.\n"+
					"Setting it is a no-op: the server falls back to its default address, "+
					"which fails only for people not already on it.", m[1])
			}
		}
	}
	if checked == 0 {
		t.Fatal("no mcpServers config block found in the README — the anchor moved")
	}
}
