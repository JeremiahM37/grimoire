package api

import (
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
)

// The events flag: history stays out of automatic injection, and only then.

const historyText = "the proxy moved to mediaserver"

var historyWhen = time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)

// historyServer stores one fact, as history or as a current claim, and returns
// the injection response for a query that matches it.
func historyServer(t *testing.T, asEvent bool) (string, string) {
	t.Helper()
	_, h := testServer(t)
	at(t, historyWhen, func() {
		rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops", "text": historyText,
			"event": asEvent, "infer": false})
	})
	ctx := do(t, h, "GET", "/api/memory/context?q=proxy+mediaserver", nil).Body.String()
	rec := recallJSON(t, h, "proxy+mediaserver")
	return ctx, rec
}

// With the flag off, marking a fact as history changes nothing about what is
// injected.
func TestEventMarkingChangesNothingWhenTheFlagIsOff(t *testing.T) {
	plain, _ := historyServer(t, false)
	marked, _ := historyServer(t, true)
	if plain != marked {
		t.Fatalf("flag off: injection differs when the fact is an event\nplain:  %s\nmarked: %s", plain, marked)
	}
	if !strings.Contains(plain, "mediaserver") {
		t.Fatalf("the fact was not injected at all: %s", plain)
	}
}

func TestEventsAreLeftOutOfInjectionWhenTheFlagIsOn(t *testing.T) {
	restore := exp.Set(eventsFlag)
	defer restore()
	ctx, _ := historyServer(t, true)
	if strings.Contains(ctx, "mediaserver") {
		t.Fatalf("flag on: an event was injected: %s", ctx)
	}
	// A claim that is not history still injects under the same flag.
	ctx, _ = historyServer(t, false)
	if !strings.Contains(ctx, "mediaserver") {
		t.Fatalf("flag on: a non-event fact stopped injecting: %s", ctx)
	}
}

// Explicit recall still returns the event, and says so.
func TestExplicitRecallKeepsEventsAndSaysSo(t *testing.T) {
	restore := exp.Set(eventsFlag)
	defer restore()
	_, rec := historyServer(t, true)
	if !strings.Contains(rec, historyText) || !strings.Contains(rec, `"event":true`) {
		t.Fatalf("explicit recall of an event: %s", rec)
	}
}

// A fact that is not history carries no event key, so explicit recall output
// is unchanged for everything that was never marked.
func TestNonEventRecallHasNoEventKey(t *testing.T) {
	_, rec := historyServer(t, false)
	if strings.Contains(rec, `"event"`) {
		t.Fatalf("a non-event fact gained an event key: %s", rec)
	}
}
