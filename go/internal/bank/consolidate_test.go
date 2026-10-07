package bank

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
)

var factIDRE = regexp.MustCompile(`\[((?:f|h)[0-9a-f]+(?:-\d+)?)\] ([^\n(]*)`)

// newFacts reads the batch's fact ids and texts out of a consolidation prompt.
func newFacts(user string) (ids, texts []string) {
	part := user
	if i := strings.Index(user, "## Existing observations"); i >= 0 {
		part = user[:i]
	}
	for _, m := range factIDRE.FindAllStringSubmatch(part, -1) {
		ids = append(ids, m[1])
		texts = append(texts, strings.TrimSpace(m[2]))
	}
	return
}

// existingObs reads the observation ids shown in a consolidation prompt.
func existingObs(user string) []map[string]any {
	i := strings.Index(user, "## Existing observations")
	if i < 0 {
		return nil
	}
	var out []map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(user[i+len("## Existing observations"):])), &out)
	return out
}

const consolidationMarker = "You keep a memory bank's observations"

type consolidator struct {
	mu    sync.Mutex
	calls int
	plan  func(ids, texts []string, existing []map[string]any) map[string]any
}

func (c *consolidator) route(next func(string, string) (string, string)) func(string, string) (string, string) {
	return func(sys, user string) (string, string) {
		if !strings.HasPrefix(sys, consolidationMarker) {
			return next(sys, user)
		}
		c.mu.Lock()
		c.calls++
		c.mu.Unlock()
		ids, texts := newFacts(user)
		raw, _ := json.Marshal(c.plan(ids, texts, existingObs(user)))
		return string(raw), "stop"
	}
}

func extractOnly(reply func(string) (string, string)) func(string, string) (string, string) {
	return func(_, user string) (string, string) { return reply(user) }
}

