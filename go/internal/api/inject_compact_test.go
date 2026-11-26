package api

import (
	"strings"
	"testing"
)

// A note chunk is injected as the sentences that match the request, plain:
// no tables, headings, emphasis, emoji or frontmatter, and no line breaks
// turned into sentence ends.
func TestCompactNoteKeepsMatchingSentencesPlain(t *testing.T) {
	chunk := "---\ntags: [x]\n---\n# 🩹 Deploy notes\n\n**Two details** worth having ready:\n" +
		"| step | what |\n|---|---|\n| one | build |\n\n" +
		"The live binary is installed into /opt/app/current and the\nservice is restarted after the copy.\n" +
		"> Unrelated: the cat food order goes out on Fridays.\n"
	got := compactNote(chunk, contextTerms("how do I restart the service after installing the binary"), noteExcerptMax)
	for _, bad := range []string{"**", "|", "#", "---", "🩹", "tags:", "the. service"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q left in %q", bad, got)
		}
	}
	if !strings.Contains(got, "The live binary is installed into /opt/app/current and the service is restarted after the copy.") {
		t.Fatalf("the matching sentence is missing or broken: %q", got)
	}
	if strings.Contains(got, "cat food") {
		t.Fatalf("a sentence that matches nothing was kept: %q", got)
	}
}

func TestCompactNoteWithoutMatchKeepsTheOpening(t *testing.T) {
	got := compactNote("First line of the note.\n\nSecond paragraph here.", nil, noteExcerptMax)
	if got != "First line of the note. Second paragraph here." {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("word ", 200)
	if n := len([]rune(compactNote(long, nil, 100))); n > 100 {
		t.Fatalf("excerpt is %d runes, over its limit", n)
	}
}

func TestCompactNotePrefersWholeSentencesOverCutCommands(t *testing.T) {
	chunk := "Restart the service with … then … and check it.\n\nRestart the service from the unit file; it comes back in a second."
	got := compactNote(chunk, contextTerms("restart the service"), 70)
	if !strings.HasPrefix(got, "Restart the service from the unit file") {
		t.Fatalf("got %q", got)
	}
}

func TestCompactFactIsPlain(t *testing.T) {
	if got := compactFact("Use **`tmux -L`** sockets, never the default one"); got != "Use tmux -L sockets, never the default one." {
		t.Fatalf("got %q", got)
	}
}

// The same paragraph pasted into several notes is injected once.
func TestNearDuplicate(t *testing.T) {
	a := wordShingles("grimoire is a context server for agents with hybrid retrieval and a credential broker")
	b := wordShingles("Open source: grimoire is a context server for agents with hybrid retrieval and a credential broker.")
	c := wordShingles("lectern restarts every session with its own conversation after an update")
	if !nearDuplicate(b, []map[string]bool{a}) {
		t.Fatal("a reworded copy should count as a duplicate")
	}
	if nearDuplicate(c, []map[string]bool{a}) {
		t.Fatal("different text is not a duplicate")
	}
}

func TestContextSkipPaths(t *testing.T) {
	got := contextSkipPaths("Job Search/, Chats/\nDreams/,, ../etc")
	if strings.Join(got, "|") != "Job Search/|Chats/|Dreams/" {
		t.Fatalf("got %q", got)
	}
	if !inScope("Job Search/Resumes/a.md", got) || inScope("Projects/x.md", got) {
		t.Fatal("prefix matching is wrong")
	}
}

// A request of nothing but stopwords shares nothing with a memory; it must
// not be scored as a full match.
func TestRankOverlapEmptyRequest(t *testing.T) {
	if terms := contextTerms("k do that then"); len(terms) != 0 {
		t.Fatalf("terms = %v", terms)
	}
	if rankOverlap(nil, "anything at all") != 0 {
		t.Fatal("no terms must mean no overlap for ranking")
	}
	if contextOverlap(nil, "anything") != 1 {
		t.Fatal("the lexical listing keeps its no-query behaviour")
	}
}
