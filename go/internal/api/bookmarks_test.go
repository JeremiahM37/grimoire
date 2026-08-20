package api

import (
	"net/http"
	"strings"
	"testing"
)

// Bookmarks live in a note, so these check the file as well as the API — the
// point of storing them there is that a person can open and edit them.

func addMark(t *testing.T, h http.Handler, kind, target string) map[string]any {
	t.Helper()
	w := do(t, h, "POST", "/api/bookmarks", map[string]any{"kind": kind, "target": target})
	if w.Code >= 400 {
		t.Fatalf("bookmark %s %q = %d: %s", kind, target, w.Code, w.Body)
	}
	var out map[string]any
	decode(t, w, &out)
	return out
}

func marks(t *testing.T, h http.Handler) []map[string]any {
	t.Helper()
	var out []map[string]any
	decode(t, do(t, h, "GET", "/api/bookmarks", nil), &out)
	return out
}

func seedRunbook(t *testing.T, h http.Handler) {
	t.Helper()
	w := do(t, h, "POST", "/api/notes", map[string]any{
		"title": "Deploy Runbook",
		"body":  "# Deploy Runbook\n\nintro\n\n## Rollback\n\nsteps here\n"})
	if w.Code != http.StatusCreated {
		t.Fatalf("seed = %d: %s", w.Code, w.Body)
	}
}

func TestBookmarkANoteAHeadingASearchAndATag(t *testing.T) {
	_, h := testServer(t)
	seedRunbook(t, h)

	addMark(t, h, "note", "Deploy Runbook")
	addMark(t, h, "heading", "Deploy Runbook#Rollback")
	addMark(t, h, "search", "tag:ops is:pinned")
	addMark(t, h, "tag", "infra")

	got := marks(t, h)
	if len(got) != 4 {
		t.Fatalf("got %d bookmarks: %v", len(got), got)
	}
	byKind := map[string]map[string]any{}
	for _, m := range got {
		byKind[m["kind"].(string)] = m
	}
	if byKind["note"]["path"] != "deploy-runbook.md" {
		t.Errorf("note bookmark did not resolve: %v", byKind["note"])
	}
	// A heading bookmark lands on the section, not at the top of a long note.
	heading := byKind["heading"]
	if heading["path"] != "deploy-runbook.md" || heading["line"].(float64) <= 0 {
		t.Errorf("heading bookmark did not locate the section: %v", heading)
	}
	if !strings.Contains(heading["label"].(string), "Rollback") {
		t.Errorf("heading label = %v", heading["label"])
	}
	if byKind["search"]["label"] != "tag:ops is:pinned" {
		t.Errorf("search bookmark = %v", byKind["search"])
	}
}

func TestBookmarksLiveInAnEditableNote(t *testing.T) {
	// The reason they are not an index table: a bookmarks file that lived in
	// the index would be lost on a reindex and would not reach the phone.
	_, h := testServer(t)
	seedRunbook(t, h)
	addMark(t, h, "note", "Deploy Runbook")
	addMark(t, h, "search", "tag:ops")

	body := noteBody(t, h, BookmarksNote)
	if !strings.Contains(body, "[[Deploy Runbook]]") {
		t.Errorf("note bookmark is not a wiki-link in the file:\n%s", body)
	}
	if !strings.Contains(body, "`tag:ops`") {
		t.Errorf("search bookmark is not readable in the file:\n%s", body)
	}
	if !strings.Contains(body, "# Bookmarks") {
		t.Errorf("the file has no heading:\n%s", body)
	}
}

func TestBookmarkLabelsAreDerivedNotStored(t *testing.T) {
	// A stored copy of a note's title goes stale silently when the note is
	// renamed.
	_, h := testServer(t)
	seedRunbook(t, h)
	addMark(t, h, "note", "deploy-runbook.md")

	w := do(t, h, "PUT", "/api/notes/deploy-runbook.md", map[string]any{
		"body": "# Deploy Runbook\n\nrenamed\n", "frontmatter": map[string]any{
			"title": "Deployment Runbook"}})
	if w.Code >= 400 {
		t.Fatalf("rename = %d: %s", w.Code, w.Body)
	}
	got := marks(t, h)
	if len(got) != 1 || got[0]["label"] != "Deployment Runbook" {
		t.Errorf("label did not follow the rename: %v", got)
	}
}

func TestDuplicateBookmarkIsNotAddedTwice(t *testing.T) {
	_, h := testServer(t)
	seedRunbook(t, h)
	addMark(t, h, "note", "Deploy Runbook")
	second := addMark(t, h, "note", "deploy runbook") // same thing, different case
	if second["created"] != false {
		t.Errorf("a duplicate was added: %v", second)
	}
	if got := marks(t, h); len(got) != 1 {
		t.Errorf("got %d bookmarks, want 1", len(got))
	}
}

