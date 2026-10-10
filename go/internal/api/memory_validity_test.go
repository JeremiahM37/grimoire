package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// setClock moves the belief clock for one test. Stamps and superseded_at are
// minted from it, so as_of can be exercised over real time.
func setClock(t *testing.T, when string) {
	t.Helper()
	tm, err := time.ParseInLocation("2006-01-02 15:04", when, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	old := vault.Now
	vault.Now = func() time.Time { return tm }
	t.Cleanup(func() { vault.Now = old })
}

func TestRememberStoresValidityAndRecallReturnsIt(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "office", "text": "the office is on floor three",
		"valid_from": "2026-03-01", "valid_to": "2026-09-01T00:00:00+02:00"})

	facts := recallFacts(t, h, "")
	if len(facts) != 1 {
		t.Fatalf("recalled %d facts, want 1", len(facts))
	}
	if facts[0]["valid_from"] != "2026-03-01T00:00:00Z" || facts[0]["valid_to"] != "2026-08-31T22:00:00Z" {
		t.Errorf("validity = %v / %v, want canonical UTC", facts[0]["valid_from"], facts[0]["valid_to"])
	}
	if !strings.Contains(noteBody(t, h, "memory/office.md"), "valid_from=2026-03-01T00:00:00Z") {
		t.Error("validity was not written to the bullet")
	}
}

func TestUndatedFactsCarryNoValidityFields(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "misc", "text": "the deploy host is ember"})
	facts := recallFacts(t, h, "")
	if _, ok := facts[0]["valid_from"]; ok {
		t.Errorf("an undated fact grew valid_from: %v", facts[0])
	}
	if strings.Contains(noteBody(t, h, "memory/misc.md"), "valid_") {
		t.Error("an undated fact wrote validity into its bullet")
	}
}

func TestRememberRefusesMalformedOrInvertedValidity(t *testing.T) {
	_, h := testServer(t)
	cases := map[string]map[string]any{
		"not a time":     {"text": "x fact", "valid_from": "last tuesday"},
		"inverted range": {"text": "y fact", "valid_from": "2026-05-01", "valid_to": "2026-04-01"},
	}
	for name, body := range cases {
		body["topic"] = "bad"
		if w := do(t, h, "POST", "/api/memory", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, w.Code)
		}
	}
}

func TestSupersedingWithValidFromClosesTheOldFactsValidity(t *testing.T) {
	_, h := testServer(t)
	setClock(t, "2026-08-10 09:00")
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers spaces",
		"valid_from": "2026-01-01"})

	setClock(t, "2026-08-15 09:00")
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs",
		"valid_from": "2026-05-01"})

	body := noteBody(t, h, "memory/prefs.md")
	if !strings.Contains(body, "valid_to=2026-05-01T00:00:00Z") {
		t.Errorf("the replaced fact's validity was not closed:\n%s", body)
	}
	if !strings.Contains(body, "valid_from=2026-05-01T00:00:00Z") {
		t.Errorf("the replacement lost its own validity:\n%s", body)
	}
}

func TestSupersedingWithoutValidFromLeavesValidityAlone(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers spaces"})
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs"})
	if strings.Contains(noteBody(t, h, "memory/prefs.md"), "valid_") {
		t.Error("a write with no valid_from changed another fact's validity")
	}
}

func TestSupersedingNeverOverwritesAHumanSetValidTo(t *testing.T) {
	_, h := testServer(t)
	setClock(t, "2026-08-10 09:00")
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers spaces",
		"valid_to": "2026-02-01"})
	setClock(t, "2026-08-15 09:00")
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs",
		"valid_from": "2026-05-01"})
	body := noteBody(t, h, "memory/prefs.md")
	if !strings.Contains(body, "valid_to=2026-02-01T00:00:00Z") || strings.Contains(body, "valid_to=2026-05-01") {
		t.Errorf("a set valid_to was overwritten:\n%s", body)
	}
}

func TestValidAtAndRangeFiltersOverHTTP(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "office", "text": "the office is on floor three",
		"valid_from": "2026-03-01", "valid_to": "2026-09-01"})
	remember(t, h, map[string]any{"topic": "office", "text": "the company name is Acme"})

	has := func(query, text string) bool {
		for _, f := range recallFacts(t, h, query) {
			if f["text"] == text {
				return true
			}
		}
		return false
	}
	const floor = "the office is on floor three"
	if has("?valid_at="+url.QueryEscape("2026-02-01T00:00:00Z"), floor) {
		t.Error("valid_at before valid_from returned the fact")
	}
	if !has("?valid_at="+url.QueryEscape("2026-06-01T00:00:00Z"), floor) {
		t.Error("valid_at inside the window did not return the fact")
	}
	if has("?valid_at=2026-09-01", floor) {
		t.Error("valid_at at valid_to returned the fact (valid_to is exclusive)")
	}
	if !has("?valid_at=2026-06-01", "the company name is Acme") {
		t.Error("an undated fact was not valid at a date")
	}
	if !has("?valid_since=2026-08-01&valid_until=2026-08-02", floor) {
		t.Error("an overlapping range did not return the fact")
	}
	if has("?valid_since=2026-10-01&valid_until=2026-10-02", floor) {
		t.Error("a range after valid_to returned the fact")
	}
}

func TestValidityQueriesRejectMalformedInput(t *testing.T) {
	_, h := testServer(t)
	for _, q := range []string{
		"?valid_at=last+tuesday",
		"?valid_since=nope",
		"?valid_since=2026-06-01&valid_until=2026-05-01",
	} {
		if w := do(t, h, "GET", "/api/memory"+q, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", q, w.Code)
		}
	}
}

func TestAsOfCombinedWithValidAtAnswersTheBitemporalQuestion(t *testing.T) {
	// On Aug 12 we recorded that the office was on floor two from January.
	// On Aug 15 we learned that floor three had been true from March, and
	// closed the earlier belief. Asking what we believed on Aug 12 about March
	// has to give the old belief; asking now gives the replacement.
	_, h := testServer(t)
	setClock(t, "2026-08-12 09:00")
	remember(t, h, map[string]any{"topic": "office", "text": "the user prefers spaces",
		"valid_from": "2026-01-01"})

	setClock(t, "2026-08-15 09:00")
	remember(t, h, map[string]any{"topic": "office", "text": "the user prefers tabs",
		"valid_from": "2026-03-01"})

	march := "2026-03-15T00:00:00Z"
	believedThen := recallFacts(t, h, "?as_of="+url.QueryEscape("2026-08-13T09:00:00Z")+
		"&valid_at="+url.QueryEscape(march))
	if got := texts(believedThen); len(got) != 1 || got[0] != "the user prefers spaces" {
		t.Errorf("believed on Aug 13 about March = %q, want the old belief", got)
	}
	believedNow := recallFacts(t, h, "?valid_at="+url.QueryEscape(march))
	if got := texts(believedNow); len(got) != 1 || got[0] != "the user prefers tabs" {
		t.Errorf("current belief about March = %q, want the replacement", got)
	}
}

func TestAsOfBehaviourIsUnchangedWithoutValidity(t *testing.T) {
	// Adding validity must not move the belief-time query for facts that have none.
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers spaces"})
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs"})
	if facts := recallFacts(t, h, "?as_of=2026-08-13T09:00:00Z"); len(facts) != 0 {
		t.Errorf("facts existed before they were written: %q", texts(facts))
	}
}
