package bank

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

var biscuitRE = regexp.MustCompile(`(?i)biscuit`)

func carriesBiscuit(s string) bool { return biscuitRE.MatchString(s) }
func redactBiscuit(s string) string {
	return biscuitRE.ReplaceAllString(s, "[forgotten]")
}

// seedCascadeBank retains two documents and writes an observation layer and
// a model layer over them, the way consolidation and refresh would.
func seedCascadeBank(t *testing.T) (*harness, map[string]string) {
	t.Helper()
	h := newHarness(t, false)
	h.retain(t, "alice", Item{
		Content:   "Alice adopted a beagle named Biscuit last Tuesday.",
		Timestamp: ts("2023-05-10T09:00:00Z"), DocumentID: "s1",
	}, Item{
		Content:   "Alice works as a nurse at Mercy Hospital.",
		Timestamp: ts("2023-05-10T09:00:00Z"), DocumentID: "s2",
	})
	ids := map[string]string{}
	for _, f := range []struct{ doc, key, text string }{
		{"s1", "beagle", "Biscuit"}, {"s2", "nurse", "nurse"},
	} {
		n, err := h.v.Read(FactsPath("alice", f.doc))
		if err != nil {
			t.Fatal(err)
		}
		for _, fact := range ParseFacts(n.Body, "alice", f.doc).Facts {
			if strings.Contains(fact.Text, f.text) {
				ids[f.key] = fact.ID
			}
		}
	}
	if ids["beagle"] == "" || ids["nurse"] == "" {
		t.Fatalf("seed facts not found: %v", ids)
	}
	of := ObservationsFile{Current: []Observation{
		{ID: "obs-pet", Text: "Alice owns a beagle named Biscuit", Sources: []string{ids["beagle"]}},
		{ID: "obs-mixed", Text: "Alice has a pet and works as a nurse", Sources: []string{ids["beagle"], ids["nurse"]}},
		{ID: "obs-human", Text: "Biscuit is the beagle", Human: true, Sources: []string{ids["beagle"]}},
		{ID: "obs-nurse", Text: "Alice is a nurse", Sources: []string{ids["nurse"]}},
	}}
	if err := h.e.writeObservations("alice", of, ""); err != nil {
		t.Fatal(err)
	}
	writeModel := func(id, body string, based []string) {
		fm := markdown.NewFrontmatter()
		fm.Set("title", id)
		fm.Set("name", id)
		fm.Set("question", "what about "+id+"?")
		fm.Set("bank", "alice")
		fm.Set("based_on", listValue(based))
		fm.Set("scope_sig", "stale-sig")
		fm.Set("body_sum", bodySum(body))
		if _, err := h.v.Write(ModelPath("alice", id), body, fm); err != nil {
			t.Fatal(err)
		}
		if _, err := h.ix.Upsert(ModelPath("alice", id)); err != nil {
			t.Fatal(err)
		}
	}
	writeModel("pets", "Alice owns a beagle named Biscuit.", []string{"obs-pet"})
	writeModel("mixed", "Alice is a nurse. Alice owns Biscuit.", []string{"obs-nurse", "obs-pet"})
	return h, ids
}

func readOr(t *testing.T, h *harness, rel string) string {
	t.Helper()
	n, err := h.v.Read(rel)
	if err != nil {
		return ""
	}
	return n.Body
}

func actionsOf(acts []CascadeAction) map[string]string {
	out := map[string]string{}
	for _, a := range acts {
		out[a.Kind+":"+a.Ref] = a.Action
	}
	return out
}

