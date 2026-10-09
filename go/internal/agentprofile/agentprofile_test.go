package agentprofile

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedContextHookMatchesTheClientCopy(t *testing.T) {
	want, err := os.ReadFile("../../../clients/hooks/" + ContextFileName)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ContextScript, want) {
		t.Fatal("go/internal/agentprofile/" + ContextFileName + " differs from clients/hooks/" + ContextFileName +
			"; copy the client file over it")
	}
}

func home(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	return h
}

func writeUser(t *testing.T, h, name, body string) {
	t.Helper()
	dir := UserDir(h)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuiltinsAreValid(t *testing.T) {
	h := home(t)
	for _, n := range []string{"claude-code", "codex", "generic"} {
		p, err := Load(n, h)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if p.Source != "builtin" {
			t.Errorf("%s source = %q", n, p.Source)
		}
	}
	p, _ := Load("codex", h)
	if p.Hooks.File != filepath.Join(h, ".codex", "hooks.json") || p.Hooks.Trust != "codex" || p.Dir() != filepath.Join(h, ".codex") {
		t.Errorf("codex paths not expanded: %+v", p.Hooks)
	}
	if p.Hooks.Events["pre_action"] != "PreToolUse" || p.Actions["apply_patch"] == "" {
		t.Errorf("codex events/actions: %+v %+v", p.Hooks.Events, p.Actions)
	}
}

func TestANewAgentIsOneFileOverGeneric(t *testing.T) {
	h := home(t)
	writeUser(t, h, "zed", `{"hooks":{"file":"~/.zed/hooks.json","events":{"prompt":"user_message"}},
		"event_fields":{"prompt":"message.text"},"output":"plain-stdout"}`)
	p, err := Load("zed", h)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "zed" || p.Source != "user" || p.Hooks.File != filepath.Join(h, ".zed", "hooks.json") {
		t.Errorf("%+v", p)
	}
	// A field named replaces generic's whole, so only the listed events exist.
	if len(p.Hooks.Events) != 1 || p.Hooks.Events["prompt"] != "user_message" || p.Output != "plain-stdout" || !p.SessionFallback {
		t.Errorf("events = %v", p.Hooks.Events)
	}
	all, errs := All(h)
	if len(errs) != 0 || len(all) != 4 {
		t.Errorf("All = %d profiles, errs %v", len(all), errs)
	}
}

func TestAUserFileOverridesABuiltin(t *testing.T) {
	h := home(t)
	writeUser(t, h, "codex", `{"hooks":{"matchers":{"post_action":"Bash"}}}`)
	p, err := Load("codex", h)
	if err != nil {
		t.Fatal(err)
	}
	if p.Hooks.Matchers["post_action"] != "Bash" || len(p.Hooks.Matchers) != 1 || p.Hooks.Events["stop"] == "" || p.Hooks.Trust != "codex" {
		t.Errorf("an override changes only the fields it names: %+v", p.Hooks)
	}
}

func TestBadProfilesAreRefusedWithAReason(t *testing.T) {
	h := home(t)
	cases := map[string]string{
		"typo":     `{"hoks":{}}`,
		"badevent": `{"hooks":{"file":"~/x.json","events":{"on_save":"Save"}}}`,
		"output":   `{"output":"xml"}`,
		"relative": `{"hooks":{"file":"hooks.json"}}`,
		"quote":    `{"hooks":{"file":"~/x.json","events":{"prompt":"P'; rm -rf"}}}`,
		"mcp":      `{"mcp":{"file":"~/m.json"}}`,
	}
	for name, body := range cases {
		writeUser(t, h, name, body)
		if _, err := Load(name, h); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := Load("../etc", h); err == nil {
		t.Error("a path in a name must be refused")
	}
	if _, err := Load("nope", h); err == nil || !strings.Contains(err.Error(), "agent profiles") {
		t.Errorf("unknown agent: %v", err)
	}
	_, errs := All(h)
	if len(errs) != len(cases) {
		t.Errorf("All should report each bad file and carry on: %v", errs)
	}
}

func TestResolveIsIdempotentAndDryRunWritesNothing(t *testing.T) {
	h := home(t)
	p, _ := Load("codex", h)
	dir := ResolvedDir(h)
	if _, changed, err := p.Resolve(dir, true); err != nil || !changed {
		t.Fatalf("dry run: %v %v", changed, err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("dry run wrote")
	}
	if _, changed, err := p.Resolve(dir, false); err != nil || !changed {
		t.Fatalf("first: %v %v", changed, err)
	}
	if _, changed, _ := p.Resolve(dir, false); changed {
		t.Error("second resolve changed the file")
	}
}

func TestStarterLoadsAsAProfile(t *testing.T) {
	h := home(t)
	text, err := Starter("nova")
	if err != nil {
		t.Fatal(err)
	}
	writeUser(t, h, "nova", string(text))
	if _, err := Load("nova", h); err != nil {
		t.Fatalf("starter does not validate: %v", err)
	}
	if _, err := Starter("Bad Name"); err == nil {
		t.Error("bad name")
	}
}
