package skillmine

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memstore"
	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

func sess(agent, cwd string, day int, cmds ...string) transcript.Session {
	s := transcript.Session{Agent: agent, ID: agent + cwd + string(rune('a'+day)), Cwd: cwd,
		Start: time.Date(2026, 10, day, 9, 0, 0, 0, time.UTC), End: time.Date(2026, 10, day, 10, 0, 0, 0, time.UTC)}
	for _, c := range cmds {
		tool := "Bash"
		if strings.HasPrefix(c, "!") { // a failed call
			s.Turns = append(s.Turns, transcript.Turn{Role: transcript.RoleTool, Tool: tool, ToolTarget: c[1:], ToolError: true})
			continue
		}
		if strings.HasPrefix(c, "@") { // another tool
			s.Turns = append(s.Turns, transcript.Turn{Role: transcript.RoleTool, Tool: "Edit", ToolTarget: c[1:]})
			continue
		}
		s.Turns = append(s.Turns, transcript.Turn{Role: transcript.RoleTool, Tool: tool, ToolTarget: c})
	}
	return s
}

func TestNormalize(t *testing.T) {
	cases := map[string][]string{
		"cd /w && git status && go test ./... -race":     {"git status", "go test"},
		"FOO=1 sudo systemctl restart grimoire.service":  {"systemctl restart"},
		"ls -la; cat x | grep y":                         nil,
		"python3 scripts/build.py --fast && ./deploy.sh": {"python3 build.py", "deploy.sh"},
		"docker compose up -d":                           {"docker compose"},
		"git -C /repo push origin main":                  {"git push"},
		"npm run build | tee out.log":                    {"npm run"},
		"# just a comment":                               nil,
	}
	for in, want := range cases {
		var got []string
		for _, s := range Normalize(in) {
			got = append(got, s.Unit)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestMineFindsTheRecurringRunAndIgnoresNoise(t *testing.T) {
	release := []string{"git status", "go test ./...", "go build ./cmd/x", "git push origin main"}
	var ss []transcript.Session
	// Three sessions, three agents' worth of projects, the same release run with
	// different surrounding noise.
	ss = append(ss, sess("claude-code", "/w/grimoire", 1, append([]string{"ls", "git status", "go test ./...", "go build ./cmd/x", "git push origin main"}, "cat x")...))
	ss = append(ss, sess("codex", "/w/grimoire", 2, "git status", "@edit", "go test ./pkg", "!go build ./bad", "go build ./cmd/y", "git push"))
	ss = append(ss, sess("claude-code", "/w/other", 3, "git status", "go test ./...", "go build .", "git push origin main", "docker compose up"))
	// Two sessions of something else: below the bar.
	ss = append(ss, sess("claude-code", "/w/x", 4, "make lint", "make test", "make docs"))
	ss = append(ss, sess("claude-code", "/w/x", 5, "make lint", "make test", "make docs"))
	// A session that only repeats one program: never a candidate.
	ss = append(ss, sess("codex", "/w/y", 6, "git add a", "git add b", "git add c", "git add d"))
	_ = release

	got := Mine(ss, Options{})
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(got), got)
	}
	c := got[0]
	if !reflect.DeepEqual(c.Units, []string{"git status", "go test", "go build", "git push"}) {
		t.Errorf("units %q", c.Units)
	}
	if c.Sessions != 3 || c.Agents["claude-code"] != 2 || c.Agents["codex"] != 1 {
		t.Errorf("%+v", c)
	}
	if c.Projects[0] != "grimoire" {
		t.Errorf("projects %v", c.Projects)
	}
	if c.Examples[0] != "git status" || c.Examples[1] != "go test ./..." {
		t.Errorf("examples %q", c.Examples)
	}
	if c.ID == "" || c.Title != "git status → go test → go build → git push" {
		t.Errorf("title %q id %q", c.Title, c.ID)
	}
	// Lowering the bar brings in the make run.
	if more := Mine(ss, Options{MinSessions: 2}); len(more) != 2 {
		t.Errorf("min 2: %d candidates", len(more))
	}
}

func TestDraftIsAProcedureNoteTheStoreReadsAsOne(t *testing.T) {
	c := Candidate{ID: "abcd1234", Units: []string{"git status", "go test"}, Examples: []string{"git status", "go test ./... -race"},
		Sessions: 4, Agents: map[string]int{"codex": 1, "claude-code": 3}, Projects: []string{"grimoire"},
		Title: "git status → go test", Description: "x: y",
		First: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Last: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	text := Draft(c)
	n := memstore.ParseNote(DraftFile(c), text, time.Now())
	if n.Kind != memstore.KindProcedure {
		t.Errorf("kind %q\n%s", n.Kind, text)
	}
	for _, want := range []string{"1. `git status`", "2. `go test ./... -race`", "Mined from 4 sessions (claude-code 3, codex 1)", "not a verified procedure", `"x: y"`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if !strings.HasPrefix(DraftFile(c), "procedure_git-status-go-test-abcd") {
		t.Errorf("file %q", DraftFile(c))
	}
	q := Queue([]Candidate{c}, 30*24*time.Hour, time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	if !strings.Contains(q, "nothing here is in memory yet") || !strings.Contains(q, "id `abcd1234`") {
		t.Errorf("queue:\n%s", q)
	}
}

func TestRetitleOnlyChangesTitlesAndSurvivesFailure(t *testing.T) {
	cs := []Candidate{{ID: "11111111", Units: []string{"a", "b"}, Title: "a → b", Description: "d1"}, {ID: "22222222", Units: []string{"c", "d"}, Title: "c → d", Description: "d2"}}
	Retitle(cs, func(c Candidate) (string, string, error) {
		if c.ID == "22222222" {
			return "", "", errTitler
		}
		return "Ship the thing", "Use when shipping.", nil
	})
	if cs[0].Title != "Ship the thing" || cs[0].Description != "Use when shipping." || !cs[0].Titled || !reflect.DeepEqual(cs[0].Units, []string{"a", "b"}) {
		t.Errorf("%+v", cs[0])
	}
	if cs[1].Title != "c → d" || cs[1].Titled {
		t.Errorf("a failing model must change nothing: %+v", cs[1])
	}
}

var errTitler = &titlerErr{}

type titlerErr struct{}

func (*titlerErr) Error() string { return "model down" }

func TestNormalizeHeredocsQuotesAndShellSyntax(t *testing.T) {
	cases := map[string][]string{
		"python3 - <<'PY'\nimport os\nfor x in y:\n  print(x)\nPY\ngit push": {"python3", "git push"},
		"echo \"a && b\" && git status":                                      {"git status"},
		"for f in *.go; do gofmt -l $f; done":                                {"gofmt"},
		"make test || make lint":                                             {"make test", "make lint"},
		"cat <<EOF > out.txt\nhello\nEOF\nsystemctl restart foo":             {"systemctl restart"},
	}
	for in, want := range cases {
		var got []string
		for _, s := range Normalize(in) {
			got = append(got, s.Unit)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
