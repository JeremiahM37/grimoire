package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/connectors"
)

type actStub struct{ ran *int }

func (actStub) Kind() string { return "actstub" }
func (actStub) Describe() connectors.Kind {
	return connectors.Kind{Kind: "actstub", Name: "Act", Help: "t", DefaultPrefix: "connectors/act"}
}
func (actStub) Fetch(context.Context, connectors.Input) (connectors.Page, error) {
	return connectors.Page{}, nil
}
func (actStub) Actions() []connectors.ActionSpec {
	return []connectors.ActionSpec{{Name: "do", Summary: "do it",
		Params: []connectors.Field{{Name: "what", Label: "What", Required: true}}}}
}
func (a actStub) Act(_ context.Context, _ connectors.Input, _ string, p map[string]string) (connectors.ActionResult, error) {
	*a.ran++
	return connectors.ActionResult{ID: "r1", Message: "did " + p["what"]}, nil
}

func TestSourceActionsGoThroughTheQueueOverHTTP(t *testing.T) {
	ran := 0
	connectors.Register(actStub{ran: &ran})
	_, h := connectorServer(t, nil)

	var c map[string]any
	decode(t, do(t, h, "POST", "/api/connectors", map[string]any{
		"kind": "actstub", "config": map[string]string{"actions": "do"}}), &c)
	id := c["id"].(string)

	var list []map[string]any
	decode(t, do(t, h, "GET", "/api/sources", nil), &list)
	if len(list) != 1 || strings.Contains(do(t, h, "GET", "/api/sources", nil).Body.String(), "secret") {
		t.Fatalf("sources = %v", list)
	}

	var act map[string]any
	decode(t, do(t, h, "POST", "/api/sources/"+id+"/act", map[string]any{
		"action": "do", "params": map[string]any{"what": "x"}}), &act)
	if act["state"] != "pending" || ran != 0 {
		t.Fatalf("act = %v ran=%d", act, ran)
	}
	if _, leaked := act["params"]; leaked {
		t.Error("agent view echoes parameters")
	}
	// disabled class
	w := do(t, h, "POST", "/api/sources/"+id+"/act", map[string]any{"action": "nope", "params": map[string]any{}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown action = %d", w.Code)
	}

	aid := act["id"].(string)
	var pending []map[string]any
	decode(t, do(t, h, "GET", "/api/source-actions?state=pending", nil), &pending)
	if len(pending) != 1 || pending[0]["params"].(map[string]any)["what"] != "x" {
		t.Fatalf("pending = %v", pending)
	}
	var done map[string]any
	decode(t, do(t, h, "POST", "/api/source-actions/"+aid+"/approve", nil), &done)
	if done["state"] != "executed" || ran != 1 {
		t.Fatalf("done = %v ran=%d", done, ran)
	}
	var st map[string]any
	decode(t, do(t, h, "GET", "/api/sources/actions/"+aid, nil), &st)
	if st["state"] != "executed" {
		t.Errorf("status = %v", st)
	}
	if do(t, h, "POST", "/api/source-actions/"+aid+"/approve", nil).Code != http.StatusNotFound {
		t.Error("double approval accepted")
	}
}

// Own-trust documents become trusted notes in the index; the rest stay untrusted.
func TestOwnTrustReachesTheIndex(t *testing.T) {
	s, h := connectorServer(t, []connectors.Document{
		{ExternalID: "1", Title: "Mine", Body: "alpha words", Own: true},
		{ExternalID: "2", Title: "Theirs", Body: "beta words"},
	})
	for _, class := range []string{"own", "external"} {
		var c map[string]any
		decode(t, do(t, h, "POST", "/api/connectors", map[string]any{
			"kind": "echo", "name": class, "prefix": "connectors/" + class,
			"config": map[string]string{"topic": "t", "trust": class}}), &c)
		do(t, h, "POST", "/api/connectors/"+c["id"].(string)+"/run", nil)
		for title, want := range map[string]int{"mine": 0, "theirs": 1} {
			if class == "external" {
				want = 1
			}
			var got int
			if err := s.Index.DB.QueryRow("SELECT untrusted FROM notes WHERE path LIKE ?",
				"connectors/"+class+"/"+title+"-%").Scan(&got); err != nil {
				t.Fatalf("%s/%s: %v", class, title, err)
			}
			if got != want {
				t.Errorf("%s/%s untrusted=%d want %d", class, title, got, want)
			}
		}
	}
}
