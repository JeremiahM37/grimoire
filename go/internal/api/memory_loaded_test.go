package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
)

// The loaded experiment: note items the agent already has are not injected
// again. Off, the endpoint is unchanged and reports nothing new.

const loadedNoteText = "# Lab\n\nthe kestrel ports live in the lab config\n"

func loadedServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s, h := testServer(t)
	if _, err := s.Vault.Write("Projects/lab/notes.md", loadedNoteText, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Sync(); err != nil {
		t.Fatal(err)
	}
	return s, h
}

// loadedContext calls the endpoint and returns the raw body and the decoded
// loaded_skipped count, which is absent when the key is not in the response.
func loadedContext(t *testing.T, h http.Handler, query string) (string, map[string]any) {
	t.Helper()
	rec := do(t, h, "GET", "/api/memory/context?"+query, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("context %s: %d %s", query, rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return rec.Body.String(), out
}

func TestLoadedChangesNothingWhenTheFlagIsOff(t *testing.T) {
	_, h := loadedServer(t)
	plain, plainOut := loadedContext(t, h, "q=kestrel+ports")
	withParam, withOut := loadedContext(t, h, "q=kestrel+ports&loaded=Projects/")
	if plain != withParam {
		t.Fatalf("flag off: loaded= changed the response\nplain: %s\nwith:  %s", plain, withParam)
	}
	if !strings.Contains(plain, "kestrel") {
		t.Fatalf("the note was not injected at all, so the test proves nothing: %s", plain)
	}
	if _, ok := plainOut["loaded_skipped"]; ok {
		t.Fatalf("flag off reported loaded_skipped: %v", plainOut)
	}
	if _, ok := withOut["loaded_skipped"]; ok {
		t.Fatalf("flag off reported loaded_skipped with loaded=: %v", withOut)
	}
}

func TestLoadedNotesAreDroppedWhenDeclaredOrConfigured(t *testing.T) {
	restore := exp.Set(loadedFlag)
	defer restore()
	s, h := loadedServer(t)

	ctx, out := loadedContext(t, h, "q=kestrel+ports")
	if !strings.Contains(ctx, "kestrel") || out["loaded_skipped"] != float64(0) {
		t.Fatalf("flag on, nothing declared: want the note and loaded_skipped 0: %s %v", ctx, out)
	}

	ctx, out = loadedContext(t, h, "q=kestrel+ports&loaded=Projects/")
	if strings.Contains(ctx, "kestrel") || out["loaded_skipped"] != float64(1) {
		t.Fatalf("declared loaded=Projects/: want the note dropped and loaded_skipped 1: %s %v", ctx, out)
	}

	// A bare file path and a glob that names the same directory both count.
	ctx, out = loadedContext(t, h, "q=kestrel+ports&loaded=Projects/lab/notes.md")
	if strings.Contains(ctx, "kestrel") || out["loaded_skipped"] != float64(1) {
		t.Fatalf("exact path: %s %v", ctx, out)
	}
	ctx, out = loadedContext(t, h, "q=kestrel+ports&loaded=Projects/*")
	if strings.Contains(ctx, "kestrel") || out["loaded_skipped"] != float64(1) {
		t.Fatalf("glob: %s %v", ctx, out)
	}
	// A segment prefix is not a directory prefix.
	ctx, out = loadedContext(t, h, "q=kestrel+ports&loaded=Proj")
	if !strings.Contains(ctx, "kestrel") || out["loaded_skipped"] != float64(0) {
		t.Fatalf("\"Proj\" must not match Projects/: %s %v", ctx, out)
	}

	if err := s.Settings.Update(map[string]string{"context_loaded_paths": "Projects/lab"}); err != nil {
		t.Fatal(err)
	}
	ctx, out = loadedContext(t, h, "q=kestrel+ports")
	if strings.Contains(ctx, "kestrel") || out["loaded_skipped"] != float64(1) {
		t.Fatalf("configured context_loaded_paths: %s %v", ctx, out)
	}
}

func TestLoadedRejectsBadParametersOnlyWhenTheFlagIsOn(t *testing.T) {
	_, h := loadedServer(t)
	bad := []string{"loaded=../etc", "loaded=/abs", "loaded=a%5Cb"}
	for _, p := range bad {
		if rec := do(t, h, "GET", "/api/memory/context?q=kestrel&"+p, nil); rec.Code != http.StatusOK {
			t.Fatalf("flag off: %s rejected with %d", p, rec.Code)
		}
	}

	restore := exp.Set(loadedFlag)
	defer restore()
	for _, p := range bad {
		if rec := do(t, h, "GET", "/api/memory/context?q=kestrel&"+p, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("flag on: %s got %d, want 400", p, rec.Code)
		}
	}
	many := strings.Repeat("loaded=Projects/&", 33)
	if rec := do(t, h, "GET", "/api/memory/context?q=kestrel&"+many, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("33 loaded paths got %d, want 400", rec.Code)
	}
	ok := strings.Repeat("loaded=Projects/&", 32)
	if rec := do(t, h, "GET", "/api/memory/context?q=kestrel&"+ok, nil); rec.Code != http.StatusOK {
		t.Fatalf("32 loaded paths got %d, want 200", rec.Code)
	}
}

// At the action stage a cue that names a note's path outright still brings
// that note back, even though the agent is declared to have it loaded.
func TestLoadedKeepsANoteACueFiredOnAtTheActionStage(t *testing.T) {
	restore := exp.Set(loadedFlag)
	defer restore()
	_, h := loadedServer(t)
	rec := do(t, h, "POST", "/api/memory/cues", map[string]any{"target": "note:Projects/lab/notes.md",
		"cues": []string{"/srv/lab/kestrel.conf"}, "kind": "action", "source": "agent"})
	if rec.Code != http.StatusOK {
		t.Fatalf("cues: %d %s", rec.Code, rec.Body)
	}
	action := "rank=hybrid&stage=action&loaded=Projects/&q=" + "Edit+/srv/lab/kestrel.conf"
	ctx, out := loadedContext(t, h, action)
	if !strings.Contains(ctx, "kestrel") || out["loaded_skipped"] != float64(0) {
		t.Fatalf("action trigger on a loaded note: want it kept: %s %v", ctx, out)
	}
	// The same note, not triggered at the request stage, is still dropped.
	ctx, out = loadedContext(t, h, "rank=hybrid&stage=request&loaded=Projects/&q=Edit+/srv/lab/kestrel.conf")
	if strings.Contains(ctx, "kestrel") {
		t.Fatalf("request stage: a loaded note was injected: %s", ctx)
	}
	if out["loaded_skipped"] != float64(1) {
		t.Fatalf("request stage: loaded_skipped = %v, want 1", out["loaded_skipped"])
	}
}
