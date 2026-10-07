package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// stubExtractor is an OpenAI-compatible endpoint that answers every
// extraction with the same canned facts.
func stubExtractor(t *testing.T, s *Server, facts ...map[string]any) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	content, _ := json.Marshal(map[string]any{"facts": facts})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": string(content)}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 50, "completion_tokens": 10},
		})
	}))
	t.Cleanup(srv.Close)
	if err := s.Settings.Update(map[string]string{"llm": "openai", "llm_base_url": srv.URL, "llm_model": "stub"}); err != nil {
		t.Fatal(err)
	}
	return &calls
}

var cannedFacts = []map[string]any{
	{"what": "Maya started a pottery class", "when": "May 2023", "who": "Maya (the user's sister)", "why": "N/A",
		"fact_kind": "event", "occurred_start": "2023-05-02", "occurred_end": "2023-05-02", "fact_type": "world",
		"entities": []any{"Maya", "pottery"}},
	{"what": "Maya prefers green tea over coffee", "fact_kind": "conversation", "fact_type": "world",
		"entities": []any{"Maya", "green tea"}},
}

func TestBankLifecycleOverHTTP(t *testing.T) {
	s, h := testServer(t)
	calls := stubExtractor(t, s, cannedFacts...)

	if w := do(t, h, "POST", "/api/banks", map[string]any{"bank_id": "Bad Id"}); w.Code != http.StatusBadRequest {
		t.Errorf("invalid id = %d", w.Code)
	}
	w := do(t, h, "POST", "/api/banks", map[string]any{"bank_id": "family", "name": "Family",
		"mission": "Remember family news.", "retain_mission": "Only family events and preferences.",
		"disposition": map[string]int{"skepticism": 4, "literalism": 3, "empathy": 5},
		"directives":  []map[string]any{{"text": "Never share addresses."}},
		"config":      map[string]string{"retain_chunk_size": "2000"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/banks", map[string]any{"bank_id": "family"}); w.Code != http.StatusConflict {
		t.Errorf("duplicate = %d", w.Code)
	}
	if w := do(t, h, "PATCH", "/api/banks/family", map[string]any{"config": map[string]string{"nope": "1"}}); w.Code != http.StatusBadRequest {
		t.Errorf("unknown setting = %d", w.Code)
	}
	w = do(t, h, "PATCH", "/api/banks/family", map[string]any{"mission": "Family news and plans."})
	var prof map[string]any
	decode(t, w, &prof)
	if prof["mission"] != "Family news and plans." || prof["retain_mission"] != "Only family events and preferences." ||
		prof["config"].(map[string]any)["retain_chunk_size"] != "2000" {
		t.Errorf("patch = %v", prof)
	}
	if d := prof["disposition"].(map[string]any); d["empathy"] != float64(5) {
		t.Errorf("disposition = %v", d)
	}
	// The profile is a readable file.
	raw, err := os.ReadFile(filepath.Join(s.Vault.Root, "banks/family/bank.md"))
	if err != nil || !strings.Contains(string(raw), "## Directives\n\n- Never share addresses. <!--d id=") {
		t.Errorf("bank.md:\n%s", raw)
	}

	// Retain a conversation.
	w = do(t, h, "POST", "/api/banks/family/memories", map[string]any{"items": []map[string]any{{
		"content": []map[string]any{
			{"speaker": "user", "text": "My sister Maya just started pottery!"},
			{"speaker": "assistant", "text": "That sounds fun."},
			{"speaker": "user", "text": "She also only drinks green tea now."}},
		"timestamp": "2023-05-08T13:56:00Z", "document_id": "session-1", "context": "chat",
		"tags": `["family"]`, "metadata": map[string]any{"source": "app"}}}})
	if w.Code != http.StatusOK {
		t.Fatalf("retain = %d %s", w.Code, w.Body)
	}
	var rr map[string]any
	decode(t, w, &rr)
	if rr["success"] != true || rr["mode"] != "concise" || calls.Load() != 1 {
		t.Errorf("retain = %v (calls %d)", rr, calls.Load())
	}
	if u := rr["usage"].(map[string]any); u["total_tokens"] != float64(60) {
		t.Errorf("usage = %v", u)
	}
	if w := do(t, h, "POST", "/api/banks/family/memories", map[string]any{"items": []map[string]any{{"content": "x"}}, "async": true}); w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "operation_id") {
		t.Errorf("async = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/banks/family/memories", map[string]any{"items": []map[string]any{{"content": "x", "timestamp": "yesterday-ish"}}}); w.Code != http.StatusBadRequest {
		t.Errorf("bad timestamp = %d", w.Code)
	}

	// Recall.
	w = do(t, h, "POST", "/api/banks/family/memories/recall", map[string]any{
		"query": "what does Maya drink", "trace": true, "include": map[string]any{"chunks": map[string]any{"max_tokens": 500}},
		"query_timestamp": "2023-06-01T00:00:00Z"})
	if w.Code != http.StatusOK {
		t.Fatalf("recall = %d %s", w.Code, w.Body)
	}
	var rec struct {
		Results []struct {
			ID, Text, Type, Authority string
			DocumentID                string `json:"document_id"`
			ChunkID                   string `json:"chunk_id"`
			Tags                      []string
			Metadata                  map[string]string
			Scores                    map[string]any
		}
		Entities map[string]any
		Chunks   map[string]map[string]any
		Trace    map[string]any
	}
	decode(t, w, &rec)
	if len(rec.Results) != 2 || !strings.Contains(rec.Results[0].Text, "green tea") {
		t.Fatalf("recall = %+v", rec.Results)
	}
	top := rec.Results[0]
	if top.DocumentID != "session-1" || top.Tags[0] != "family" || top.Metadata["source"] != "app" ||
		top.Authority != "agent" || top.ChunkID == "" || top.Scores["final"] == nil {
		t.Errorf("result = %+v", top)
	}
	if _, ok := rec.Entities["Maya"]; !ok || len(rec.Chunks) != 1 || rec.Trace["arms"] == nil {
		t.Errorf("entities=%v chunks=%d trace=%v", rec.Entities, len(rec.Chunks), rec.Trace != nil)
	}
	w = do(t, h, "POST", "/api/banks/family/memories/recall", map[string]any{"query": "Maya", "include": map[string]any{"entities": nil}})
	rec.Entities = nil
	decode(t, w, &rec)
	if rec.Entities != nil {
		t.Error("include.entities=null turns entities off")
	}
	if w := do(t, h, "POST", "/api/banks/family/memories/recall", map[string]any{"query": "?!"}); w.Code != http.StatusBadRequest {
		t.Errorf("wordless query = %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/banks/family/memories/recall", map[string]any{"query": "x", "budget": "huge"}); w.Code != http.StatusBadRequest {
		t.Errorf("bad budget = %d", w.Code)
	}

	// Listing, entities, documents, chunks.
	var list struct {
		Items []map[string]any
		Total int
	}
	decode(t, do(t, h, "GET", "/api/banks/family/memories?q=pottery", nil), &list)
	if list.Total != 1 {
		t.Errorf("memories filter = %+v", list)
	}
	id := list.Items[0]["id"].(string)
	if w := do(t, h, "GET", "/api/banks/family/memories/"+id, nil); w.Code != http.StatusOK {
		t.Errorf("get memory = %d", w.Code)
	}
	decode(t, do(t, h, "GET", "/api/banks/family/entities", nil), &list)
	if len(list.Items) != 3 || list.Items[0]["canonical_name"] != "Maya" || list.Items[0]["mention_count"] != float64(2) {
		t.Errorf("entities = %v", list.Items)
	}
	w = do(t, h, "GET", "/api/banks/family/entities/maya", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "green tea") {
		t.Errorf("entity = %d %s", w.Code, w.Body)
	}
	decode(t, do(t, h, "GET", "/api/banks/family/documents", nil), &list)
	if list.Total != 1 || list.Items[0]["facts"] != float64(2) {
		t.Errorf("documents = %+v", list)
	}
	w = do(t, h, "GET", "/api/banks/family/documents/session-1", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "pottery") {
		t.Errorf("document = %d", w.Code)
	}
	w = do(t, h, "GET", "/api/banks/family/chunks/"+url.PathEscape(top.ChunkID), nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "green tea") {
		t.Errorf("chunk = %d %s", w.Code, w.Body)
	}
	var banks struct{ Banks []map[string]any }
	decode(t, do(t, h, "GET", "/api/banks", nil), &banks)
	if len(banks.Banks) != 1 || banks.Banks[0]["facts"] != float64(2) {
		t.Errorf("banks = %v", banks)
	}

	// Deletes.
	if w := do(t, h, "DELETE", "/api/banks/family/memories/"+id, nil); w.Code != http.StatusOK {
		t.Errorf("delete memory = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/api/banks/family/memories/"+id, nil); w.Code != http.StatusNotFound {
		t.Errorf("deleted memory still there: %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/banks/family/documents/session-1", nil); w.Code != http.StatusOK {
		t.Errorf("delete document = %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/banks/family", nil); w.Code != http.StatusOK {
		t.Errorf("delete bank = %d", w.Code)
	}
	if w := do(t, h, "GET", "/api/banks/family", nil); w.Code != http.StatusNotFound {
		t.Errorf("deleted bank = %d", w.Code)
	}
}

func TestRetainCreatesTheBankAndWorksWithNoModel(t *testing.T) {
	_, h := testServer(t)
	w := do(t, h, "POST", "/api/banks/notes:scratch/memories", map[string]any{"items": []map[string]any{
		{"content": "Priya moved to Toronto in March 2022. She works at Shopify."}}})
	if w.Code != http.StatusOK {
		t.Fatalf("retain = %d %s", w.Code, w.Body)
	}
	var rr map[string]any
	decode(t, w, &rr)
	if rr["mode"] != "rules" || rr["bank_created"] != true {
		t.Errorf("retain = %v", rr)
	}
	w = do(t, h, "POST", "/api/banks/notes:scratch/memories/recall", map[string]any{"query": "where does Priya work"})
	if !strings.Contains(w.Body.String(), "Shopify") {
		t.Errorf("recall = %s", w.Body)
	}
	if w := do(t, h, "GET", "/api/notes/banks/notes__scratch/bank.md", nil); w.Code != http.StatusOK {
		t.Errorf("a bank is a folder of ordinary notes: %d", w.Code)
	}
}

// A human edit made through the file survives a re-retain over HTTP, and the
// extracted fact it contradicts comes back below it, marked disputed.
func TestHumanEditWinsOverHTTP(t *testing.T) {
	s, h := testServer(t)
	stubExtractor(t, s, map[string]any{"what": "Maya's favourite tea is green tea", "fact_type": "world",
		"entities": []any{"Maya"}})
	retain := func(content string) {
		if w := do(t, h, "POST", "/api/banks/fam/memories", map[string]any{"items": []map[string]any{
			{"content": content, "document_id": "d"}}}); w.Code != http.StatusOK {
			t.Fatalf("retain = %d %s", w.Code, w.Body)
		}
	}
	retain("Maya: green tea is my favourite")
	p := filepath.Join(s.Vault.Root, "banks/fam/facts/d.md")
	raw, _ := os.ReadFile(p)
	edited := strings.Replace(string(raw), "favourite tea is green tea <!--f", "favourite tea is oolong <!--f", 1)
	if err := os.WriteFile(p, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Upsert("banks/fam/facts/d.md"); err != nil {
		t.Fatal(err)
	}
	retain("Maya: green tea is my favourite, honestly")
	var rec struct {
		Results []struct {
			Text, Authority string
			DisputedBy      string `json:"disputed_by"`
		}
	}
	decode(t, do(t, h, "POST", "/api/banks/fam/memories/recall", map[string]any{"query": "Maya favourite tea green tea"}), &rec)
	if len(rec.Results) < 2 || rec.Results[0].Authority != "human" || !strings.Contains(rec.Results[0].Text, "oolong") {
		t.Fatalf("the person's correction must lead: %+v", rec.Results)
	}
	if rec.Results[1].DisputedBy == "" {
		t.Errorf("the model fact must be marked disputed: %+v", rec.Results[1])
	}
	// Removing a person's fact needs force.
	var list struct{ Items []map[string]any }
	decode(t, do(t, h, "GET", "/api/banks/fam/memories?authority=human", nil), &list)
	if len(list.Items) != 1 {
		t.Fatalf("human facts = %v", list.Items)
	}
	hid := list.Items[0]["id"].(string)
	if w := do(t, h, "DELETE", "/api/banks/fam/memories/"+hid, nil); w.Code != http.StatusConflict {
		t.Errorf("unforced delete of a human fact = %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/banks/fam/memories/"+hid+"?force=true", nil); w.Code != http.StatusOK {
		t.Errorf("forced delete = %d", w.Code)
	}
}

func TestBanksFollowSpacesAndMembership(t *testing.T) {
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, adminKey, "bob", "member")
	w := asKey(t, h, adminKey, "POST", "/api/spaces", map[string]any{"name": "Secret", "prefix": "banks/secret"})
	if w.Code != http.StatusCreated {
		t.Fatalf("space = %d %s", w.Code, w.Body)
	}
	var space map[string]any
	decode(t, w, &space)
	if w := asKey(t, h, adminKey, "POST", "/api/banks/secret/memories", map[string]any{"items": []map[string]any{
		{"content": "The launch code word is heliotrope and only Alice knows it."}}}); w.Code != http.StatusOK {
		t.Fatalf("admin retain = %d %s", w.Code, w.Body)
	}
	recall := map[string]any{"query": "launch code word heliotrope"}
	for _, probe := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/banks/secret", nil},
		{"POST", "/api/banks/secret/memories/recall", recall},
		{"GET", "/api/banks/secret/memories", nil},
		{"GET", "/api/banks/secret/entities", nil},
		{"GET", "/api/banks/secret/documents", nil},
	} {
		w := asKey(t, h, bobKey, probe.method, probe.path, probe.body)
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "heliotrope") {
			t.Errorf("non-member %s %s = %d %s", probe.method, probe.path, w.Code, w.Body)
		}
	}
	if w := asKey(t, h, bobKey, "POST", "/api/banks/secret/memories", map[string]any{"items": []map[string]any{{"content": "x y z"}}}); w.Code < 400 {
		t.Errorf("non-member retain = %d", w.Code)
	}
	if strings.Contains(asKey(t, h, bobKey, "GET", "/api/banks", nil).Body.String(), "secret") {
		t.Error("the bank list leaked a bank bob cannot read")
	}
	// A reader may recall but not retain.
	if w := asKey(t, h, adminKey, "POST", "/api/spaces/"+space["id"].(string)+"/members",
		map[string]any{"user": "bob", "role": "reader"}); w.Code != http.StatusOK {
		t.Fatalf("add member = %d %s", w.Code, w.Body)
	}
	if w := asKey(t, h, bobKey, "POST", "/api/banks/secret/memories/recall", recall); !strings.Contains(w.Body.String(), "heliotrope") {
		t.Errorf("reader recall = %d %s", w.Code, w.Body)
	}
	if w := asKey(t, h, bobKey, "POST", "/api/banks/secret/memories", map[string]any{"items": []map[string]any{{"content": "x y z"}}}); w.Code != http.StatusForbidden {
		t.Errorf("reader retain = %d", w.Code)
	}
	// Bob can keep a bank of his own in the commons.
	if w := asKey(t, h, bobKey, "POST", "/api/banks/bobs/memories", map[string]any{"items": []map[string]any{{"content": "Bob likes rowing on the lake."}}}); w.Code != http.StatusOK {
		t.Errorf("bob's own bank = %d %s", w.Code, w.Body)
	}
	// Anonymous callers get nothing.
	if w := do(t, h, "POST", "/api/banks/secret/memories/recall", recall); strings.Contains(w.Body.String(), "heliotrope") {
		t.Error("anonymous recall leaked")
	}
}

func TestRetainOverHTTPDropsPrivateSpansAndRedactsOnRequest(t *testing.T) {
	s, h := testServer(t)
	key := "ghp_" + strings.Repeat("aB3dE5gH7j", 4)
	w := do(t, h, "POST", "/api/banks/priv/memories", map[string]any{"items": []map[string]any{
		{"content": "Priya works at Shopify. <private>her badge number is 90210</private> Key " + key,
			"scan_secrets": true, "document_id": "d"}}})
	if w.Code != http.StatusOK {
		t.Fatalf("retain = %d %s", w.Code, w.Body)
	}
	_ = filepath.Walk(s.Vault.Root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			raw, _ := os.ReadFile(p)
			if strings.Contains(string(raw), "90210") || strings.Contains(string(raw), key) {
				t.Errorf("%s holds private or secret text", p)
			}
		}
		return nil
	})
	if w := do(t, h, "POST", "/api/banks/priv/memories", map[string]any{"items": []map[string]any{
		{"content": "<private>only this</private>"}}}); w.Code != http.StatusOK {
		t.Errorf("an all-private retain must be a quiet success: %d %s", w.Code, w.Body)
	}
}

func TestBankContextEndpointHonoursMaxChars(t *testing.T) {
	_, h := testServer(t)
	for i := 0; i < 40; i++ {
		do(t, h, "POST", "/api/banks/ctx/memories", map[string]any{"items": []map[string]any{
			{"content": "Service number " + strings.Repeat("n", i+1) + " listens on a port.", "document_id": "d" + strings.Repeat("x", i+1)}}})
	}
	w := do(t, h, "GET", "/api/banks/ctx/context?max_chars=600&source=resume", nil)
	var out struct {
		Context string `json:"context"`
		Chars   int    `json:"chars"`
		Limit   int    `json:"limit"`
		Dropped int    `json:"dropped"`
	}
	decode(t, w, &out)
	if w.Code != http.StatusOK || out.Chars > 600 || out.Limit != 600 || out.Dropped == 0 || !strings.Contains(out.Context, "grimoire_bank_context") {
		t.Fatalf("%d %+v", w.Code, out)
	}
	if w := do(t, h, "GET", "/api/banks/missing/context", nil); w.Code != http.StatusNotFound {
		t.Errorf("missing bank = %d", w.Code)
	}
}
