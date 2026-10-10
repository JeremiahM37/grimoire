package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/bank"
	"time"
)

func TestBankReasoningRoutesWithoutAModel(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if w := do(t, h, "POST", "/api/banks/b/memories", map[string]any{"items": []map[string]any{
		{"content": "Alice adopted a beagle named Biscuit. Alice works as a nurse.", "document_id": "d1", "tags": []string{"pets"}}}}); w.Code != 200 {
		t.Fatalf("retain = %d %s", w.Code, w.Body)
	}
	// Reflect with no model answers extractively.
	var rf map[string]any
	decode(t, do(t, h, "POST", "/api/banks/b/reflect", map[string]any{"query": "What dog did Alice adopt?",
		"include": map[string]any{"tool_calls": map[string]any{}}}), &rf)
	if rf["mode"] != "extractive" || !strings.Contains(rf["text"].(string), "beagle") || rf["trace"] == nil {
		t.Errorf("reflect = %v", rf)
	}
	decode(t, do(t, h, "POST", "/api/banks/b/reflect", map[string]any{"query": "What dog?"}), &rf)
	if rf["trace"] != nil || rf["based_on"] == nil {
		t.Errorf("trace must be opt-in, based_on always there: %v", rf)
	}
	// Model-only features say so.
	if w := do(t, h, "POST", "/api/banks/b/consolidate", nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "model_required") {
		t.Errorf("consolidate = %d %s", w.Code, w.Body)
	}
	// Recall options pass through.
	var rc map[string]any
	decode(t, do(t, h, "POST", "/api/banks/b/memories/recall", map[string]any{"query": "beagle",
		"tag_groups": []map[string]any{{"tags": []string{"pets"}}}, "temporal_window": map[string]any{"start": "2020-01-01", "end": "2030-01-01"},
		"min_scores": map[string]any{"final": 0.0}, "trace": true}), &rc)
	if len(rc["results"].([]any)) == 0 || rc["trace"].(map[string]any)["temporal_window"] == nil {
		t.Errorf("recall = %v", rc)
	}
	if w := do(t, h, "POST", "/api/banks/b/memories/recall", map[string]any{"query": "beagle",
		"tag_groups": []map[string]any{{"tags": []string{"x"}, "or": []any{}}}}); w.Code != 400 {
		t.Errorf("bad tag group = %d", w.Code)
	}

	// Directives.
	var d map[string]any
	if w := do(t, h, "POST", "/api/banks/b/directives", map[string]any{"name": "Kind", "content": "Be kind."}); w.Code != 201 {
		t.Fatalf("directive = %d %s", w.Code, w.Body)
	} else {
		decode(t, w, &d)
	}
	if w := do(t, h, "PATCH", "/api/banks/b/directives/"+d["id"].(string), map[string]any{"text": "Be very kind."}); w.Code != 200 {
		t.Errorf("patch directive = %d", w.Code)
	}
	var dl map[string]any
	decode(t, do(t, h, "GET", "/api/banks/b/directives", nil), &dl)
	if dl["total"] != 1.0 {
		t.Errorf("directives = %v", dl)
	}

	// Mental models, with a folder id that has to be URL-encoded.
	var mm map[string]any
	w := do(t, h, "POST", "/api/banks/b/mental-models", map[string]any{"id": "people/alice", "name": "Alice",
		"source_query": "Who is Alice?", "content": "Alice is a nurse with a beagle."})
	if w.Code != 201 {
		t.Fatalf("create model = %d %s", w.Code, w.Body)
	}
	decode(t, w, &mm)
	if mm["operation_id"] != nil || mm["mental_model"].(map[string]any)["authority"] != "human" {
		t.Errorf("create = %v", mm)
	}
	esc := "/api/banks/b/mental-models/" + url.PathEscape("people/alice")
	var got map[string]any
	decode(t, do(t, h, "GET", esc, nil), &got)
	if got["id"] != "people/alice" || got["body"] != "Alice is a nurse with a beagle." {
		t.Errorf("get model = %v", got)
	}
	if w := do(t, h, "POST", esc+"/refresh", nil); w.Code != http.StatusConflict {
		t.Errorf("refresh with no model = %d", w.Code)
	}
	var tree map[string]any
	decode(t, do(t, h, "GET", "/api/banks/b/mental-models-tree", nil), &tree)
	if roots := tree["roots"].([]any); len(roots) != 1 || roots[0].(map[string]any)["name"] != "people" {
		t.Errorf("tree = %v", tree)
	}
	if w := do(t, h, "GET", "/api/banks/b/mental-models-export?format=markdown", nil); !strings.Contains(w.Body.String(), "# Knowledge pages") {
		t.Errorf("export = %s", w.Body)
	}
	// Reflect now has a mental-model level to search first.
	decode(t, do(t, h, "POST", "/api/banks/b/reflect", map[string]any{"query": "Who is Alice?", "trace": true}), &rf)
	if lv := rf["trace"].(map[string]any)["levels"].([]any); lv[0] != "search_mental_models" {
		t.Errorf("levels = %v", lv)
	}
	if w := do(t, h, "GET", esc+"/history", nil); w.Code != 200 {
		t.Errorf("history = %d", w.Code)
	}
	if w := do(t, h, "DELETE", esc, nil); w.Code != 200 {
		t.Errorf("delete model = %d", w.Code)
	}

	// Observations list (empty), stats, templates.
	var obs map[string]any
	decode(t, do(t, h, "GET", "/api/banks/b/observations", nil), &obs)
	if obs["total"] != 0.0 {
		t.Errorf("observations = %v", obs)
	}
	var st map[string]any
	decode(t, do(t, h, "GET", "/api/banks/b/stats", nil), &st)
	if st["facts"] != 2.0 || st["model_available"] != false {
		t.Errorf("stats = %v", st)
	}
	var tl map[string]any
	decode(t, do(t, h, "GET", "/api/bank-templates", nil), &tl)
	if len(tl["templates"].([]any)) != 5 {
		t.Errorf("templates = %v", tl)
	}
	var imp map[string]any
	decode(t, do(t, h, "POST", "/api/banks/research-1/import?dry_run=1", map[string]any{"template": "research"}), &imp)
	if imp["dry_run"] != true || imp["bank_created"] != true {
		t.Errorf("dry import = %v", imp)
	}
	if w := do(t, h, "GET", "/api/banks/research-1", nil); w.Code != 404 {
		t.Errorf("a dry import created the bank: %d", w.Code)
	}
	decode(t, do(t, h, "POST", "/api/banks/research-1/import", map[string]any{"template": "research"}), &imp)
	var exp map[string]any
	decode(t, do(t, h, "GET", "/api/banks/research-1/export", nil), &exp)
	if len(exp["mental_models"].([]any)) != 2 {
		t.Errorf("export = %v", exp)
	}
	if w := do(t, h, "GET", "/api/banks/nosuch/operations", nil); w.Code != 404 || !strings.Contains(w.Body.String(), "detail") {
		t.Errorf("missing bank operations = %d %s", w.Code, w.Body)
	}
	_ = s
}

