package memstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func note(name, typ, desc, body string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\nmetadata:\n  type: " + typ + "\n---\n\n" + body + "\n"
}

func TestKinds(t *testing.T) {
	cases := []struct {
		file, raw, want string
	}{
		{"a.md", note("a", "feedback", "d", "Never do x."), KindRule},
		{"b.md", note("b", "user", "d", "likes tabs"), KindPreference},
		{"c.md", note("c", "reference", "d", "see http://x"), KindReference},
		{"d.md", note("d", "project", "d", "state of things"), KindFact},
		{"e.md", note("e", "project", "d", "1. run a\n2. run b\n3. run c"), KindProcedure},
		{"f.md", "---\nname: f\ndescription: d\nmetadata:\n  kind: procedure\n  type: feedback\n---\nx", KindProcedure},
		{"feedback_g.md", "no frontmatter", KindRule},
		{"h.md", "plain", KindFact},
	}
	for _, c := range cases {
		if got := ParseNote(c.file, c.raw, time.Time{}).Kind; got != c.want {
			t.Errorf("%s: kind %q, want %q", c.file, got, c.want)
		}
	}
	if KindOfFact("", "Never push to main") != KindRule || KindOfFact("how-to", "x") != KindProcedure ||
		KindOfFact("", "How to deploy: run make") != KindProcedure || KindOfFact("gotcha", "x") != KindFact {
		t.Error("fact kinds")
	}
}

func TestCoreRulesFirstAndPointers(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		write(t, dir, "feedback_r"+string(rune('a'+i))+".md", note("r", "feedback", "rule number "+string(rune('a'+i)), "Because. Apply when."))
	}
	for i := 0; i < 30; i++ {
		topic := []string{"deploy pipeline", "tailscale network", "grimoire memory"}[i%3]
		write(t, dir, "project_"+string(rune('a'+i))+".md", note("p", "project", topic+" detail "+string(rune('a'+i)), "x"))
	}
	notes, _ := LoadDir(dir)
	c := BuildCore(notes, CoreOptions{Links: true})
	if c.Rules != 5 || c.RulesShown != 5 || c.Pointers == 0 {
		t.Fatalf("counts %+v", c)
	}
	if !strings.HasPrefix(c.Text, "<!-- grimoire:generated-index -->") {
		t.Error("marker missing")
	}
	if !strings.Contains(c.Text, "notes — recall '") {
		t.Errorf("no pointer line:\n%s", c.Text)
	}
	if c.Notes != 30 {
		t.Errorf("pointers cover %d notes, want 30", c.Notes)
	}
	// Tight budget: pointers survive, surplus rules collapse into one line.
	small := BuildCore(notes, CoreOptions{Budget: 520})
	if small.Bytes > 520 || small.Pointers == 0 || small.Pointers >= c.Pointers {
		t.Errorf("budget not honoured: %d bytes, %+v\n%s", small.Bytes, small, small.Text)
	}
	again := BuildCore(notes, CoreOptions{Links: true})
	if again.Text != c.Text {
		t.Error("not deterministic")
	}
}

func TestLinkDir(t *testing.T) {
	root := t.TempDir()
	canon := filepath.Join(root, "store")
	write(t, canon, "a.md", "A")
	agent := filepath.Join(root, "agent-mem")

	// absent -> symlink
	if r, err := Link(ShapeDir, agent, canon, LinkOptions{Agent: "x"}); err != nil || r.Action != "linked" {
		t.Fatal(r, err)
	}
	if r, _ := Link(ShapeDir, agent, canon, LinkOptions{}); r.Action != "already-linked" {
		t.Fatal(r)
	}
	if st := Status(ShapeDir, agent, canon, ""); st.State != "linked" || st.Notes != 1 {
		t.Fatalf("%+v", st)
	}
	if _, err := Unlink(ShapeDir, agent, canon, LinkOptions{}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(agent); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("still a link")
	}
	if b, _ := os.ReadFile(filepath.Join(agent, "a.md")); string(b) != "A" {
		t.Fatal("unlink lost the memory")
	}
	os.RemoveAll(agent)

	// identical content links without --merge; differing refuses
	write(t, agent, "a.md", "A")
	if r, err := Link(ShapeDir, agent, canon, LinkOptions{Agent: "x"}); err != nil || len(r.Deduped) != 1 || r.Backup == "" {
		t.Fatal(r, err)
	}
	os.Remove(agent)
	write(t, agent, "a.md", "A2")
	write(t, agent, "b.md", "B")
	if _, err := Link(ShapeDir, agent, canon, LinkOptions{Agent: "x"}); err == nil || !strings.Contains(err.Error(), "--merge") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(canon, "b.md")); b != nil {
		t.Fatal("refusal changed the store")
	}
	r, err := Link(ShapeDir, agent, canon, LinkOptions{Agent: "codex", Merge: true})
	if err != nil || r.Action != "merged" || len(r.Moved) != 1 || len(r.Renamed) != 1 {
		t.Fatal(r, err)
	}
	if b, _ := os.ReadFile(filepath.Join(canon, "a.from-codex.md")); string(b) != "A2" {
		t.Fatal("conflict not kept")
	}
	if b, _ := os.ReadFile(filepath.Join(canon, "a.md")); string(b) != "A" {
		t.Fatal("store file overwritten")
	}
	if _, err := os.Stat(r.Backup); err != nil {
		t.Fatal("no backup")
	}
}

