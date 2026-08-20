package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Metrics exist to answer questions during an incident, so the test asserts
// the questions can be answered — not that a library was called.

func TestMetricsAnswerTheQuestionsAnIncidentAsks(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{"path": "n.md", "body": "# N\n\nkestrel"})
	do(t, h, "GET", "/api/search?q=kestrel", nil)
	do(t, h, "GET", "/api/retrieve?q=kestrel&k=3", nil)
	do(t, h, "GET", "/api/notes/n.md", nil)

	w := do(t, h, "GET", "/metrics", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics = %d", w.Code)
	}
	body := w.Body.String()
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("content type = %q", ct)
	}
	for _, want := range []string{
		"grimoire_requests_total",         // is it serving, and what is failing
		"grimoire_request_seconds_bucket", // how slow
		"grimoire_retrievals_total",       // is retrieval being used
		"grimoire_retrieval_seconds",      // is retrieval slow
		"grimoire_cache_rebuilds_total",   // is the cache thrashing
		"grimoire_notes_current",          // how big is the corpus
		"grimoire_vault_unlocked_info",    // can the broker serve
		"grimoire_uptime_seconds",         // did it restart
		"# HELP", "# TYPE",                // scrapeable, and self-describing
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics do not report %s", want)
		}
	}
}

// A note path is user content. Emitting one as a label would put note titles in
// a monitoring system for the retention period, visible to anyone who can read
// a dashboard — and add one series per note while doing it.
func TestMetricsNeverLabelWithUserContent(t *testing.T) {
	_, h := testServer(t)
	secret := "quarterly-layoffs-plan"
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": secret + ".md", "body": "# Secret\n\nsensitive"})
	do(t, h, "GET", "/api/notes/"+secret+".md", nil)
	do(t, h, "GET", "/api/search?q="+secret, nil)

	body := do(t, h, "GET", "/metrics", nil).Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("a note path leaked into metrics:\n%s", body)
	}
	// The request was still counted, under a bounded class.
	if !strings.Contains(body, `route="notes:one"`) {
		t.Errorf("the note read was not counted under a route class:\n%s", body)
	}
}

// The cheap path has to be visibly cheap: a write patches the cache, and only
// the first query after a bulk operation rebuilds it. If rebuilds climb with
// write volume, in-place patching has broken.
func TestMetricsDistinguishCachePatchesFromRebuilds(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{"path": "a.md", "body": "# A\n\nalpha"})
	do(t, h, "GET", "/api/retrieve?q=alpha&k=3", nil) // builds the cache

	// Counters are process-global, so every other test in this package has
	// already moved them. Measuring the DELTA around the writes is the only
	// assertion that means anything here — the absolute value is a statement
	// about the test binary, not about the server. (Written the other way
	// first, it failed with "11 rebuilds for five writes" and the eleven
	// belonged to other tests.)
	before := do(t, h, "GET", "/metrics", nil).Body.String()
	patchesBefore := metricValue(t, before, "grimoire_cache_patches_total")
	rebuildsBefore := metricValue(t, before, "grimoire_cache_rebuilds_total")

	for i := 0; i < 5; i++ {
		do(t, h, "POST", "/api/notes", map[string]any{
			"path": "n" + string(rune('a'+i)) + ".md", "body": "# N\n\nalpha beta"})
	}
	do(t, h, "GET", "/api/retrieve?q=alpha&k=3", nil)

	after := do(t, h, "GET", "/metrics", nil).Body.String()
	patches := metricValue(t, after, "grimoire_cache_patches_total") - patchesBefore
	rebuilds := metricValue(t, after, "grimoire_cache_rebuilds_total") - rebuildsBefore
	if patches < 5 {
		t.Errorf("five writes produced %v cache patches", patches)
	}
	if rebuilds > 1 {
		t.Errorf("%v rebuilds for five writes — the cache is being thrown away", rebuilds)
	}
}

// A disabled metrics endpoint must be absent, not empty: an operator who turned
// it off should get a 404 rather than a page implying nothing is happening.
func TestMetricsCanBeTurnedOff(t *testing.T) {
	t.Setenv("GRIMOIRE_METRICS", "off")
	_, h := testServer(t)
	if w := do(t, h, "GET", "/metrics", nil); w.Code != http.StatusNotFound {
		t.Fatalf("/metrics with GRIMOIRE_METRICS=off = %d", w.Code)
	}
}

func metricValue(t *testing.T, body, name string) float64 {
	t.Helper()
	_ = t
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+" ") {
			var v float64
			if _, err := fmt.Sscan(line[len(name)+1:], &v); err == nil {
				return v
			}
		}
	}
	// A counter that has not been incremented yet has no series at all, which
	// for a delta measurement means zero rather than a failure.
	return 0
}
