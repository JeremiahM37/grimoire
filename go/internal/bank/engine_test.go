package bank

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

type mapSettings map[string]string

func (m mapSettings) Get(k string) string { return m[k] }

// stubLLM is an OpenAI-compatible endpoint whose replies a test scripts.
type stubLLM struct {
	calls  atomic.Int64
	mu     sync.Mutex
	seen   []string
	system []string
	reply  func(user string) (content, finish string)
	// route, when set, answers instead of reply and sees the system prompt.
	route func(system, user string) (content, finish string)
}

func (s *stubLLM) handler(w http.ResponseWriter, r *http.Request) {
	s.calls.Add(1)
	var body struct {
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	user, sys := "", ""
	for _, m := range body.Messages {
		if m.Role == "user" {
			user = m.Content
		} else if m.Role == "system" {
			sys = m.Content
		}
	}
	s.mu.Lock()
	s.seen = append(s.seen, user)
	s.system = append(s.system, sys)
	s.mu.Unlock()
	content, finish := `{"facts":[]}`, "stop"
	if s.route != nil {
		content, finish = s.route(sys, user)
	} else if s.reply != nil {
		content, finish = s.reply(user)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"content": content}, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 20},
	})
}

type harness struct {
	e    *Engine
	ix   *index.Index
	v    *vault.Vault
	llm  *stubLLM
	root string
}

func newHarness(t *testing.T, withLLM bool) *harness {
	t.Helper()
	root := t.TempDir()
	v, err := vault.New(root)
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(root, ".grimoire", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ix := index.New(database, v, embed.Hash{})
	settings := mapSettings{}
	stub := &stubLLM{}
	if withLLM {
		srv := httptest.NewServer(http.HandlerFunc(stub.handler))
		t.Cleanup(srv.Close)
		settings["llm"] = "openai"
		settings["llm_base_url"] = srv.URL
		settings["llm_model"] = "stub"
	}
	e := New(ix, v, ai.New(settings, nil), nil)
	e.Now = func() time.Time { return time.Date(2023, 5, 24, 12, 0, 0, 0, time.UTC) }
	ix.Banks = e
	return &harness{e: e, ix: ix, v: v, llm: stub, root: root}
}

func ts(s string) *time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return &t
}

func (h *harness) retain(t *testing.T, bank string, items ...Item) *RetainResult {
	t.Helper()
	res, err := h.e.Retain(context.Background(), bank, items, RetainOptions{Agent: "test"})
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	return res
}

func (h *harness) recall(t *testing.T, bank, q string, mod ...func(*RecallRequest)) *RecallResponse {
	t.Helper()
	req := RecallRequest{Query: q, Trace: true}
	for _, m := range mod {
		m(&req)
	}
	res, err := h.e.Recall(context.Background(), bank, req)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	return res
}

func (h *harness) read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// editFile replaces text in a vault file the way a person in an editor would,
// and lets the index notice, as the watcher would.
func (h *harness) editFile(t *testing.T, rel, old, new string) {
	t.Helper()
	p := filepath.Join(h.root, rel)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("%s does not contain %q:\n%s", rel, old, b)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ix.Upsert(rel); err != nil {
		t.Fatal(err)
	}
}

func texts2(rs []RecallFact) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Text
	}
	return out
}

