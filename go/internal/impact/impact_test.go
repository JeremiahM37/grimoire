package impact

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

var day0 = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

func mk(agent string, day int, errors int, say ...string) transcript.Session {
	s := transcript.Session{Agent: agent, ID: agent + string(rune('A'+day)), Start: day0.AddDate(0, 0, day), End: day0.AddDate(0, 0, day).Add(30 * time.Minute)}
	for _, t := range say {
		s.Turns = append(s.Turns, transcript.Turn{Role: transcript.RoleUser, Text: t})
	}
	for i := 0; i < errors; i++ {
		s.Turns = append(s.Turns, transcript.Turn{Role: transcript.RoleTool, Tool: "Bash", ToolTarget: "kestrel deploy", ToolError: true})
	}
	s.Turns = append(s.Turns, transcript.Turn{Role: transcript.RoleTool, Tool: "Bash", ToolTarget: "ls"})
	return s
}

func TestCorrectionsAndRetellsAreCounted(t *testing.T) {
	s := mk("a", 1, 0, "deploy kestrel to staging please now", "no, that is wrong", "deploy kestrel to staging please now again", "I told you to use staging", "thanks")
	f := Stats([]transcript.Session{s})[0]
	if f.Prompts != 5 || f.Corrections != 2 || f.Retells != 1 {
		t.Errorf("%+v", f)
	}
	if f.Minutes != 30 {
		t.Errorf("minutes %v", f.Minutes)
	}
}

func TestBeforeAfterNeedsEnoughSessionsAndSaysCorrelation(t *testing.T) {
	var ss []transcript.Session
	for d := 0; d < 12; d++ { // twelve sessions before: 3 errors each; twelve after: 0
		ss = append(ss, mk("claude-code", d, 3, "deploy kestrel", "no, wrong"))
	}
	for d := 12; d < 24; d++ {
		ss = append(ss, mk("claude-code", d, 0, "deploy kestrel"))
	}
	landed := day0.AddDate(0, 0, 12).Add(-time.Hour)
	rep := Compute([]Item{
		{Kind: "memory", Name: "Deploy kestrel", At: landed, DateSource: "frontmatter", Terms: []string{"deploy kestrel staging"}},
		{Kind: "skill", Name: "late", Agent: "claude-code", At: day0.AddDate(0, 0, 23), DateSource: "exported"},
		{Kind: "skill", Name: "other-agent", Agent: "codex", At: landed, DateSource: "exported"},
	}, Stats(ss), Options{})
	if !strings.Contains(rep.Caveat, "Correlation only") || len(rep.Rows) != 3 {
		t.Fatalf("%+v", rep)
	}
	byName := map[string]Row{}
	for _, r := range rep.Rows {
		byName[r.Name] = r
	}
	m := byName["Deploy kestrel"]
	if m.All.Before.Sessions != 12 || m.All.After.Sessions != 12 || m.All.Before.MedianErrors != 3 || m.All.After.MedianErrors != 0 {
		t.Errorf("%+v", m.All)
	}
	if !strings.HasPrefix(m.All.Verdict, "lower after") || !strings.Contains(m.All.Verdict, "correlation only") {
		t.Errorf("verdict %q", m.All.Verdict)
	}
	if m.Topical == nil || m.Topical.Before.Sessions != 12 {
		t.Errorf("topical %+v", m.Topical)
	}
	if v := byName["late"].All.Verdict; !strings.Contains(v, "not enough sessions") {
		t.Errorf("late skill: %q", v)
	}
	if o := byName["other-agent"]; o.All.Before.Sessions != 0 || !strings.Contains(o.All.Verdict, "not enough") {
		t.Errorf("a skill for an agent with no sessions: %+v", o.All)
	}
}

func TestNoClearDifferenceWhenNothingMoved(t *testing.T) {
	var ss []transcript.Session
	for d := 0; d < 20; d++ {
		ss = append(ss, mk("a", d, d%2, "do the thing"))
	}
	rep := Compute([]Item{{Kind: "memory", Name: "x", At: day0.AddDate(0, 0, 10).Add(-time.Hour)}}, Stats(ss), Options{})
	if v := rep.Rows[0].All.Verdict; v != "no clear difference" {
		t.Errorf("verdict %q", v)
	}
}

func TestCollectReadsProfilesMemoriesAndSkills(t *testing.T) {
	home := t.TempDir()
	// A user profile for a generic JSONL agent, with a skills dir.
	cfg := filepath.Join(home, ".config", "grimoire", "agents")
	os.MkdirAll(cfg, 0o755)
	profile := `{"hooks":{"file":""},"transcripts":{"glob":"~/logs/*.jsonl","format":"generic-jsonl","map":{"session":"s","time":"t","role":"r","text":"x","tool":"tool","tool_target":"cmd","tool_error":"err"}},"skills_dir":"~/skills"}`
	os.WriteFile(filepath.Join(cfg, "myagent.json"), []byte(profile), 0o644)
	logs := filepath.Join(home, "logs")
	os.MkdirAll(logs, 0o755)
	var b strings.Builder
	base := time.Now().AddDate(0, 0, -30)
	for d := 0; d < 20; d++ {
		ts := base.AddDate(0, 0, d).Unix()
		sid := string(rune('a' + d))
		errv := 0
		if d < 10 {
			errv = 1
		}
		b.WriteString(`{"s":"` + sid + `","t":` + itoa(ts) + `,"r":"user","x":"deploy kestrel"}` + "\n")
		b.WriteString(`{"s":"` + sid + `","t":` + itoa(ts+60) + `,"tool":"bash","cmd":"kestrel deploy","err":` + itoa(int64(errv)) + `}` + "\n")
	}
	os.WriteFile(filepath.Join(logs, "a.jsonl"), []byte(b.String()), 0o644)
	// A memory dated in the middle.
	store := filepath.Join(home, "store")
	os.MkdirAll(store, 0o755)
	mid := base.AddDate(0, 0, 10).Format("2006-01-02")
	os.WriteFile(filepath.Join(store, "reference_kestrel.md"), []byte("---\nname: Kestrel deploy\ndescription: deploy kestrel\nmetadata:\n  created: "+mid+"\n---\n1. a\n2. b\n3. c\n"), 0o644)

	rep, errs := Collect(CollectOptions{Home: home, Store: store, Since: 60 * 24 * time.Hour, Now: time.Now(), Min: 5})
	for _, e := range errs {
		t.Logf("err: %v", e)
	}
	if rep.Sessions != 20 {
		t.Fatalf("sessions %d: %+v", rep.Sessions, rep)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].DateSource != "frontmatter" || rep.Rows[0].All.Before.Sessions != 10 || rep.Rows[0].All.After.Sessions != 10 {
		t.Fatalf("%+v", rep.Rows)
	}
	if !strings.Contains(Text(rep), "Correlation only") {
		t.Error("text report must carry the caveat")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