func TestLinkFileBlock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "AGENTS.md")
	write(t, filepath.Dir(p), "AGENTS.md", "# mine\n\nkeep this\n")
	r, err := Link(ShapeFile, p, "", LinkOptions{Block: "- rule one\n"})
	if err != nil || r.Action != "block-written" || r.Backup == "" {
		t.Fatal(r, err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "keep this") || !strings.Contains(string(b), "- rule one") {
		t.Fatalf("%s", b)
	}
	if r, _ := Link(ShapeFile, p, "", LinkOptions{Block: "- rule one\n"}); r.Action != "block-unchanged" {
		t.Fatal(r)
	}
	Link(ShapeFile, p, "", LinkOptions{Block: "- rule two\n"})
	b, _ = os.ReadFile(p)
	if strings.Contains(string(b), "rule one") || strings.Count(string(b), "grimoire:memory:start") != 1 {
		t.Fatalf("%s", b)
	}
	if Status(ShapeFile, p, "", "- rule two\n").State != "block" || Status(ShapeFile, p, "", "- other\n").State != "block-stale" {
		t.Fatal("status")
	}
	if _, err := Unlink(ShapeFile, p, "", LinkOptions{}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if string(b) != "# mine\n\nkeep this\n" {
		t.Fatalf("unlink left %q", b)
	}
}

func TestProcedureSchedule(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	var p ProcState
	if !p.Due(now) {
		t.Fatal("new procedure is due")
	}
	p = p.Record(true, "", now)
	if p.EveryDays != 14 || p.Due(now.AddDate(0, 0, 13)) || !p.Due(now.AddDate(0, 0, 14)) {
		t.Fatalf("%+v", p)
	}
	for i := 0; i < 8; i++ {
		p = p.Record(true, "", now)
	}
	if p.EveryDays != MaxEveryDays {
		t.Fatalf("cap %d", p.EveryDays)
	}
	p = p.Record(false, "gone", now)
	if p.EveryDays != 14 || !p.Failed || !p.Due(now) {
		t.Fatalf("%+v", p)
	}
	in := InitialState(map[string]string{"metadata.verified_at": "2026-09-01", "metadata.verify_every": "30d"})
	if in.EveryDays != 30 || in.VerifiedAt.IsZero() {
		t.Fatal(in)
	}
}

func TestChecks(t *testing.T) {
	env := Env{
		Home:         "/home/u",
		Ports:        []int{9111},
		Exists:       func(p string) bool { return p == "/opt/x/run.sh" },
		SystemctlCat: func(u string) (bool, bool) { return u == "grimoire.service", true },
		Listening:    func(p int) bool { return false },
	}
	text := "Run /opt/x/run.sh then /opt/x/gone.sh, restart grimoire.service and ghost.service; serves on port 9111 and port 80. See /api/memory/core and /tmp/foo/bar."
	all, failed := RunChecks(text, Checkers(env))
	if len(all) != 5 || len(failed) != 3 {
		t.Fatalf("all=%v failed=%v", all, failed)
	}
	// No systemd: the unit check silently does not apply.
	env.SystemctlCat = func(string) (bool, bool) { return false, false }
	if _, f := RunChecks("restart ghost.service", Checkers(env)); len(f) != 0 {
		t.Fatal(f)
	}
}

func TestLint(t *testing.T) {
	has := func(ws []Warning, code string) bool {
		for _, w := range ws {
			if w.Code == code {
				return true
			}
		}
		return false
	}
	if w := Lint("rule", "Never force-push."); !has(w, "rule_missing_why") || !has(w, "rule_missing_how") {
		t.Errorf("%v", w)
	}
	if w := Lint("rule", "Never force-push because it broke prod once. Apply when pushing to main."); len(w) != 0 {
		t.Errorf("%v", w)
	}
	if w := Lint("fact", "- a\n- b\n- c\n- d"); !has(w, "multiple_facts") {
		t.Errorf("%v", w)
	}
	if w := Lint("fact", "token is ghp_"+strings.Repeat("a", 36)); !has(w, "looks_like_secret") {
		t.Errorf("%v", w)
	}
	if w := Lint("fact", "fixed in commit 3e99aab12"); !has(w, "repo_info") {
		t.Errorf("%v", w)
	}
}