func TestCascadeReachesFactsObservationsAndModels(t *testing.T) {
	h, ids := seedCascadeBank(t)
	opts := CascadeOptions{Agent: true}
	acts, err := h.e.CascadeForget("alice", carriesBiscuit, redactBiscuit, opts)
	if err != nil {
		t.Fatal(err)
	}
	got := actionsOf(acts)
	want := map[string]string{
		"fact:" + ids["beagle"]: ActRemoved,
		"observation:obs-pet":   ActRetracted,
		"observation:obs-mixed": ActNeedsRederive,
		"observation:obs-human": ActChallenged,
		"model:pets":            ActRemoved,
		"model:mixed":           ActNeedsRederive,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("action %s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if strings.Contains(readOr(t, h, FactsPath("alice", "s1")), "Biscuit") {
		t.Error("the fact file still carries the forgotten text")
	}
	for _, line := range strings.Split(readOr(t, h, ObservationsPath("alice")), "\n") {
		if carriesBiscuit(line) && !strings.Contains(line, "obs-human") {
			t.Errorf("observations still carry the text outside the human record: %q", line)
		}
	}
	of, _, _ := h.e.readObservations("alice")
	var challenge *Observation
	for i := range of.Current {
		if of.Current[i].Challenges == "obs-human" {
			challenge = &of.Current[i]
		}
		if of.Current[i].ID == "obs-pet" || of.Current[i].ID == "obs-mixed" {
			t.Errorf("observation %s should have been removed", of.Current[i].ID)
		}
	}
	if challenge == nil || carriesBiscuit(challenge.Text) || challenge.IsHuman() {
		t.Fatalf("expected an agent challenge beside the human observation, got %+v", challenge)
	}
	// The person's own observation is untouched by an agent-initiated forget.
	human := false
	for _, o := range of.Current {
		if o.ID == "obs-human" && o.Text == "Biscuit is the beagle" {
			human = true
		}
	}
	if !human {
		t.Error("the human observation was altered")
	}
	if readOr(t, h, ModelPath("alice", "pets")) != "" {
		t.Error("model pets should be deleted")
	}
	mixed, err := h.v.Read(ModelPath("alice", "mixed"))
	if err != nil {
		t.Fatal(err)
	}
	if carriesBiscuit(mixed.Body) || mixed.Frontmatter.StringVal("needs_rederive") != "true" ||
		mixed.Frontmatter.StringVal("scope_sig") != "" {
		t.Errorf("mixed model should be cleared and marked:\n%s", mixed.Body)
	}
}

func TestCascadeDryRunChangesNothing(t *testing.T) {
	h, _ := seedCascadeBank(t)
	before := snapshotBank(t, h)
	acts, err := h.e.CascadeForget("alice", carriesBiscuit, redactBiscuit, CascadeOptions{Agent: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) == 0 {
		t.Fatal("dry run planned nothing")
	}
	if after := snapshotBank(t, h); after != before {
		t.Errorf("dry run changed the bank")
	}
}

func TestCascadeIsIdempotent(t *testing.T) {
	h, _ := seedCascadeBank(t)
	if _, err := h.e.CascadeForget("alice", carriesBiscuit, redactBiscuit, CascadeOptions{Agent: true}); err != nil {
		t.Fatal(err)
	}
	before := snapshotBank(t, h)
	acts, err := h.e.CascadeForget("alice", carriesBiscuit, redactBiscuit, CascadeOptions{Agent: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range acts {
		if a.Action != ActKeptHuman && a.Action != "challenge_pending" {
			t.Errorf("second pass still changed something: %+v", a)
		}
	}
	if after := snapshotBank(t, h); after != before {
		t.Error("second pass changed the bank")
	}
}

func TestHumanCascadeMayAlterHumanRecords(t *testing.T) {
	h, _ := seedCascadeBank(t)
	acts, err := h.e.CascadeForget("alice", carriesBiscuit, redactBiscuit, CascadeOptions{Agent: false})
	if err != nil {
		t.Fatal(err)
	}
	got := actionsOf(acts)
	if got["observation:obs-human"] != ActRetracted {
		t.Errorf("human forget should retract the human observation, got %v", got)
	}
	if strings.Contains(readOr(t, h, ObservationsPath("alice")), "Biscuit") {
		t.Error("a human-initiated cascade left the text in observations")
	}
}

func TestCascadeKeepsHumanFactUnderAgentForget(t *testing.T) {
	h, ids := seedCascadeBank(t)
	n, _ := h.v.Read(FactsPath("alice", "s1"))
	ff := ParseFacts(n.Body, "alice", "s1")
	for i := range ff.Facts {
		if ff.Facts[i].ID == ids["beagle"] {
			ff.Facts[i].Human = true
		}
	}
	if err := h.e.rewriteFacts("alice", "s1", n.Path, n.Body, ff); err != nil {
		t.Fatal(err)
	}
	acts, err := h.e.CascadeForget("alice", carriesBiscuit, redactBiscuit, CascadeOptions{Agent: true})
	if err != nil {
		t.Fatal(err)
	}
	if actionsOf(acts)["fact:"+ids["beagle"]] != ActKeptHuman {
		t.Errorf("human fact was not kept: %v", actionsOf(acts))
	}
	if !strings.Contains(readOr(t, h, FactsPath("alice", "s1")), "Biscuit") {
		t.Error("a human fact was removed by an agent-initiated forget")
	}
}

func TestCascadeRejectsBadBank(t *testing.T) {
	h := newHarness(t, false)
	if _, err := h.e.CascadeForget("../x", carriesBiscuit, redactBiscuit, CascadeOptions{}); err == nil {
		t.Error("invalid bank id accepted")
	}
}

// snapshotBank concatenates every file under the bank so two states compare.
func snapshotBank(t *testing.T, h *harness) string {
	t.Helper()
	var b strings.Builder
	root := filepath.Join(h.root, Root)
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		data, _ := os.ReadFile(p)
		b.WriteString(p + "\x00" + string(data) + "\x01")
		return nil
	})
	return b.String()
}
