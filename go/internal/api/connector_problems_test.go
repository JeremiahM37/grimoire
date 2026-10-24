package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/connectors"
)

func healthConnector(id, kind, name, secret string, enabled bool) connectors.Connector {
	return connectors.Connector{ID: id, Kind: kind, Name: name, Secret: secret,
		Enabled: enabled, Interval: 3600, Prefix: "connectors/" + kind,
		Created: "2026-08-19T00:00:00Z"}
}

func TestBuildConnectorStateClassifiesProblems(t *testing.T) {
	since := "2026-10-17T09:00:00Z"
	list := []connectors.Connector{
		healthConnector("g", "gmail", "Gmail", "google-oauth", true),
		healthConnector("d", "gdrive", "Drive", "google-oauth", true),
		healthConnector("c", "gcal", "Calendar", "google-oauth", true),
		healthConnector("x", "gmail", "Old", "google-oauth", false),
	}
	health := map[string]connectors.Health{
		"g": {State: connectors.StateNeedsReauth, Since: since, LastError: "invalid_grant: Token has been expired or revoked."},
		"d": {State: connectors.StateFailing, Failures: 3, Since: "2026-10-23T10:00:00Z", LastError: "timeout"},
		"x": {State: connectors.StateNeedsReauth, Since: since},
	}
	report, problems := buildConnectorState(list, health)

	if report.OK != 1 {
		t.Errorf("ok = %d, want 1 (Calendar); the disabled connector must not count", report.OK)
	}
	if len(report.NeedsReauth) != 1 || report.NeedsReauth[0].ID != "g" {
		t.Errorf("needs_reauth = %+v", report.NeedsReauth)
	}
	if len(report.Failing) != 1 || report.Failing[0].ID != "d" {
		t.Errorf("failing = %+v", report.Failing)
	}
	if len(problems) != 2 {
		t.Fatalf("problems = %d, want 2 (disabled connector excluded): %+v", len(problems), problems)
	}

	p := problems[0]
	if p.Source != "connector" || p.ID != "g" || p.Title != "Gmail (google-oauth)" {
		t.Errorf("problem header = %+v", p)
	}
	if !strings.Contains(p.Detail, "needs you to sign in again since 2026-10-17") {
		t.Errorf("detail = %q", p.Detail)
	}
	if !strings.Contains(p.Action, "Settings → Connectors") || !strings.Contains(p.Action, "In production") {
		t.Errorf("google action does not say how to stop the weekly expiry: %q", p.Action)
	}
	if p.Since.Format(time.RFC3339) != since {
		t.Errorf("since = %s", p.Since)
	}
	if !strings.Contains(problems[1].Detail, "failed 3 runs in a row") {
		t.Errorf("failing detail = %q", problems[1].Detail)
	}
}

func TestBuildConnectorStateEmptyIsNotNull(t *testing.T) {
	report, problems := buildConnectorState(nil, nil)
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"failing":[]`) || !strings.Contains(string(raw), `"needs_reauth":[]`) {
		t.Errorf("empty lists must render as [], got %s", raw)
	}
	if problems == nil || len(problems) != 0 {
		t.Errorf("problems = %#v, want an empty non-nil slice", problems)
	}
}

func TestHealthReportsConnectorProblemsWithoutFailingTheServer(t *testing.T) {
	s, h := testServer(t)
	s.Connectors = connectors.NewStore(s.Index.DB)
	g := healthConnector("g", "gmail", "Gmail", "google-oauth", true)
	if err := s.Connectors.Save(g); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	dead := &connectors.TokenError{Status: http.StatusBadRequest, Code: "invalid_grant",
		Description: "Token has been expired or revoked."}
	if err := s.Connectors.RecordRun("g", dead, now); err != nil {
		t.Fatal(err)
	}

	body := getHealth(t, h)
	if body["ok"] != true {
		t.Errorf("ok = %v; the server itself is fine", body["ok"])
	}
	if body["degraded"] != true {
		t.Errorf("degraded = %v, want true", body["degraded"])
	}
	problems, _ := body["problems"].([]any)
	want := "Gmail (google-oauth) needs you to sign in again since 2026-10-17"
	if len(problems) != 1 || problems[0] != want {
		t.Errorf("problems = %v, want [%q]", problems, want)
	}
	conns, _ := body["connectors"].(map[string]any)
	if conns == nil {
		t.Fatalf("connectors block missing: %v", body)
	}
	if conns["ok"] != float64(0) {
		t.Errorf("connectors.ok = %v", conns["ok"])
	}
	parked, _ := conns["needs_reauth"].([]any)
	if len(parked) != 1 {
		t.Fatalf("needs_reauth = %v", conns["needs_reauth"])
	}
	entry := parked[0].(map[string]any)
	if entry["id"] != "g" || entry["kind"] != "gmail" || entry["account"] != "Gmail" ||
		entry["since"] != "2026-10-17T09:00:00Z" {
		t.Errorf("entry = %v", entry)
	}
	if _, ok := conns["failing"].([]any); !ok {
		t.Errorf("failing must be a list, got %v", conns["failing"])
	}
}

func TestHealthHealthyConnectorsNotDegraded(t *testing.T) {
	s, h := testServer(t)
	s.Connectors = connectors.NewStore(s.Index.DB)
	if err := s.Connectors.Save(healthConnector("g", "gmail", "Gmail", "google-oauth", true)); err != nil {
		t.Fatal(err)
	}
	body := getHealth(t, h)
	if body["degraded"] != false {
		t.Errorf("degraded = %v on a healthy server", body["degraded"])
	}
	if problems, _ := body["problems"].([]any); len(problems) != 0 {
		t.Errorf("problems = %v", problems)
	}
	conns, _ := body["connectors"].(map[string]any)
	if conns["ok"] != float64(1) {
		t.Errorf("connectors.ok = %v, want 1", conns["ok"])
	}
}

func TestConnectorProblemsMethodReadsStore(t *testing.T) {
	s, _ := testServer(t)
	if got := s.ConnectorProblems(); len(got) != 0 {
		t.Fatalf("no connectors configured, got problems %v", got)
	}
	s.Connectors = connectors.NewStore(s.Index.DB)
	if err := s.Connectors.Save(healthConnector("g", "gmail", "Gmail", "google-oauth", true)); err != nil {
		t.Fatal(err)
	}
	if err := s.Connectors.RecordRun("g", &connectors.TokenError{Status: 401}, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := s.ConnectorProblems()
	if len(got) != 1 || got[0].ID != "g" || got[0].Action == "" {
		t.Fatalf("ConnectorProblems = %+v", got)
	}
}

func getHealth(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/health = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}
