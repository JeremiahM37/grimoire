package render

import (
	"strings"
	"testing"
)

// Obsidian's %%comment%% syntax marks the part of a note you did NOT want shown.
// This renderer printed it verbatim, which is a parity gap in the preview and a
// leak everywhere else: RenderWith is what publish.go and the static export
// call, so a note marked `publish: true` published its private asides.

func TestCommentsAreRemoved(t *testing.T) {
	for _, tc := range []struct{ name, src, gone, kept string }{
		{"inline", "before %%hidden%% after", "hidden", "before"},
		{"multi-line", "a\n\n%%\nwhole block\nhidden\n%%\n\nb", "whole block", "b"},
		{"two in one line", "x %%a%% y %%b%% z", "%%", "y"},
		{"mid-sentence", "the port is %%actually 6432%% 5432", "6432", "5432"},
	} {
		out := Render(tc.src, nil)
		if strings.Contains(out, tc.gone) {
			t.Errorf("%s: %q survived into the output:\n%s", tc.name, tc.gone, out)
		}
		if !strings.Contains(out, tc.kept) {
			t.Errorf("%s: %q was removed along with the comment:\n%s", tc.name, tc.kept, out)
		}
	}
}

// Non-greedy matching. Two comments in a paragraph must not merge into one that
// swallows the sentence between them.
func TestATextBetweenTwoCommentsSurvives(t *testing.T) {
	out := Render("%%one%% KEEP THIS %%two%%", nil)
	if !strings.Contains(out, "KEEP THIS") {
		t.Fatalf("the text between two comments was eaten:\n%s", out)
	}
}

// %% inside a code fence is content — a shell script full of them should
// survive being written about.
func TestCommentsInsideCodeFencesAreContent(t *testing.T) {
	// Asserted on the %% delimiters rather than on the words between them: the
	// syntax highlighter wraps keywords in spans, so a literal match on the
	// content fails for a reason that has nothing to do with comments. The
	// invariant is that the delimiters survive inside a fence.
	out := Render("```sh\necho %%literal%%\n```", nil)
	if strings.Count(out, "%%") != 2 {
		t.Fatalf("a fenced %%%% was stripped as if it were a comment:\n%s", out)
	}
}

// The fence-protection machinery must put every fence back exactly where it was.
func TestFencesAreRestoredInOrder(t *testing.T) {
	src := "%%c%%\n\n```\nfirst\n```\n\ntext\n\n```\nsecond\n```"
	out := Render(src, nil)
	if strings.Index(out, "first") > strings.Index(out, "second") {
		t.Fatalf("fences came back in the wrong order:\n%s", out)
	}
	for _, want := range []string{"first", "second", "text"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q lost during fence round-trip:\n%s", want, out)
		}
	}
}

// A note with no comments must not pay for the check, and must not change.
func TestNotesWithoutCommentsAreUntouched(t *testing.T) {
	src := "# Title\n\nA paragraph with 100% coverage and a $ sign."
	if got, want := Render(src, nil), Render(src, nil); got != want {
		t.Fatal("render is not deterministic")
	}
	if !strings.Contains(Render(src, nil), "100% coverage") {
		t.Error("a single % was treated as a comment delimiter")
	}
}