func TestRuleRetainAndRecallWithNoModel(t *testing.T) {
	h := newHarness(t, false)
	res := h.retain(t, "alice", Item{
		Content:   "Alice adopted a beagle named Biscuit last Tuesday. She works as a nurse at Mercy Hospital. The weather was fine.",
		Timestamp: ts("2023-05-10T09:00:00Z"), DocumentID: "s1", Tags: []string{"pets"},
	})
	if res.Mode != "rules" || !res.BankCreate || res.Documents[0].Facts != 3 {
		t.Fatalf("result = %+v", res)
	}
	facts := h.read(t, FactsPath("alice", "s1"))
	if !strings.Contains(facts, "Alice adopted a beagle named Biscuit last Tuesday <!--f id=f") ||
		!strings.Contains(facts, "kind=event") || !strings.Contains(facts, "occ=2023-05-09..2023-05-09") {
		t.Errorf("facts file:\n%s", facts)
	}
	if !strings.Contains(facts, "ent=Alice;Biscuit") || !strings.Contains(facts, "tags=pets") {
		t.Errorf("entities/tags missing:\n%s", facts)
	}
	r := h.recall(t, "alice", "what dog did Alice adopt?")
	if len(r.Results) == 0 || !strings.Contains(r.Results[0].Text, "beagle") {
		t.Fatalf("recall = %v", texts2(r.Results))
	}
	if r.Results[0].ChunkID != ChunkID("alice", "s1", 0) || r.Results[0].Authority != "agent" {
		t.Errorf("result = %+v", r.Results[0])
	}
	if _, ok := r.Entities["Alice"]; !ok {
		t.Errorf("entities = %v", r.Entities)
	}
	if r.Trace == nil || len(r.Trace.Arms["keyword"]) == 0 {
		t.Errorf("trace arms = %v", r.Trace)
	}
	// The hashing embedder in tests matches on shared words only, so the
	// semantic arm is exercised with a close paraphrase.
	r = h.recall(t, "alice", "Alice adopted a beagle named Biscuit")
	if len(r.Trace.Arms["semantic"]) == 0 {
		t.Errorf("semantic arm empty: %+v", r.Trace.Arms)
	}
	// The temporal arm fires on a date in the question.
	r = h.recall(t, "alice", "what happened on May 9th 2023?")
	if r.Trace.Window == nil || len(r.Trace.Arms["temporal"]) == 0 {
		t.Errorf("temporal arm did not fire: %+v", r.Trace)
	}
}

func extractionReply(facts ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"facts": facts})
	return string(b)
}

