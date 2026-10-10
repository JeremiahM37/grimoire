package main

import (
	"strings"
	"testing"
)

func TestWatchLineShowsWhatChangedAndWhoWroteIt(t *testing.T) {
	line := formatWatchLine("memory.superseded",
		`{"at":"2026-10-10 14:03","event":"memory.superseded","id":"new1","path":"memory/prefs.md",`+
			`"agent":"claude-code","text":"the user prefers tabs","replaced_text":"the user prefers spaces"}`)
	for _, want := range []string{"2026-10-10 14:03", "memory.superseded", "new1",
		"claude-code", "memory/prefs.md", "the user prefers tabs", "(was: the user prefers spaces)"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q is missing %q", line, want)
		}
	}
}

func TestWatchLineMarksWhyAFactWentAwayAndWhatDisputesIt(t *testing.T) {
	forgot := formatWatchLine("memory.forgotten", `{"at":"2026-10-10 14:05","id":"x","reason":"expired","text":"old"}`)
	if !strings.Contains(forgot, "[expired]") {
		t.Errorf("expiry not marked: %q", forgot)
	}
	dispute := formatWatchLine("memory.disputed", `{"at":"2026-10-10 14:06","id":"fact","challenger_id":"claim","text":"x"}`)
	if !strings.Contains(dispute, "(challenged by claim)") {
		t.Errorf("challenger not shown: %q", dispute)
	}
}

func TestWatchLineSurvivesAMalformedFrame(t *testing.T) {
	if got := formatWatchLine("memory.added", "not json"); !strings.Contains(got, "not json") {
		t.Errorf("a malformed frame should be shown, not dropped: %q", got)
	}
}