func TestRemoveBookmarkLeavesTheRestAlone(t *testing.T) {
	_, h := testServer(t)
	seedRunbook(t, h)
	addMark(t, h, "note", "Deploy Runbook")
	addMark(t, h, "search", "tag:ops")

	w := do(t, h, "DELETE", "/api/bookmarks?kind=search&target=tag%3Aops", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("remove = %d: %s", w.Code, w.Body)
	}
	got := marks(t, h)
	if len(got) != 1 || got[0]["kind"] != "note" {
		t.Fatalf("remove took the wrong one: %v", got)
	}
	if !strings.Contains(noteBody(t, h, BookmarksNote), "# Bookmarks") {
		t.Error("the note's heading was removed with the bookmark")
	}
	if w := do(t, h, "DELETE", "/api/bookmarks?kind=search&target=tag%3Aops", nil); w.Code != http.StatusNotFound {
		t.Errorf("removing a bookmark twice = %d, want 404", w.Code)
	}
}

func TestBookmarkValidation(t *testing.T) {
	_, h := testServer(t)
	for _, body := range []map[string]any{
		{"kind": "everything", "target": "x"},
		{"kind": "note", "target": ""},
		{"kind": "note", "target": strings.Repeat("x", 501)},
		// A target is rendered into a markdown line, so it must not be able to
		// end that line and write another, or close the link it sits in.
		{"kind": "note", "target": "ok]]\n- **note** — [[Injected"},
		{"kind": "search", "target": "back`tick"},
	} {
		if w := do(t, h, "POST", "/api/bookmarks", body); w.Code != http.StatusBadRequest {
			t.Errorf("%v = %d, want 400", body, w.Code)
		}
	}
	if w := do(t, h, "DELETE", "/api/bookmarks?kind=note", nil); w.Code != http.StatusBadRequest {
		t.Errorf("remove with no target = %d, want 400", w.Code)
	}
}

func TestBookmarkToAMissingNoteIsKeptAsDangling(t *testing.T) {
	// Same behaviour as an unresolved wiki-link: the bookmark stands, and says
	// what it points at.
	_, h := testServer(t)
	addMark(t, h, "note", "Not Written Yet")
	got := marks(t, h)
	if len(got) != 1 || got[0]["label"] != "Not Written Yet" {
		t.Fatalf("got %v", got)
	}
	if got[0]["path"] != nil && got[0]["path"] != "" {
		t.Errorf("a dangling bookmark claimed a path: %v", got[0])
	}
}

func TestBookmarksRespectReaderLists(t *testing.T) {
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")
	if _, err := s.WriteNote("restricted.md", "# Restricted\n\nseverance terms\n",
		map[string]any{"title": "Restricted", "readers": "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
	// Alice bookmarks her own note.
	if w := asKey(t, h, aliceKey, "POST", "/api/bookmarks",
		map[string]any{"kind": "note", "target": "restricted.md"}); w.Code >= 400 {
		t.Fatalf("alice = %d: %s", w.Code, w.Body)
	}
	// Bob sees the list without it: the line stays in the file, since it is
	// somebody's bookmark and he is not its owner.
	body := asKey(t, h, bobKey, "GET", "/api/bookmarks", nil).Body.String()
	if strings.Contains(strings.ToLower(body), "restricted") {
		t.Errorf("a bookmark leaked a note bob cannot read:\n%s", body)
	}
	// And he cannot create one either, which would otherwise be a way to probe
	// for a note by watching which targets resolve.
	if w := asKey(t, h, bobKey, "POST", "/api/bookmarks",
		map[string]any{"kind": "note", "target": "restricted.md"}); w.Code != http.StatusNotFound {
		t.Errorf("bob bookmarked a note he cannot read: %d", w.Code)
	}
}

func TestBookmarkParsingIgnoresOtherLines(t *testing.T) {
	// The file is a note, so a person will write in it.
	_, h := testServer(t)
	seedRunbook(t, h)
	addMark(t, h, "note", "Deploy Runbook")

	w := do(t, h, "PUT", "/api/notes/"+BookmarksNote, map[string]any{
		"body": "# Bookmarks\n\nsome prose I typed\n\n" +
			"- **note** — [[Deploy Runbook]]\n" +
			"- a plain bullet\n" +
			"- **nonsense** — [[Deploy Runbook]]\n"})
	if w.Code >= 400 {
		t.Fatalf("edit = %d: %s", w.Code, w.Body)
	}
	if got := marks(t, h); len(got) != 1 {
		t.Errorf("got %v, want only the real bookmark", got)
	}
}
