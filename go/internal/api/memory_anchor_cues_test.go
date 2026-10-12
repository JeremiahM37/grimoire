package api

import (
	"slices"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/cues"
	"github.com/JeremiahM37/grimoire/go/internal/exp"
)

// A fact anchored to a file is an action cue for that file, behind the anchor
// flag, so it can come to mind when an action touches the file.

func TestAnchoredFactBecomesAnActionCueForItsFileUnderTheFlag(t *testing.T) {
	restore := exp.Set(anchorFlag)
	defer restore()
	s, h := testServer(t)
	lit := portFile(t, "9000")
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": "the kestrel port lives in the conf",
		"anchors": []map[string]any{{"type": "file_contains", "path": lit, "text": "KESTREL_PORT=9000"}}})
	id := entryByText(t, h, "memory/net.md", "the kestrel port lives in the conf").ID

	action := "sed -i s/9000/9001/ " + lit
	got := s.cues().Triggered(action)
	if !slices.Contains(got, cues.FactTarget(id)) {
		t.Fatalf("Triggered(%q) = %v, want the anchored fact %s", action, got, cues.FactTarget(id))
	}
}

func TestAnchorCuesAreNotAddedWithTheFlagOff(t *testing.T) {
	s, h := testServer(t)
	lit := portFile(t, "9000")
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": "the kestrel port lives in the conf",
		"anchors": []map[string]any{{"type": "file_contains", "path": lit, "text": "KESTREL_PORT=9000"}}})
	if got := s.cues().Triggered("sed -i " + lit); len(got) != 0 {
		t.Fatalf("flag off: anchor produced cues %v", got)
	}
}

// An anchor about another machine names a path there, not here, so it is not
// a key for this machine's actions.
func TestAnchorOnAnotherMachineIsNotACue(t *testing.T) {
	restore := exp.Set(anchorFlag)
	defer restore()
	fakeHost(t, "aiserver")
	s, h := testServer(t)
	lit := portFile(t, "9000")
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": "the kestrel port lives in the conf",
		"anchors": []map[string]any{{"type": "file_contains", "path": lit, "text": "KESTREL_PORT=9000", "host": "mediaserver"}}})
	if body := noteBody(t, h, "memory/net.md"); !strings.Contains(body, "anc=") {
		t.Fatalf("the other-machine anchor was not stored, so the test proves nothing:\n%s", body)
	}
	if got := s.cues().Triggered("sed -i " + lit); len(got) != 0 {
		t.Fatalf("an anchor on another machine became a local cue: %v", got)
	}
}
