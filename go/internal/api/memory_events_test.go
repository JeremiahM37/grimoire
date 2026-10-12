package api

import (
	"strings"
	"testing"
)

// Automatic event marking: a handoff's past-tense facts are history without the
// caller having to say so. Explicit recall behaviour is in memory_events_flag_test.go.

func eventIDs(out map[string]any) []string {
	raw, _ := out["events_auto"].([]any)
	var ids []string
	for _, v := range raw {
		ids = append(ids, v.(string))
	}
	return ids
}

func TestSplitWriteMarksPastTenseFactsAsEvents(t *testing.T) {
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops",
		"text": "Fixed the proxy config. Deployed the cache change on 2026-10-10."})
	if ids := eventIDs(out); len(ids) != 2 {
		t.Fatalf("events_auto = %v, want both facts; response %v", ids, out)
	}
	body := noteBody(t, h, "memory/ops.md")
	if strings.Count(body, "evt=1") != 2 {
		t.Fatalf("both facts should carry evt=1:\n%s", body)
	}
}

func TestHandoffCategoryMarksASingleFactAsEvent(t *testing.T) {
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops",
		"category": "handoff", "text": "Deployed the cache change."})
	if ids := eventIDs(out); len(ids) != 1 {
		t.Fatalf("events_auto = %v, want one; response %v", ids, out)
	}
	if !strings.Contains(noteBody(t, h, "memory/ops.md"), "evt=1") {
		t.Fatal("handoff fact not marked evt=1")
	}
}

func TestSingleFactOutsideHandoffIsNotMarked(t *testing.T) {
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops",
		"text": "Deployed the cache change."})
	if out["events_auto"] != nil || strings.Contains(noteBody(t, h, "memory/ops.md"), "evt=1") {
		t.Fatalf("a lone past-tense fact outside a handoff was marked: %v", out)
	}
}

func TestPresentTenseHandoffFactIsNotMarked(t *testing.T) {
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops",
		"category": "handoff", "text": "The proxy runs on mediaserver."})
	if out["events_auto"] != nil {
		t.Fatalf("a present-tense claim was marked as history: %v", out)
	}
}

func TestExplicitEventFalseIsRespected(t *testing.T) {
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops",
		"category": "handoff", "event": false, "text": "Deployed the cache change."})
	if out["events_auto"] != nil || strings.Contains(noteBody(t, h, "memory/ops.md"), "evt=1") {
		t.Fatalf("event:false was overridden: %v", out)
	}
}

// A fact with an anchor is a claim with a check, never history.
func TestAnchoredHandoffFactIsNeverAutoMarked(t *testing.T) {
	_, h := testServer(t)
	lit := portFile(t, "9000")
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops",
		"category": "handoff", "text": "Deployed the cache change.",
		"anchors": []map[string]any{{"type": "file_contains", "path": lit, "text": "KESTREL_PORT=9000"}}})
	if out["events_auto"] != nil || strings.Contains(noteBody(t, h, "memory/ops.md"), "evt=1") {
		t.Fatalf("an anchored fact was auto-marked: %v", out)
	}
}

func TestEventAutoSwitchOff(t *testing.T) {
	t.Setenv("GRIMOIRE_EVENT_AUTO", "0")
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops",
		"text": "Fixed the proxy config. Deployed the cache change."})
	if out["events_auto"] != nil || strings.Contains(noteBody(t, h, "memory/ops.md"), "evt=1") {
		t.Fatalf("GRIMOIRE_EVENT_AUTO=0 still marked events: %v", out)
	}
}

// The advice about setting event is dropped when the store marked the facts
// itself, and kept otherwise.
func TestAdviceToSetEventIsDroppedOnlyWhenMarked(t *testing.T) {
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops",
		"category": "handoff", "text": "Deployed the cache change."})
	for _, a := range asStrings(out["advice"]) {
		if strings.Contains(a, "reads like something that happened") {
			t.Fatalf("advice asks for event after the store marked it: %v", out["advice"])
		}
	}
	t.Setenv("GRIMOIRE_EVENT_AUTO", "0")
	out = rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops2",
		"text": "Deployed the cache change."})
	found := false
	for _, a := range asStrings(out["advice"]) {
		found = found || strings.Contains(a, "reads like something that happened")
	}
	if !found {
		t.Fatalf("advice missing with the switch off: %v", out["advice"])
	}
}

func asStrings(v any) []string {
	raw, _ := v.([]any)
	var out []string
	for _, x := range raw {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}