func TestAsyncRetainOverHTTPAndOperations(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if err := s.Banks.StartWorkers(1); err != nil {
		t.Fatal(err)
	}
	defer s.Banks.StopWorkers()
	var res map[string]any
	w := do(t, h, "POST", "/api/banks/b/memories", map[string]any{"async": true, "items": []map[string]any{
		{"content": "Bob plays the cello.", "document_id": "d1"}, {"content": "Bob lives in Oslo.", "document_id": "d2"}}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("async retain = %d %s", w.Code, w.Body)
	}
	decode(t, w, &res)
	opID := res["operation_id"].(string)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if op, err := s.Banks.WaitOperation(ctx, "b", opID); err != nil || op.Status != "completed" {
		t.Fatalf("op = %+v %v", op, err)
	}
	var list map[string]any
	decode(t, do(t, h, "GET", "/api/banks/b/operations?type=retain", nil), &list)
	if list["total"] != 1.0 || len(list["operations"].([]any)) != 1 {
		t.Errorf("operations = %v", list)
	}
	var op map[string]any
	decode(t, do(t, h, "GET", "/api/banks/b/operations/"+opID, nil), &op)
	if op["status"] != "completed" || op["result"] == nil {
		t.Errorf("operation = %v", op)
	}
	if w := do(t, h, "DELETE", "/api/banks/b/operations/"+opID, nil); w.Code != http.StatusConflict {
		t.Errorf("cancel finished = %d", w.Code)
	}
	// Webhooks: private targets need the operator's opt-in.
	if w := do(t, h, "POST", "/api/banks/b/webhooks", map[string]any{"url": "http://127.0.0.1:9/hook"}); w.Code != 400 {
		t.Errorf("loopback webhook = %d %s", w.Code, w.Body)
	}
	s.Settings.Update(map[string]string{"webhook_allow_private": "1"})
	var wh map[string]any
	w = do(t, h, "POST", "/api/banks/b/webhooks", map[string]any{"url": "http://127.0.0.1:9/hook", "event_types": []string{"retain.completed"}})
	if w.Code != 201 {
		t.Fatalf("webhook = %d %s", w.Code, w.Body)
	}
	decode(t, w, &wh)
	if !strings.HasPrefix(wh["secret"].(string), "whsec_") {
		t.Errorf("webhook = %v", wh)
	}
	if w := do(t, h, "PATCH", "/api/banks/b/webhooks/"+wh["id"].(string), map[string]any{"enabled": false}); w.Code != 200 {
		t.Errorf("patch webhook = %d", w.Code)
	}
	var hooks map[string]any
	decode(t, do(t, h, "GET", "/api/banks/b/webhooks", nil), &hooks)
	if item := hooks["items"].([]any)[0].(map[string]any); item["secret"] != nil || item["enabled"] != false {
		t.Errorf("hooks = %v", hooks)
	}
	if w := do(t, h, "GET", "/api/banks/b/webhooks/"+wh["id"].(string)+"/deliveries", nil); w.Code != 200 {
		t.Errorf("deliveries = %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/banks/b/webhooks/"+wh["id"].(string), nil); w.Code != 200 {
		t.Errorf("delete webhook = %d", w.Code)
	}
}

func TestPatchObservationMakesItAPersons(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if w := do(t, h, "POST", "/api/banks/b/memories", map[string]any{"items": []map[string]any{
		{"content": "Alice likes tea.", "document_id": "d1"}}}); w.Code != 200 {
		t.Fatalf("retain = %d %s", w.Code, w.Body)
	}
	body := bank.FormatObservations(bank.ObservationsFile{Current: []bank.Observation{{ID: "oapi1", Text: "Alice likes tea"}}})
	if _, err := s.Vault.Write(bank.ObservationsPath("b"), body, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Upsert(bank.ObservationsPath("b")); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	decode(t, do(t, h, "GET", "/api/banks/b/observations/oapi1", nil), &got)
	if got["observation"].(map[string]any)["authority"] != "agent" {
		t.Fatalf("setup: %v", got)
	}
	w := do(t, h, "PATCH", "/api/banks/b/observations/oapi1", map[string]any{"text": "Alice prefers green tea"})
	if w.Code != 200 {
		t.Fatalf("patch = %d %s", w.Code, w.Body)
	}
	decode(t, do(t, h, "GET", "/api/banks/b/observations/oapi1", nil), &got)
	o := got["observation"].(map[string]any)
	if o["text"] != "Alice prefers green tea" || o["authority"] != "human" || len(got["history"].([]any)) != 1 {
		t.Errorf("after patch = %v", got)
	}
	if w := do(t, h, "PATCH", "/api/banks/b/observations/oapi1", map[string]any{"text": " "}); w.Code != 400 {
		t.Errorf("empty = %d", w.Code)
	}
	if w := do(t, h, "PATCH", "/api/banks/b/observations/nope", map[string]any{"text": "x"}); w.Code != 404 {
		t.Errorf("unknown = %d", w.Code)
	}
}
