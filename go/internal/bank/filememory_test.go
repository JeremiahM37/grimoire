package bank

import (
	"context"
	"strings"
	"testing"
)

func TestMentionsPath(t *testing.T) {
	for _, c := range []struct {
		text, rel string
		want      bool
	}{
		{"Edited src/app/main.go to fix the race", "src/app/main.go", true},
		{"Edited app/main.go.", "src/app/main.go", true},
		{"Edited src/app/main.gold", "src/app/main.go", false},
		{"the main.go file", "main.go", true},
		{"the domain.go file", "main.go", false},
	} {
		if got := mentionsPath(c.text, c.rel); got != c.want {
			t.Errorf("mentionsPath(%q, %q) = %v", c.text, c.rel, got)
		}
	}
}

func TestFileMemoryFindsFactsAndDigestsAndRanksPeopleFirst(t *testing.T) {
	h := newHarness(t, false)
	h.retain(t, "b", Item{Content: "The retry loop in src/queue/worker.go was rewritten after the outage.", DocumentID: "d1"},
		Item{Content: "The CI config lives in .github/ci.yml.", DocumentID: "d2"})
	h.editFile(t, FactsPath("b", "d1"), "was rewritten after the outage", "was rewritten after the outage (my note)")
	if _, err := h.e.WriteDigest(context.Background(), "b", DigestInput{SessionID: "s1",
		Activity: SessionActivity{Files: []string{"src/queue/worker.go"}},
		Turns:    []SessionTurn{{Speaker: "user", Text: "Fix the worker."}}}); err != nil {
		t.Fatal(err)
	}
	items, err := h.e.FileMemory("b", "src/queue/worker.go", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) < 2 || !items[0].Human || !strings.Contains(items[0].Text, "my note") {
		t.Fatalf("a person's entry first: %+v", items)
	}
	hasDigest := false
	for _, it := range items {
		hasDigest = hasDigest || it.Kind == "digest"
		if strings.Contains(it.Text, "ci.yml") {
			t.Errorf("unrelated entry: %+v", it)
		}
	}
	if !hasDigest {
		t.Errorf("the digest naming the file is missing: %+v", items)
	}
	if got, _ := h.e.FileMemory("b", "src/other.go", 10); len(got) != 0 {
		t.Errorf("no memory expected: %+v", got)
	}
	// The person's text is untouched by being listed.
	if !strings.Contains(h.read(t, FactsPath("b", "d1")), "(my note)") {
		t.Fatal("person's text lost")
	}
}
