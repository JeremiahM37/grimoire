package skillgate

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/skillmine"
	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

// fixture builds one session. A command prefixed with "!" ran and failed.
func fixture(id, user string, cmds ...string) transcript.Session {
	s := transcript.Session{Agent: "claude-code", ID: id, Cwd: "/work/kestrel",
		Start: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	if user != "" {
		s.Turns = append(s.Turns, transcript.Turn{Role: transcript.RoleUser, Text: user})
	}
	for _, c := range cmds {
		t := transcript.Turn{Role: transcript.RoleTool, Tool: "Bash", ToolTarget: c}
		if strings.HasPrefix(c, "!") {
			t.ToolTarget, t.ToolError = c[1:], true
		}
		s.Turns = append(s.Turns, t)
	}
	return s
}

var pattern = []string{"go test", "git push"}

// sources are sessions where the procedure ran, successfully.
func sources(n int) []transcript.Session {
	var out []transcript.Session
	for i := 0; i < n; i++ {
		out = append(out, fixture(fmt.Sprintf("src-%d", i), "please release the kestrel build",
			"go test ./...", "git push origin main"))
	}
	return out
}

// unrelated sessions do not contain the pattern and are not about the topic.
func unrelated(n int) []transcript.Session {
	var out []transcript.Session
	for i := 0; i < n; i++ {
		out = append(out, fixture(fmt.Sprintf("other-%d", i), "check the sonarr queue", "git status", "ls"))
	}
	return out
}

func goodSpec() Spec {
	return Spec{Trigger: "release kestrel", Preconditions: []string{"command:go", "command:git"},
		Postconditions: []string{"ran:go test", "ran:git push"}}
}

func TestSkillMinedFromOneSessionFailsSupport(t *testing.T) {
	sess := append(sources(1), unrelated(6)...)
	g := Check(goodSpec(), pattern, sess, Options{})
	if g.Pass || g.Support != 1 {
		t.Fatalf("one source session should fail support: %+v", g)
	}
	if !hasReason(g, "support") {
		t.Errorf("reasons %v", g.Reasons)
	}
}

func TestGoodSkillPasses(t *testing.T) {
	sess := append(sources(4), unrelated(6)...)
	g := Check(goodSpec(), pattern, sess, Options{})
	if !g.Pass || len(g.Reasons) != 0 {
		t.Fatalf("a good skill should pass: %+v", g)
	}
	if g.Support != 4 || g.HeldOut != 6 || g.HeldOutFires != 0 || g.FalseFireRate != 0 {
		t.Errorf("numbers: support %d held-out %d fires %d", g.Support, g.HeldOut, g.HeldOutFires)
	}
	if len(g.Sources) != 4 || !strings.HasPrefix(g.Sources[0], "claude-code/") {
		t.Errorf("provenance %v", g.Sources)
	}
}

func TestLearnedFromFailuresIsFlagged(t *testing.T) {
	// The pattern is there in every source, but three of the four sessions
	// then ended with a failed shell call.
	var sess []transcript.Session
	for i := 0; i < 3; i++ {
		sess = append(sess, fixture(fmt.Sprintf("bad-%d", i), "please release the kestrel build",
			"go test ./...", "git push origin main", "!systemctl restart kestrel"))
	}
	sess = append(sess, fixture("good", "please release the kestrel build", "go test ./...", "git push origin main"))
	sess = append(sess, unrelated(6)...)
	g := Check(goodSpec(), pattern, sess, Options{})
	if g.Pass {
		t.Fatalf("failure-learned skill must not pass: %+v", g)
	}
	if g.FailedSources != 3 || g.KnownSources != 4 {
		t.Errorf("failed %d known %d", g.FailedSources, g.KnownSources)
	}
	if !hasReason(g, "failure contamination") {
		t.Errorf("reasons %v", g.Reasons)
	}
}

func TestOverBroadTriggerFailsFalseFire(t *testing.T) {
	// "test" alone fires on any session that mentions testing.
	spec := Spec{Trigger: "test"}
	var other []transcript.Session
	for i := 0; i < 3; i++ {
		other = append(other, fixture(fmt.Sprintf("t-%d", i), "run the test suite", "git status"))
	}
	other = append(other, unrelated(3)...)
	g := Check(spec, pattern, append(sources(4), other...), Options{})
	if g.Pass || g.HeldOutFires != 3 || g.HeldOut != 6 {
		t.Fatalf("over-broad trigger should fail false-fire: %+v", g)
	}
	if !hasReason(g, "over-broad") {
		t.Errorf("reasons %v", g.Reasons)
	}
}

func TestTooFewHeldOutSessionsIsNotAPass(t *testing.T) {
	g := Check(goodSpec(), pattern, append(sources(4), unrelated(2)...), Options{})
	if g.Pass || !hasReason(g, "not measured") {
		t.Fatalf("false fires cannot be measured on 2 held-out sessions: %+v", g)
	}
}

func TestUnmetPreconditionIsReported(t *testing.T) {
	ctx := Context{
		Cwd:     "/work/kestrel",
		Exists:  func(p string) bool { return p == "/work/kestrel/deploy.sh" },
		Command: func(n string) bool { return n == "git" },
		Env:     func(n string) bool { return n == "KESTREL_KEY" },
		Facts:   []string{"Kestrel deploys go to the staging host first"},
	}
	s := Spec{Preconditions: []string{"command:go", "command:git", "file:deploy.sh", "env:KESTREL_KEY", "env:MISSING", "fact:staging host", "fact:production", "bogus"}}
	ok, unmet := Applicable(s, ctx)
	if ok {
		t.Fatal("should not apply")
	}
	want := []string{"command:go", "env:MISSING", "fact:production", "bogus"}
	if strings.Join(unmet, " ") != strings.Join(want, " ") {
		t.Errorf("unmet %v, want %v", unmet, want)
	}
	if ok, unmet := Applicable(Spec{Trigger: "x"}, ctx); !ok || len(unmet) != 0 {
		t.Error("a spec with no preconditions always applies")
	}
}

func TestCheckPost(t *testing.T) {
	good := fixture("g", "", "go test ./...", "git push origin main")
	if ok, unmet := CheckPost(goodSpec(), good); !ok {
		t.Errorf("should hold: %v", unmet)
	}
	bad := fixture("b", "", "go test ./...", "!git push origin main")
	if ok, unmet := CheckPost(goodSpec(), bad); ok || len(unmet) != 1 || unmet[0] != "ran:git push" {
		t.Errorf("errored push should be unmet: %v", unmet)
	}
	none := fixture("n", "just chatting")
	if ok, _ := CheckPost(Spec{Postconditions: []string{"last-ok"}}, none); ok {
		t.Error("no shell outcome is not a success")
	}
}

func TestApplyRecordsGateOnCandidates(t *testing.T) {
	cs := []skillmine.Candidate{{ID: "abcd1234", Units: pattern, Examples: []string{"go test ./...", "git push origin main"}, Sessions: 4}}
	Apply(cs, append(sources(4), unrelated(6)...), Options{})
	if cs[0].Gate == nil || !cs[0].Gate.Pass {
		t.Fatalf("gate not recorded: %+v", cs[0].Gate)
	}
	if cs[0].Gate.Trigger != "go test git push" {
		t.Errorf("default trigger %q", cs[0].Gate.Trigger)
	}
}

func hasReason(g skillmine.Gate, sub string) bool {
	for _, r := range g.Reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}
