package api

import (
	"net/http"
	"strings"
	"testing"
)

// The three behaviours below are the ones a naive port drops, because each only
// shows up under a specific shape of query. Omitting the OR fallback and
// full=true cost ~6 points of benchmark recall before these tests existed —
// every individual endpoint still "worked", the answers just got half a context.

func TestSearchFallsBackToAnyTermForQuestions(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "trip.md",
		"body": "# Trip\n\nWe flew to Lisbon in March and the weather was warm.\n"})

	// no note contains every word — an AND-only query answers nothing here,
	// which is exactly the case an agent asks about
	q := "/api/search?q=what+was+the+weather+like+in+Lisbon"
	var hits []map[string]any
	decode(t, do(t, h, "GET", q, nil), &hits)
	if len(hits) == 0 {
		t.Fatal("a question-shaped query returned nothing")
	}
	if hits[0]["path"] != "trip.md" {
		t.Errorf("best hit = %v, want trip.md", hits[0]["path"])
	}
}

func TestSearchFullReturnsBodiesAndExcerptsLongOnes(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "short.md", "body": "# Short\n\nthe gateway is fine\n"})

	// long enough to be excerpted, with the answer buried past the head so a
	// head-only excerpt would miss it
	filler := strings.Repeat("Unrelated background prose about the office. ", 120)
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "long.md",
		"body": "# Long\n\n" + filler + "\n\nThe gateway listens on port 8443.\n" + filler})

	var hits []map[string]any
	decode(t, do(t, h, "GET", "/api/search?q=gateway&full=true", nil), &hits)
	if len(hits) < 2 {
		t.Fatalf("want both notes, got %d", len(hits))
	}
	byPath := map[string]map[string]any{}
	for _, hit := range hits {
		byPath[hit["path"].(string)] = hit
	}

	short := byPath["short.md"]
	if !strings.Contains(short["body"].(string), "the gateway is fine") {
		t.Errorf("short body = %q, want it whole", short["body"])
	}
	if short["excerpted"] == true {
		t.Error("a short note should not be excerpted")
	}

	long := byPath["long.md"]
	if long["excerpted"] != true {
		t.Error("a long note should be excerpted")
	}
	body := long["body"].(string)
	if !strings.Contains(body, "port 8443") {
		t.Error("the excerpt dropped the only query-relevant passage")
	}
	if len(body) >= len(filler)*2 {
		t.Errorf("excerpt is %d chars — it returned the whole note", len(body))
	}
}

func TestSearchWithoutFullOmitsBodies(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "gw.md", "body": "# Gateway\n\nthe gateway listens on 8443\n"})

	var hits []map[string]any
	decode(t, do(t, h, "GET", "/api/search?q=gateway", nil), &hits)
	if len(hits) != 1 {
		t.Fatalf("hits = %d", len(hits))
	}
	if _, ok := hits[0]["body"]; ok {
		t.Error("body present without full=true — callers pay for it on every search")
	}
	if hits[0]["snippet"] == "" {
		t.Error("no snippet")
	}
}

func TestSearchLimitCapsResults(t *testing.T) {
	_, h := testServer(t)
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		do(t, h, "POST", "/api/notes", map[string]any{
			"path": p, "body": "# " + p + "\n\ngateway\n"})
	}
	var hits []map[string]any
	decode(t, do(t, h, "GET", "/api/search?q=gateway&limit=2", nil), &hits)
	if len(hits) != 2 {
		t.Errorf("limit=2 returned %d hits", len(hits))
	}
}

func TestSearchOperators(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "infra/gw.md", "body": "# Gateway\n\ngateway notes #infra\n"})
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "docs/gw2.md", "body": "# Gateway Two\n\ngateway notes #docs\n"})
	do(t, h, "POST", "/api/notes/infra/gw.md/pin", nil)

	for _, c := range []struct {
		q    string
		want string
	}{
		{"gateway tag:infra", "infra/gw.md"},
		{"gateway path:docs", "docs/gw2.md"},
		{"gateway is:pinned", "infra/gw.md"},
		{"tag:docs", "docs/gw2.md"}, // operator alone, no free text
		{"is:pinned", "infra/gw.md"},
	} {
		var hits []map[string]any
		decode(t, do(t, h, "GET", "/api/search?q="+strings.ReplaceAll(c.q, " ", "+"), nil), &hits)
		if len(hits) != 1 {
			t.Errorf("%q returned %d hits, want 1: %v", c.q, len(hits), hits)
			continue
		}
		if hits[0]["path"] != c.want {
			t.Errorf("%q = %v, want %v", c.q, hits[0]["path"], c.want)
		}
	}
}

func TestSearchKeepsEncryptedBodiesSealed(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "secret.md", "body": "# Secret\n\nthe gateway passphrase is hunter2\n"})
	if w := do(t, h, "POST", "/api/vault/init",
		map[string]any{"passphrase": "correct horse battery"}); w.Code != http.StatusOK {
		t.Fatalf("vault init = %d: %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/notes/secret.md/encrypt", nil); w.Code != http.StatusOK {
		t.Fatalf("encrypt = %d: %s", w.Code, w.Body)
	}

	// searched by TITLE, which stays in the clear — so the note is genuinely
	// found and this asserts a blanked body rather than passing vacuously on
	// zero hits
	var hits []map[string]any
	decode(t, do(t, h, "GET", "/api/search?q=Secret&full=true", nil), &hits)
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want the encrypted note found by title", len(hits))
	}
	if body := hits[0]["body"]; body != nil && body != "" {
		t.Errorf("body = %q, want it blank rather than ciphertext", body)
	}
	if strings.Contains(hits[0]["snippet"].(string), "hunter2") {
		t.Error("snippet leaked plaintext from before encryption")
	}
}
