package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
)

// The staleness floor, the all-automatic anchor rule and the where field. All of
// it sits behind the "anchor" flag.

const floorText = "the homelab agent is deployed at v1.15.0"

// verdictOf returns the recall verdict for the one fact a query finds. Unlike
// freshnessAction it accepts a verdict with no reason, which the plain use
// verdict is.
func verdictOf(t *testing.T, h http.Handler, q string) (action, reason string) {
	t.Helper()
	var facts []map[string]any
	if err := json.Unmarshal([]byte(recallJSON(t, h, q)), &facts); err != nil || len(facts) == 0 {
		t.Fatalf("recall %q: %v", q, err)
	}
	f, ok := facts[0]["freshness"].(map[string]any)
	if !ok {
		t.Fatalf("recall %q: no freshness on %v", q, facts[0])
	}
	action, _ = f["action"].(string)
	reason, _ = f["reason"].(string)
	return action, reason
}

func TestStalenessFloorAsksForACheckOfVolatileClaimWithNoAnchor(t *testing.T) {
	_, h := testServer(t)
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops", "text": floorText})
	if w := recallJSON(t, h, "homelab+agent+deployed"); !strings.Contains(w, `"action":"use"`) || strings.Contains(w, "volatile wording") {
		t.Fatalf("flag off: recall = %s, want the plain use verdict", w)
	}
	restore := exp.Set(anchorFlag)
	defer restore()
	action, reason := verdictOf(t, h, "homelab+agent+deployed")
	if action != "verify" || reason != "volatile wording and no anchor" {
		t.Fatalf("flag on: action = %q reason = %q, want verify for volatile wording", action, reason)
	}
}

func TestStalenessFloorLeavesAnchoredEventsAndPreferencesAlone(t *testing.T) {
	restore := exp.Set(anchorFlag)
	defer restore()
	_, h := testServer(t)
	lit := portFile(t, "9000")
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops", "text": floorText,
		"anchors": []map[string]any{{"type": "file_contains", "path": lit, "text": "KESTREL_PORT=9000"}}})
	if action, reason := verdictOf(t, h, "homelab+agent+deployed"); action != "use" || reason != "stored anchors hold" {
		t.Fatalf("anchored volatile claim: action = %q reason = %q, want use", action, reason)
	}

	_, h2 := testServer(t)
	rememberFrom(t, h2, anchorLoopback, map[string]any{"topic": "ops",
		"text": "the homelab agent was deployed at v1.15.0", "event": true})
	if action, _ := verdictOf(t, h2, "homelab+agent+deployed"); action != "use" {
		t.Fatalf("event with volatile wording: action = %q, want use", action)
	}

	_, h3 := testServer(t)
	rememberFrom(t, h3, anchorLoopback, map[string]any{"topic": "prefs",
		"text": "the homelab agent should stay at v1.15.0", "kind": "preference"})
	if action, _ := verdictOf(t, h3, "homelab+agent+stay"); action != "use" {
		t.Fatalf("preference with volatile wording: action = %q, want use", action)
	}
}

// Automatic anchors that hold are weak: the model's verdict stands, and the
// reason is not "stored anchors hold".
func TestPassingAutoAnchorsDoNotForceUse(t *testing.T) {
	restore := exp.Set(anchorFlag)
	defer restore()
	s, h := testServer(t)
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": "kestrel gateway uses the lan segment", "fresh": "stable"})
	storeAnchor(t, s, "memory/net.md", "kestrel gateway uses the lan segment",
		"ref_path;path="+portFile(t, "9000")+";auto=1")
	action, reason := verdictOf(t, h, "kestrel+gateway+segment")
	if action != "use" || reason != "stable: declared not to change" {
		t.Fatalf("auto anchor that holds: action = %q reason = %q, want the model's stable verdict", action, reason)
	}
}

// A failing automatic anchor still sends the fact to verify, and names the
// thing that is missing.
func TestFailingAutoAnchorNamesTheMissingThing(t *testing.T) {
	restore := exp.Set(anchorFlag)
	defer restore()
	s, h := testServer(t)
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": "kestrel gateway uses the lan segment", "fresh": "stable"})
	gone := portFile(t, "9000") + ".gone"
	storeAnchor(t, s, "memory/net.md", "kestrel gateway uses the lan segment",
		"ref_path;path="+gone+";auto=1")
	action, reason := verdictOf(t, h, "kestrel+gateway+segment")
	if action != "verify" || !strings.Contains(reason, "ref_path") || !strings.Contains(reason, gone) {
		t.Fatalf("failing auto anchor: action = %q reason = %q, want verify naming %s", action, reason, gone)
	}
}

// A claim about another machine says so in its freshness verdict, under the flag.
func TestRecallNamesTheOtherMachineUnderTheFlag(t *testing.T) {
	fakeHost(t, "aiserver")
	_, h := testServer(t)
	rememberFrom(t, h, anchorRemote, map[string]any{"topic": "net", "infer": false,
		"text": "kestrel proxy runs on the other box", "host": "mediaserver"})
	if w := recallJSON(t, h, "kestrel+proxy+other"); strings.Contains(w, `"where"`) {
		t.Fatalf("flag off: where appeared: %s", w)
	}
	restore := exp.Set(anchorFlag)
	defer restore()
	var facts []map[string]any
	decode(t, do(t, h, "GET", "/api/memory?q=kestrel+proxy+other", nil), &facts)
	if len(facts) == 0 {
		t.Fatal("no recall")
	}
	f := facts[0]["freshness"].(map[string]any)
	if f["where"] != "mediaserver" {
		t.Fatalf("freshness.where = %v, want mediaserver", f["where"])
	}
}

// A local claim gets no where.
func TestRecallOmitsWhereForThisMachine(t *testing.T) {
	fakeHost(t, "aiserver")
	_, h := testServer(t)
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "text": "kestrel proxy runs here"})
	restore := exp.Set(anchorFlag)
	defer restore()
	if w := recallJSON(t, h, "kestrel+proxy+runs"); strings.Contains(w, `"where"`) {
		t.Fatalf("local claim named a machine: %s", w)
	}
}
