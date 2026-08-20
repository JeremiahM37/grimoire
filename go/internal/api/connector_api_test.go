package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/connectors"
)

// A connector, configured over the API, ending as searchable notes.

// echoSource is a source that returns whatever the test put in it, so the API
// path can be exercised without a network.
type echoSource struct{ docs []connectors.Document }

func (echoSource) Kind() string { return "echo" }
func (echoSource) Describe() connectors.Kind {
	return connectors.Kind{Kind: "echo", Name: "Echo", Help: "test source",
		DefaultPrefix: "connectors/echo",
		Fields:        []connectors.Field{{Name: "topic", Label: "Topic", Required: true}}}
}
func (e echoSource) Fetch(context.Context, connectors.Input) (connectors.Page, error) {
	return connectors.Page{Docs: e.docs, Cursor: "done"}, nil
}

func connectorServer(t *testing.T, docs []connectors.Document) (*Server, http.Handler) {
	t.Helper()
	connectors.Register(echoSource{docs: docs})
	s, h := testServer(t)
	s.Connectors = connectors.NewStore(s.Index.DB)
	s.Runner = &connectors.Runner{Store: s.Connectors, Writer: s,
		Secrets: SecretsForConnectors{Server: s}}
	return s, h
}

func TestConnectorKindsDescribeThemselvesToTheConsole(t *testing.T) {
	_, h := connectorServer(t, nil)
	var kinds []map[string]any
	decode(t, do(t, h, "GET", "/api/connectors/kinds", nil), &kinds)

	byKind := map[string]map[string]any{}
	for _, k := range kinds {
		byKind[k["kind"].(string)] = k
	}
	// The console renders a form from this, so every shipped source has to be
	// describable without the console knowing anything about it.
	for _, want := range []string{"slack", "confluence", "jira", "gdrive", "github", "rss"} {
		k, ok := byKind[want]
		if !ok {
			t.Errorf("%s is not offered", want)
			continue
		}
		if k["help"] == "" || k["default_prefix"] == "" {
			t.Errorf("%s: %v", want, k)
		}
	}
	if byKind["slack"]["secret_help"] == "" {
		t.Error("slack does not say what credential it needs or where to get one")
	}
}

func TestConfiguringAConnectorAndRunningItProducesSearchableNotes(t *testing.T) {
	_, h := connectorServer(t, []connectors.Document{{
		ExternalID: "42", Title: "Rollback runbook",
		Body:    "If the proxy still 502s, force-recreate the dependents.",
		URL:     "https://example.test/42",
		Updated: "2026-08-19T10:00:00Z",
		Meta:    map[string]string{"channel": "C1"},
	}})

	w := do(t, h, "POST", "/api/connectors", map[string]any{
		"kind": "echo", "name": "Team wiki", "config": map[string]string{"topic": "ops"},
		"interval": 0})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var c map[string]any
	decode(t, w, &c)
	if c["prefix"] != "connectors/echo" {
		t.Fatalf("prefix = %v, want the kind's default", c["prefix"])
	}

	w = do(t, h, "POST", "/api/connectors/"+c["id"].(string)+"/run", nil)
	var run map[string]any
	decode(t, w, &run)
	if run["ok"] != true || run["written"].(float64) != 1 {
		t.Fatalf("run = %v", run)
	}

	// It is an ordinary note now: findable, readable, with provenance.
	body := do(t, h, "GET", "/api/search?q=force-recreate", nil).Body.String()
	if !strings.Contains(body, "connectors/echo") {
		t.Fatalf("pulled document is not searchable: %s", body)
	}
	var note map[string]any
	decode(t, do(t, h, "GET", "/api/notes/connectors/echo/rollback-runbook-42.md", nil), &note)
	fm, _ := note["frontmatter"].(map[string]any)
	if fm["source"] != "echo" || fm["external_id"] != "42" ||
		fm["url"] != "https://example.test/42" || fm["channel"] != "C1" {
		t.Fatalf("provenance missing from frontmatter: %v", fm)
	}

	// Running again changes nothing: the document has not changed.
	w = do(t, h, "POST", "/api/connectors/"+c["id"].(string)+"/run", nil)
	decode(t, w, &run)
	if run["written"].(float64) != 0 || run["skipped"].(float64) != 1 {
		t.Fatalf("second run = %v, want it to skip the unchanged document", run)
	}
}