func TestConsolidationCreatesObservationsAndTheyAreRecalled(t *testing.T) {
	h := newHarness(t, true)
	cons := &consolidator{plan: func(ids, texts []string, _ []map[string]any) map[string]any {
		var creates []any
		for i, id := range ids {
			if strings.Contains(texts[i], "Lyon") {
				creates = append(creates, map[string]any{"text": "Alice is based in Lyon", "source_fact_ids": []any{id},
					"evidence": []any{map[string]any{"fact_id": id, "quote": "lives in Lyon"}}, "reason": "new"})
			}
		}
		return map[string]any{"creates": creates, "updates": []any{}, "deletes": []any{}}
	}}
	h.llm.route = cons.route(extractOnly(livesIn("Lyon")))
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon and play chess.", DocumentID: "d1", Tags: []string{"fam"}})
	if n := h.e.pendingConsolidation("b"); n != 2 {
		t.Fatalf("pending = %d, want 2", n)
	}
	// Retain queued a consolidation (the bank consolidates automatically).
	ops, _, _ := h.e.ListOperations("b", OperationQuery{})
	if len(ops) != 1 || ops[0].Kind != OpConsolidation {
		t.Fatalf("auto consolidation not queued: %+v", ops)
	}
	res, err := h.e.Consolidate(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 1 || res.FactsProcessed != 2 || h.e.pendingConsolidation("b") != 0 {
		t.Fatalf("result = %+v pending=%d", res, h.e.pendingConsolidation("b"))
	}
	file := h.read(t, ObservationsPath("b"))
	if !strings.Contains(file, "- Alice is based in Lyon <!--o id=o") || !strings.Contains(file, "proof=1") ||
		!strings.Contains(file, "ev=f") || !strings.Contains(file, "tags=fam") {
		t.Fatalf("observations file:\n%s", file)
	}
	// Nothing new: the next run reads nothing and calls no model.
	calls := cons.calls
	res, _ = h.e.Consolidate(context.Background(), "b")
	if res.Status != "no_new_facts" || cons.calls != calls {
		t.Errorf("second run = %+v", res)
	}
	r := h.recall(t, "b", "Alice is based in Lyon", func(q *RecallRequest) {
		q.Types = []string{"observation"}
		q.SourceFacts = true
	})
	if len(r.Results) != 1 || r.Results[0].Type != "observation" || r.Results[0].ProofCount != 1 {
		t.Fatalf("observation recall = %+v", r.Results)
	}
	if len(r.SourceFacts) != 1 || len(r.Results[0].Entities) == 0 {
		t.Errorf("source facts / inherited entities missing: %+v %+v", r.SourceFacts, r.Results[0])
	}
	// prefer_observations: the fact the observation came from gives way.
	all := h.recall(t, "b", "Alice lives in Lyon", func(q *RecallRequest) { q.PreferObservations = true })
	for _, f := range all.Results {
		if f.Text == "Alice lives in Lyon" {
			t.Errorf("a fact covered by a returned observation was kept: %v", texts2(all.Results))
		}
	}
}

func TestConsolidationNeverRewritesAPersonsObservation(t *testing.T) {
	h := newHarness(t, true)
	var obsID string
	stage := 0
	cons := &consolidator{plan: func(ids, texts []string, existing []map[string]any) map[string]any {
		switch stage {
		case 0:
			return map[string]any{"creates": []any{map[string]any{"text": "Alice lives in Lyon", "source_fact_ids": []any{ids[0]}, "reason": "x"}}}
		default:
			// The model wants to revise and also retire the observation it
			// is shown — which a person has since rewritten.
			for _, o := range existing {
				if o["authority"] != "human" {
					t.Errorf("the edited observation must be shown as human: %v", o)
				}
			}
			return map[string]any{
				"updates": []any{map[string]any{"observation_id": obsID, "text": "Alice lives in Lyon again", "source_fact_ids": []any{ids[0]}, "reason": "restated"}},
				"deletes": []any{map[string]any{"observation_id": obsID, "reason": "Contradicted by the latest chat"}},
			}
		}
	}}
	h.llm.route = cons.route(extractOnly(livesIn("Lyon")))
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon.", DocumentID: "d1"})
	if _, err := h.e.Consolidate(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	of, _, _ := h.e.readObservations("b")
	obsID = of.Current[0].ID
	// A person corrects the observation in their editor.
	h.editFile(t, ObservationsPath("b"), "- Alice lives in Lyon <!--o", "- Alice lives in Paris <!--o")
	stage = 1
	h.retain(t, "b", Item{Content: "Alice: Lyon is still home.", DocumentID: "d2"})
	res, err := h.e.Consolidate(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if res.Challenges != 2 || res.Updated != 0 || res.Deleted != 0 {
		t.Fatalf("result = %+v", res)
	}
	file := h.read(t, ObservationsPath("b"))
	if !strings.Contains(file, "- Alice lives in Paris <!--o id="+obsID+" by=human") {
		t.Fatalf("the person's observation was changed:\n%s", file)
	}
	if !strings.Contains(file, "chal="+obsID) {
		t.Fatalf("no challenge filed:\n%s", file)
	}
	// Recall: the person's observation first, the challenge disputed below it.
	r := h.recall(t, "b", "where does Alice live Lyon", func(q *RecallRequest) { q.Types = []string{"observation"} })
	if len(r.Results) < 2 || r.Results[0].ID != obsID || r.Results[0].Authority != "human" {
		t.Fatalf("recall = %+v", r.Results)
	}
	disputed := false
	for _, f := range r.Results[1:] {
		disputed = disputed || f.DisputedBy == obsID
	}
	if !disputed {
		t.Errorf("challenge not marked disputed: %+v", r.Results)
	}
	// A rebuild from the files keeps all of it.
	if _, err := h.ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	r2 := h.recall(t, "b", "where does Alice live Lyon", func(q *RecallRequest) { q.Types = []string{"observation"} })
	if r2.Results[0].ID != obsID || r2.Results[0].Authority != "human" {
		t.Errorf("reindex lost authorship: %+v", r2.Results)
	}
}

func TestConsolidationRevisesItsOwnObservationWithHistory(t *testing.T) {
	h := newHarness(t, true)
	stage := 0
	var obsID string
	cons := &consolidator{plan: func(ids, _ []string, _ []map[string]any) map[string]any {
		if stage == 0 {
			return map[string]any{"creates": []any{map[string]any{"text": "Alice lives in Lyon", "source_fact_ids": []any{ids[0]}}}}
		}
		return map[string]any{"updates": []any{map[string]any{"observation_id": obsID,
			"text": "Alice lived in Lyon until 2024 and now lives in Paris", "source_fact_ids": []any{ids[0]}}}}
	}}
	h.llm.route = cons.route(extractOnly(livesIn("Lyon")))
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon.", DocumentID: "d1"})
	h.e.Consolidate(context.Background(), "b")
	of, _, _ := h.e.readObservations("b")
	obsID = of.Current[0].ID
	stage = 1
	h.llm.route = cons.route(extractOnly(livesIn("Paris")))
	h.retain(t, "b", Item{Content: "Alice: I moved to Paris.", DocumentID: "d2"})
	res, err := h.e.Consolidate(context.Background(), "b")
	if err != nil || res.Updated != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	file := h.read(t, ObservationsPath("b"))
	if !strings.Contains(file, "- Alice lived in Lyon until 2024 and now lives in Paris <!--o id="+obsID) ||
		!strings.Contains(file, "## History") || !strings.Contains(file, "- ~~Alice lives in Lyon~~ <!--o of="+obsID) {
		t.Fatalf("revision or history missing:\n%s", file)
	}
	if !strings.Contains(file, "proof=2") {
		t.Errorf("sources not accumulated:\n%s", file)
	}
	_, hist, err := h.e.GetObservation("b", obsID)
	if err != nil || len(hist) != 1 {
		t.Errorf("history = %+v %v", hist, err)
	}
}

func TestConsolidationHalvesAFailingBatch(t *testing.T) {
	h := newHarness(t, true)
	cons := &consolidator{plan: func(ids, texts []string, _ []map[string]any) map[string]any {
		for _, tx := range texts {
			if strings.Contains(tx, "chess") && len(ids) > 1 {
				return nil // a malformed reply for any batch holding the chess fact with others
			}
		}
		if len(ids) == 1 && strings.Contains(texts[0], "chess") {
			return nil
		}
		return map[string]any{"creates": []any{map[string]any{"text": texts[0], "source_fact_ids": []any{ids[0]}}}}
	}}
	h.llm.route = func(sys, user string) (string, string) {
		if strings.HasPrefix(sys, consolidationMarker) {
			ids, texts := newFacts(user)
			p := cons.plan(ids, texts, nil)
			if p == nil {
				return "not json at all", "stop"
			}
			raw, _ := json.Marshal(p)
			return string(raw), "stop"
		}
		return livesIn("Lyon")(user)
	}
	h.retain(t, "b", Item{Content: "Alice: Lyon, chess.", DocumentID: "d1"})
	res, err := h.e.Consolidate(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if res.FactsProcessed != 1 || res.FactsFailed != 1 || res.Created != 1 || res.LLMFailures < 2 {
		t.Fatalf("res = %+v", res)
	}
	if h.e.pendingConsolidation("b") != 0 {
		t.Error("a fact that failed alone must not be retried forever")
	}
}

func TestNearDuplicateCreateMergesIntoTheTwin(t *testing.T) {
	h := newHarness(t, true)
	cons := &consolidator{plan: func(ids, texts []string, _ []map[string]any) map[string]any {
		var cr []any
		for _, id := range ids {
			cr = append(cr, map[string]any{"text": "Alice plays chess on Sundays", "source_fact_ids": []any{id}})
		}
		return map[string]any{"creates": cr}
	}}
	h.llm.route = cons.route(extractOnly(livesIn("Lyon")))
	h.retain(t, "b", Item{Content: "Alice: Lyon, chess.", DocumentID: "d1"})
	res, err := h.e.Consolidate(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	of, _, _ := h.e.readObservations("b")
	if res.Created != 1 || res.Merged != 1 || len(of.Current) != 1 || len(of.Current[0].Sources) != 2 {
		t.Fatalf("res = %+v obs = %+v", res, of.Current)
	}
}

func TestConsolidationNeedsAModel(t *testing.T) {
	h := newHarness(t, false)
	h.retain(t, "b", Item{Content: "Alice lives in Lyon.", DocumentID: "d1"})
	if _, err := h.e.Consolidate(context.Background(), "b"); err != ErrModelRequired {
		t.Errorf("err = %v", err)
	}
	if ops, _, _ := h.e.ListOperations("b", OperationQuery{}); len(ops) != 0 {
		t.Errorf("no model, yet a consolidation was queued: %+v", ops)
	}
}

func TestObservationFileRoundTrips(t *testing.T) {
	o := Observation{ID: "o1", Text: "Dana, the lead, said: ship it", Sources: []string{"f1", "f2"},
		Evidence: []Evidence{{FactID: "f1", Quote: "ship it; now = go"}}, Tags: []string{"a b", "c"}}
	line := o.Format()
	p, ok := ParseObservationLine(line, "b")
	if !ok || p.Text != o.Text || p.IsHuman() || len(p.Sources) != 2 || p.Evidence[0].Quote != "ship it; now = go" ||
		p.Tags[0] != "a b" {
		t.Fatalf("round trip: %s → %+v", line, p)
	}
	hand, _ := ParseObservationLine("- Dana prefers mornings", "b")
	if !hand.IsHuman() || hand.ID == "" {
		t.Errorf("hand-typed observation = %+v", hand)
	}
}

// In a fresh bank the first batch has nothing to revise, so related facts must
// still end up in one observation: within a batch through one create citing
// every fact, across batches through an update of what an earlier batch made.
func TestFreshBankMergesRelatedFactsAcrossBatches(t *testing.T) {
	h := newHarness(t, true)
	var sawSystem string
	cons := &consolidator{plan: func(ids, texts []string, existing []map[string]any) map[string]any {
		var src []any
		for _, id := range ids {
			src = append(src, id)
		}
		if len(existing) > 0 {
			return map[string]any{"updates": []any{map[string]any{"observation_id": existing[0]["id"],
				"text": "Alice keeps a vegetable garden", "source_fact_ids": src}}}
		}
		return map[string]any{"creates": []any{map[string]any{"text": "Alice keeps a vegetable garden", "source_fact_ids": src}}}
	}}
	h.llm.route = func(sys, user string) (string, string) {
		if strings.HasPrefix(sys, consolidationMarker) {
			sawSystem = sys
			return cons.route(nil)(sys, user)
		}
		return extractionReply(
			map[string]any{"what": "Alice grows tomatoes in her garden", "fact_type": "world", "fact_kind": "conversation", "entities": []any{"Alice"}},
			map[string]any{"what": "Alice grows carrots in her garden", "fact_type": "world", "fact_kind": "conversation", "entities": []any{"Alice"}},
			map[string]any{"what": "Alice waters her garden every morning", "fact_type": "world", "fact_kind": "conversation", "entities": []any{"Alice"}},
			map[string]any{"what": "Alice built raised beds in her garden", "fact_type": "world", "fact_kind": "conversation", "entities": []any{"Alice"}},
		), "stop"
	}
	h.retain(t, "b", Item{Content: "Alice talks about her garden.", DocumentID: "d1"})
	h.e.UpdateProfile("b", func(p *Profile) error { p.Config["consolidation_batch_size"] = "2"; return nil })
	res, err := h.e.Consolidate(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if res.Batches != 2 || res.Created != 1 || res.Updated != 1 || res.ObservationTotal != 1 {
		t.Fatalf("res = %+v", res)
	}
	of, _, _ := h.e.readObservations("b")
	if len(of.Current) != 1 || len(of.Current[0].Sources) != 4 {
		t.Fatalf("observations = %+v", of.Current)
	}
	for _, want := range []string{"Related new facts belong together", "ONE observation"} {
		if !strings.Contains(sawSystem, want) {
			t.Errorf("consolidation prompt lacks %q", want)
		}
	}
}

func TestEditedObservationBecomesAPersonsAndSurvivesConsolidation(t *testing.T) {
	h := newHarness(t, true)
	revise := false
	cons := &consolidator{plan: func(ids, texts []string, existing []map[string]any) map[string]any {
		if len(existing) > 0 && revise {
			return map[string]any{"updates": []any{map[string]any{"observation_id": existing[0]["id"],
				"text": "Alice lives in Nice", "source_fact_ids": []any{ids[0]}}}}
		}
		return map[string]any{"creates": []any{map[string]any{"text": "Alice is based in Lyon", "source_fact_ids": []any{ids[0]}}}}
	}}
	h.llm.route = cons.route(extractOnly(livesIn("Lyon")))
	h.retain(t, "b", Item{Content: "Alice: Lyon.", DocumentID: "d1"})
	if _, err := h.e.Consolidate(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	of, _, _ := h.e.readObservations("b")
	if len(of.Current) == 0 || of.Current[0].IsHuman() {
		t.Fatalf("setup: %+v", of.Current)
	}
	id := of.Current[0].ID
	if _, err := h.e.UpdateObservation("b", id, "  "); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty text: %v", err)
	}
	if _, err := h.e.UpdateObservation("b", "onope", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
	out, err := h.e.UpdateObservation("b", id, "Alice has lived in Lyon since 2019")
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != id || !strings.Contains(h.read(t, ObservationsPath("b")), "by=human") {
		t.Fatalf("edit = %+v\n%s", out, h.read(t, ObservationsPath("b")))
	}
	_, hist, _ := h.e.GetObservation("b", id)
	if len(hist) != 1 || hist[0].Text != "Alice is based in Lyon" {
		t.Errorf("history = %+v", hist)
	}
	// A new fact makes the model want to revise it: it is filed as a challenge.
	revise = true
	h.llm.route = cons.route(extractOnly(func(string) (string, string) {
		return extractionReply(map[string]any{"what": "Alice moved to Nice", "fact_type": "world", "fact_kind": "conversation",
			"entities": []any{"Alice", "Nice"}}), "stop"
	}))
	h.retain(t, "b", Item{Content: "Alice: Nice.", DocumentID: "d2"})
	res, err := h.e.Consolidate(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	of, _, _ = h.e.readObservations("b")
	var mine *Observation
	for i := range of.Current {
		if of.Current[i].ID == id {
			mine = &of.Current[i]
		}
	}
	if mine == nil || !mine.IsHuman() || mine.Text != "Alice has lived in Lyon since 2019" || res.Challenges != 1 || res.Updated != 0 {
		t.Fatalf("person's observation changed: %+v res=%+v", mine, res)
	}
}
