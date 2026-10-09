package cues

import (
	"strings"
	"testing"
)

// wordEmb embeds text as a bag of its words over a fixed vocabulary, so
// similarity is word overlap: enough to test matching without a model.
type wordEmb struct{ calls int }

var vocab = strings.Fields("deploy release restart service tmux kill session backup disk full port token")

func (e *wordEmb) Embed(texts []string) [][]float32 {
	e.calls += len(texts)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, len(vocab))
		for j, w := range vocab {
			if strings.Contains(strings.ToLower(t), w) {
				v[j] = 1
			}
		}
		out[i] = v
	}
	return out
}
func (e *wordEmb) Signature() string { return "words" }

func TestAddMatchAndPersist(t *testing.T) {
	dir := t.TempDir()
	emb := &wordEmb{}
	s, err := Open(dir, emb)
	if err != nil || s.Len() != 0 {
		t.Fatalf("open empty: %v %d", err, s.Len())
	}
	n, err := s.Add([]Cue{
		{Target: FactTarget("a1"), Kind: Request, Text: "restart the service after a release", Source: Generated},
		{Target: FactTarget("a1"), Kind: Request, Text: "restart the service after a release", Source: Generated}, // duplicate
		{Target: NoteTarget("Agent Memory/tmux.md"), Kind: Action, Text: "tmux kill-server", Source: Agent},
		{Target: NoteTarget("x.md"), Kind: Request, Text: "   ", Source: Agent}, // empty
	})
	if err != nil || n != 2 {
		t.Fatalf("add: n=%d err=%v", n, err)
	}
	m := s.Best(emb.Embed([]string{"kill the tmux session"})[0], 5)
	if len(m) == 0 || m[0].Target != NoteTarget("Agent Memory/tmux.md") {
		t.Fatalf("best match = %+v", m)
	}
	if got := s.Best(emb.Embed([]string{"tmux kill"})[0], 5, Request); len(got) != 1 || got[0].Target != FactTarget("a1") {
		t.Fatalf("kind filter: %+v", got)
	}

	// Reopening reads the same cues and reuses cached vectors.
	emb2 := &wordEmb{}
	s2, err := Open(dir, emb2)
	if err != nil || s2.Len() != 2 {
		t.Fatalf("reopen: %v len=%d", err, s2.Len())
	}
	if emb2.calls != 0 {
		t.Fatalf("reopen re-embedded %d cues; the vector cache should cover them", emb2.calls)
	}
	if got := s2.For(FactTarget("a1")); len(got) != 1 || got[0].Source != Generated {
		t.Fatalf("for: %+v", got)
	}
}

func TestLongCueIsTruncated(t *testing.T) {
	s, _ := Open(t.TempDir(), &wordEmb{})
	if _, err := s.Add([]Cue{{Target: FactTarget("b"), Kind: Request, Text: strings.Repeat("é", MaxText+50)}}); err != nil {
		t.Fatal(err)
	}
	if got := []rune(s.For(FactTarget("b"))[0].Text); len(got) != MaxText {
		t.Fatalf("len %d", len(got))
	}
}

func TestNoEmbedderStillStores(t *testing.T) {
	s, _ := Open(t.TempDir(), nil)
	if n, err := s.Add([]Cue{{Target: FactTarget("c"), Kind: Keywords, Text: "disk full"}}); n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := s.Best([]float32{1, 0}, 3); len(got) != 0 {
		t.Fatalf("matched without vectors: %+v", got)
	}
}

func TestTriggers(t *testing.T) {
	cases := []struct {
		cue, action string
		want        bool
	}{
		{"/etc/systemd/system/grimoire.service.d/release.conf", "Edit /etc/systemd/system/grimoire.service.d/release.conf", true},
		{"~/projects/lectern/.claude/hooks/pre_tool_use.sh", "Bash cat /home/admin/projects/lectern/.claude/hooks/pre_tool_use.sh", true},
		{"systemctl restart grimoire", "Bash systemctl restart lectern", false},
		{"docker system prune", "Bash docker system prune -af", false}, // no exact name: left to embeddings
		{"edit go.mod", "Edit /x/go.sum", false},
		{"cat docs/FRESHNESS.md", "Read the docs/FRESHNESS.md file", true},
	}
	for _, c := range cases {
		if got := Triggers(c.cue, c.action); got != c.want {
			t.Errorf("Triggers(%q, %q) = %v, want %v", c.cue, c.action, got, c.want)
		}
	}
}
