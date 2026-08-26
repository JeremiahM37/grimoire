package api

import (
	"net/http"
	"strings"
	"testing"
)

// Renaming a note used to move the file and stop, leaving every [[link]] aimed
// at the old name pointing at nothing. No error, no warning — the graph just
// loses the edges and backlinks that were there yesterday are gone.

func TestRenameRepointsInboundLinks(t *testing.T) {
	s, h := testServer(t)
	if _, err := s.WriteNote("deploy.md", "# Deploy\n\nhow to ship\n",
		map[string]any{"title": "Deploy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteNote("runbook.md",
		"# Runbook\n\nsee [[Deploy]] and also [[deploy]] for details\n", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "POST", "/api/notes/deploy.md/rename",
		map[string]any{"to": "shipping.md"})
	if w.Code != http.StatusOK {
		t.Fatalf("rename = %d: %s", w.Code, w.Body)
	}

	note, err := s.Vault.Read("runbook.md")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(note.Body, "[[Deploy]]") || strings.Contains(note.Body, "[[deploy]]") {
		t.Errorf("a link still points at the old name:\n%s", note.Body)
	}
	if !strings.Contains(note.Body, "shipping") {
		t.Errorf("links were not repointed at the new name:\n%s", note.Body)
	}
	// The words the reader sees must not change because a file moved.
	if !strings.Contains(note.Body, "|Deploy]]") {
		t.Errorf("display text was lost, so a rename silently edited a sentence:\n%s", note.Body)
	}
}

// The rewrite must not reach links that never pointed here. It works from the
// index's resolved targets, so a same-named note elsewhere is untouched.
func TestRenameLeavesUnrelatedLinksAlone(t *testing.T) {
	s, h := testServer(t)
	for path, body := range map[string]string{
		"deploy.md":             "# Deploy\n\nthe one being moved\n",
		"archive/old-deploy.md": "# Old Deploy\n\nsomething else entirely\n",
		"other.md":              "# Other\n\nlinks to [[Old Deploy]] only\n",
	} {
		if _, err := s.WriteNote(path, body, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
	before, err := s.Vault.Read("other.md")
	if err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "POST", "/api/notes/deploy.md/rename",
		map[string]any{"to": "shipping.md"}); w.Code != http.StatusOK {
		t.Fatalf("rename = %d: %s", w.Code, w.Body)
	}
	after, err := s.Vault.Read("other.md")
	if err != nil {
		t.Fatal(err)
	}
	if after.Body != before.Body {
		t.Errorf("a note linking to a DIFFERENT note was rewritten:\nbefore: %q\nafter:  %q",
			before.Body, after.Body)
	}
}

// Anchors and existing display text are part of the link a person wrote.
func TestRelinkPreservesAnchorsAndDisplayText(t *testing.T) {
	old := map[string]bool{"deploy": true}
	for _, tc := range []struct{ in, want string }{
		{"see [[Deploy]]", "see [[shipping|Deploy]]"},
		{"see [[Deploy|the runbook]]", "see [[shipping|the runbook]]"},
		{"see [[Deploy#Rollback]]", "see [[shipping#Rollback|Deploy]]"},
		{"see [[Deploy#Rollback|how to undo]]", "see [[shipping#Rollback|how to undo]]"},
		{"embed ![[Deploy]]", "embed ![[shipping|Deploy]]"},
		{"unrelated [[Monitoring]]", "unrelated [[Monitoring]]"},
	} {
		got, _ := relinkBody(tc.in, old, "shipping")
		if got != tc.want {
			t.Errorf("relinkBody(%q)\n got  %q\n want %q", tc.in, got, tc.want)
		}
	}
}

// Every spelling that used to reach the note has to be rewritten, or the ones
// missed stay broken and the rename looks like it worked.
func TestLinkAliasesCoversEverySpelling(t *testing.T) {
	got := linkAliases("projects/deploy-guide.md", "Deploy Guide", []string{"Shipping", "How to Ship"})
	for _, want := range []string{
		"projects/deploy-guide.md", "projects/deploy-guide", "deploy-guide",
		"deploy guide", "shipping", "how to ship",
	} {
		if !got[want] {
			t.Errorf("%q is a way to reach the note and is not covered: %v", want, got)
		}
	}
}

func TestRelinkIsANoOpWithNothingToDo(t *testing.T) {
	body := "no links here at all"
	got, n := relinkBody(body, map[string]bool{"deploy": true}, "shipping")
	if got != body || n != 0 {
		t.Errorf("body changed with no matching links: %q (%d)", got, n)
	}
	got, n = relinkBody("has [[Something]]", nil, "shipping")
	if got != "has [[Something]]" || n != 0 {
		t.Errorf("rewrote with an empty target set: %q (%d)", got, n)
	}
}