func TestModelRetainStoresTheSchema(t *testing.T) {
	h := newHarness(t, true)
	h.llm.reply = func(user string) (string, string) {
		return extractionReply(
			map[string]any{"what": "Alice lost her job at Acme", "when": "early May 2023", "where": "N/A",
				"who": "Alice", "why": "N/A", "fact_kind": "event", "occurred_start": "2023-05",
				"fact_type": "world", "entities": []any{"Alice", "Acme"}},
			map[string]any{"what": "Alice moved to Lyon", "when": "N/A", "who": "Alice", "why": "she could not pay rent",
				"fact_kind": "event", "occurred_start": "2023-05-20", "occurred_end": "2023-05-20",
				"fact_type": "world", "entities": []any{map[string]any{"text": "Alice"}, "Lyon"},
				"causal_relations": []any{map[string]any{"target_index": 0, "relation_type": "caused_by"}}},
			map[string]any{"what": "", "fact_type": "world"},
			map[string]any{"what": "I fixed the deploy script", "fact_type": "assistant", "fact_kind": "conversation",
				"entities": []any{"deploy script"}},
		), "stop"
	}
	h.e.UpdateProfile("alice", func(*Profile) error { return nil }) // not created yet: error ignored
	if err := h.e.CreateBank(&Profile{ID: "alice", Name: "Alice", Disposition: Disposition{3, 3, 3},
		RetainMission: "Track Alice's life events.", Config: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	res := h.retain(t, "alice", Item{Content: "chat transcript", Timestamp: ts("2023-05-24T10:00:00Z"),
		DocumentID: "conv-1", Context: "a chat with Alice", Metadata: map[string]string{"channel": "sms"}})
	if res.Mode != ModeConcise || res.Documents[0].FactsAdded != 3 {
		t.Fatalf("result = %+v", res)
	}
	user, sys := h.llm.seen[0], h.llm.system[0]
	if !strings.Contains(user, "Track Alice's life events.") || !strings.Contains(user, "Event Date: Wednesday, May 24, 2023") ||
		!strings.Contains(user, "Context: a chat with Alice") || !strings.Contains(user, "channel: sms") {
		t.Errorf("user message:\n%s", user)
	}
	if !strings.Contains(sys, "causal_relations") || strings.Contains(sys, "Track Alice") {
		t.Error("the system prompt carries the causal section and never the mission")
	}
	facts := h.read(t, FactsPath("alice", "conv-1"))
	for _, want := range []string{
		"Alice lost her job at Acme | When: early May 2023 | Involving: Alice <!--f",
		"occ=2023-05-01..2023-05-31", // a month-only date widens to the month
		"Alice moved to Lyon | Involving: Alice | she could not pay rent",
		"type=experience",
		"men=2023-05-24T10:00:00Z",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts file lacks %q:\n%s", want, facts)
		}
	}
	ff := ParseFacts(strings.SplitN(facts, "---\n", 3)[2], "alice", "conv-1")
	if len(ff.Facts) != 3 || len(ff.Facts[1].Causes) != 1 || ff.Facts[1].Causes[0] != ff.Facts[0].ID {
		t.Fatalf("causal link lost: %+v", ff.Facts)
	}
	for _, f := range ff.Facts {
		if f.IsHuman() {
			t.Errorf("a model fact parsed as human: %+v", f)
		}
	}
	r := h.recall(t, "alice", "why did Alice move to Lyon?")
	if len(r.Results) == 0 || r.Results[0].Context != "a chat with Alice" || r.Results[0].Metadata["channel"] != "sms" {
		t.Fatalf("recall = %+v", r.Results)
	}
	// Types filter.
	r = h.recall(t, "alice", "deploy script", func(q *RecallRequest) { q.Types = []string{"experience"} })
	if len(r.Results) != 1 || r.Results[0].Type != "experience" {
		t.Errorf("types filter: %v", texts2(r.Results))
	}
}

func TestTruncatedReplySplitsTheChunk(t *testing.T) {
	h := newHarness(t, true)
	h.llm.reply = func(user string) (string, string) {
		if strings.Count(user, "Sentence") > 30 {
			return `{"facts":[{"what":"partial`, "length"
		}
		return extractionReply(map[string]any{"what": "a fact from a half", "fact_type": "world",
			"fact_kind": "conversation", "entities": []any{}}), "stop"
	}
	content := longProse(2500)
	res := h.retain(t, "b", Item{Content: content, DocumentID: "d"})
	if res.Documents[0].FactsAdded < 2 || h.llm.calls.Load() < 3 {
		t.Errorf("split did not recover facts: %+v after %d calls", res.Documents[0], h.llm.calls.Load())
	}
}

func TestMalformedRepliesAreRetried(t *testing.T) {
	h := newHarness(t, true)
	var n atomic.Int64
	h.llm.reply = func(string) (string, string) {
		if n.Add(1) < 3 {
			return "sorry, I can't produce JSON today", "stop"
		}
		return "```json\n" + extractionReply(map[string]any{"what": "Bob likes tea", "fact_type": "world",
			"entities": []any{"Bob"}}) + "\n```\nHope this helps!", "stop"
	}
	res := h.retain(t, "b", Item{Content: "Bob: I like tea", DocumentID: "d"})
	if res.Documents[0].FactsAdded != 1 || h.llm.calls.Load() != 3 {
		t.Errorf("facts=%d calls=%d", res.Documents[0].FactsAdded, h.llm.calls.Load())
	}
}

func TestReRetainExtractsOnlyChangedChunks(t *testing.T) {
	h := newHarness(t, true)
	h.llm.reply = func(user string) (string, string) {
		i := strings.Index(user, "Content:\n")
		first := strings.Fields(user[i+9:])
		return extractionReply(map[string]any{"what": "chunk starting " + strings.Join(first[:min(3, len(first))], " "),
			"fact_type": "world", "entities": []any{}}), "stop"
	}
	var paras []string
	for i := 0; i < 6; i++ {
		paras = append(paras, strings.Repeat("Paragraph "+itoa(i)+" words. ", 40))
	}
	setProfile := func() {
		if _, err := h.e.UpdateProfile("b", func(p *Profile) error {
			p.Config["retain_chunk_size"] = "1000"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	h.e.CreateBank(NewProfile("b"))
	setProfile()
	res := h.retain(t, "b", Item{Content: strings.Join(paras, "\n\n"), DocumentID: "d"})
	first := h.llm.calls.Load()
	if res.Documents[0].Chunks < 4 || first != int64(res.Documents[0].Chunks) {
		t.Fatalf("chunks=%d calls=%d", res.Documents[0].Chunks, first)
	}
	// Same content: nothing extracted, nothing rewritten.
	res = h.retain(t, "b", Item{Content: strings.Join(paras, "\n\n"), DocumentID: "d"})
	if h.llm.calls.Load() != first || !res.Documents[0].Unchanged {
		t.Errorf("identical re-retain called the model or rewrote: %+v", res.Documents[0])
	}
	// Change the last paragraph only.
	paras[5] = strings.Repeat("Changed closing words. ", 40)
	res = h.retain(t, "b", Item{Content: strings.Join(paras, "\n\n"), DocumentID: "d"})
	if res.Documents[0].ChunksExtracted != 1 || h.llm.calls.Load() != first+1 {
		t.Errorf("re-extracted %d chunks with %d calls", res.Documents[0].ChunksExtracted, h.llm.calls.Load()-first)
	}
	// Append: the stored text is the base, only the tail is new.
	res = h.retain(t, "b", Item{Content: "A brand new closing paragraph.", DocumentID: "d", UpdateMode: "append"})
	if res.Documents[0].ChunksReused < 3 {
		t.Errorf("append reused only %d chunks", res.Documents[0].ChunksReused)
	}
	doc, err := h.e.GetDocument("b", "d")
	if err != nil || !strings.HasSuffix(doc.Content, "A brand new closing paragraph.") {
		t.Errorf("appended doc = %v", err)
	}
}

func TestPackingAndRRF(t *testing.T) {
	keep, used := packByTokens([]int{5, 50, 3, 4}, 12)
	if len(keep) != 3 || keep[1] != 2 || used != 12 {
		t.Errorf("skip-oversized packing: %v %d", keep, used)
	}
	keep, _ = packByTokens([]int{50, 60}, 10)
	if len(keep) != 1 || keep[0] != 0 {
		t.Errorf("never-empty floor: %v", keep)
	}
	if keep, _ := packByTokens([]int{1}, 0); keep != nil {
		t.Error("budget 0 returns nothing")
	}
	got := fuseRRF([]string{"a", "b"}, [][]int32{{1, 2, 3}, {3, 4}})
	if got[0].pos != 3 || got[0].ranks["a"] != 3 || got[0].ranks["b"] != 1 {
		t.Errorf("an item two arms agree on must lead: %+v", got[0])
	}
	want := 1/61.0 + 1/63.0
	if d := got[0].rrf - want; d > 1e-12 || d < -1e-12 {
		t.Errorf("rrf = %v, want %v", got[0].rrf, want)
	}
	if got[1].pos != 1 {
		t.Errorf("order = %d", got[1].pos)
	}
}

func TestLinks(t *testing.T) {
	h := newHarness(t, false)
	var items []Item
	base := time.Date(2023, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		tm := base.Add(time.Duration(i) * 6 * time.Hour)
		items = append(items, Item{Content: "Carol walked the dog number " + itoa(i) + " around the park.",
			Timestamp: &tm, DocumentID: "d" + itoa(i)})
	}
	h.retain(t, "b", items...)
	c, err := h.e.cache("b")
	if err != nil {
		t.Fatal(err)
	}
	p := c.byID[c.units[0].ID]
	for i := range c.units {
		if c.units[i].Doc == "d10" {
			p = int32(i)
		}
	}
	ns := c.temporalNeighbors(p)
	if len(ns) != temporalLinksPerUnit {
		t.Fatalf("temporal neighbours = %d", len(ns))
	}
	if ns[0].weight != 0.75 || ns[len(ns)-1].weight != temporalMinWeight {
		t.Errorf("weights %v .. %v; 6h apart is 0.75 and far ones floor at 0.3", ns[0].weight, ns[len(ns)-1].weight)
	}
	for _, n := range ns {
		if n.pos == p {
			t.Error("a fact is not its own neighbour")
		}
	}
	sem := c.semanticNeighbors([]int32{p})[p]
	if len(sem) == 0 || len(sem) > semanticLinksPerUnit {
		t.Fatalf("semantic neighbours = %d", len(sem))
	}
	for _, n := range sem {
		if n.pos == p || n.weight < semanticLinkMin {
			t.Errorf("bad semantic link %+v", n)
		}
	}
}

func TestTagsFilter(t *testing.T) {
	h := newHarness(t, false)
	h.retain(t, "b",
		Item{Content: "The red team owns the billing service.", DocumentID: "a", Tags: []string{"red"}},
		Item{Content: "The blue team owns the billing dashboard.", DocumentID: "b", Tags: []string{"blue"}},
		Item{Content: "Everybody owns the billing runbook.", DocumentID: "c"})
	r := h.recall(t, "b", "who owns billing", func(q *RecallRequest) { q.Tags = []string{"red"} })
	got := strings.Join(texts2(r.Results), "|")
	if strings.Contains(got, "blue") || !strings.Contains(got, "red") || !strings.Contains(got, "Everybody") {
		t.Errorf("any: %s", got)
	}
	r = h.recall(t, "b", "who owns billing", func(q *RecallRequest) { q.Tags = []string{"red"}; q.TagsMatch = "any_strict" })
	if got := strings.Join(texts2(r.Results), "|"); strings.Contains(got, "Everybody") {
		t.Errorf("any_strict admitted an untagged fact: %s", got)
	}
}

func TestMaxTokensAndChunks(t *testing.T) {
	h := newHarness(t, false)
	h.retain(t, "b", Item{Content: "Dana plays the cello every evening. Dana joined an orchestra in Lyon. Dana practises scales.", DocumentID: "x"})
	zero := 0
	r := h.recall(t, "b", "Dana cello", func(q *RecallRequest) { q.MaxTokens = &zero; q.ChunkTokens = 100 })
	if len(r.Results) != 0 || len(r.Chunks) != 1 {
		t.Errorf("max_tokens 0 returns no facts but chunks still come back: %d facts %d chunks", len(r.Results), len(r.Chunks))
	}
	small := 8
	r = h.recall(t, "b", "Dana cello", func(q *RecallRequest) { q.MaxTokens = &small })
	if r.Trace.TokensUsed > small && len(r.Results) != 1 {
		t.Errorf("budget overrun: %d tokens, %d facts", r.Trace.TokensUsed, len(r.Results))
	}
	r = h.recall(t, "b", "Dana", func(q *RecallRequest) { q.ChunkTokens = 5 })
	for _, ch := range r.Chunks {
		if !ch.Truncated {
			t.Error("a chunk over its budget is truncated and marked")
		}
	}
}

func TestGraphArmFollowsSharedEntities(t *testing.T) {
	h := newHarness(t, false)
	h.retain(t, "b",
		Item{Content: "Erin adopted a kitten called Pixel.", DocumentID: "a"},
		Item{Content: "Pixel needs a vaccine appointment at the clinic in June.", DocumentID: "b"})
	r := h.recall(t, "b", "Erin adopted a kitten called Pixel")
	found := false
	for _, hit := range r.Trace.Arms["graph"] {
		if c, _ := h.e.cache("b"); c.units[c.byID[hit.ID]].Doc == "b" {
			found = true
		}
	}
	if !found {
		t.Errorf("the vaccine fact shares Pixel with the seed and must be reached by the graph arm: %+v", r.Trace.Arms)
	}
}