func TestConnectorConfigurationIsValidatedOnSave(t *testing.T) {
	_, h := connectorServer(t, nil)
	w := do(t, h, "POST", "/api/connectors", map[string]any{"kind": "echo", "config": map[string]string{}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Topic") {
		t.Fatalf("missing required field accepted: %d %s", w.Code, w.Body)
	}
	w = do(t, h, "POST", "/api/connectors", map[string]any{"kind": "nope"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown kind accepted: %d", w.Code)
	}
	// A destination outside the vault is refused when it is configured, not
	// when the first sync tries to write there.
	w = do(t, h, "POST", "/api/connectors", map[string]any{
		"kind": "echo", "config": map[string]string{"topic": "x"}, "prefix": "../../etc"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("escaping prefix accepted: %d %s", w.Code, w.Body)
	}
}

func TestDeletingAConnectorKeepsItsNotesUnlessPurgeIsAsked(t *testing.T) {
	_, h := connectorServer(t, []connectors.Document{{
		ExternalID: "1", Title: "Kept", Body: "still here"}})
	var c map[string]any
	decode(t, do(t, h, "POST", "/api/connectors", map[string]any{
		"kind": "echo", "config": map[string]string{"topic": "x"}}), &c)
	do(t, h, "POST", "/api/connectors/"+c["id"].(string)+"/run", nil)
	path := "connectors/echo/kept-1.md"
	if do(t, h, "GET", "/api/notes/"+path, nil).Code != http.StatusOK {
		t.Fatal("the connector wrote nothing")
	}

	if w := do(t, h, "DELETE", "/api/connectors/"+c["id"].(string), nil); w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	if do(t, h, "GET", "/api/notes/"+path, nil).Code != http.StatusOK {
		t.Fatal("deleting a connector deleted notes somebody may have linked to")
	}

	// With purge, they go.
	decode(t, do(t, h, "POST", "/api/connectors", map[string]any{
		"kind": "echo", "config": map[string]string{"topic": "x"}}), &c)
	do(t, h, "POST", "/api/connectors/"+c["id"].(string)+"/run", nil)
	do(t, h, "DELETE", "/api/connectors/"+c["id"].(string)+"?purge=1", nil)
	if do(t, h, "GET", "/api/notes/"+path, nil).Code == http.StatusOK {
		t.Fatal("purge left the notes behind")
	}
}

func TestConnectorSecretsAreNeverReturned(t *testing.T) {
	s, h := connectorServer(t, nil)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if err := s.Secrets.Put("slack-token", "xoxb-super-secret", nil); err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	decode(t, do(t, h, "POST", "/api/connectors", map[string]any{
		"kind": "echo", "config": map[string]string{"topic": "x"},
		"secret": "slack-token"}), &c)

	for _, path := range []string{"/api/connectors", "/api/connectors/kinds"} {
		body := do(t, h, "GET", path, nil).Body.String()
		if strings.Contains(body, "xoxb-super-secret") {
			t.Fatalf("%s returned a credential value: %s", path, body)
		}
	}
	// The NAME is returned — an operator has to see which credential is used —
	// and only the name.
	var list []map[string]any
	decode(t, do(t, h, "GET", "/api/connectors", nil), &list)
	if list[0]["secret"] != "slack-token" {
		t.Fatalf("connector does not record which credential it uses: %v", list[0])
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "xoxb") {
		t.Fatal("a credential value leaked through the list")
	}
}
