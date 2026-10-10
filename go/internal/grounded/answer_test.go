package grounded

import (
	"context"
	"github.com/JeremiahM37/grimoire/go/internal/bank"
	"strings"
	"testing"
	"time"
)

type scripted struct{ prompts []string }

func (s *scripted) Complete(_ context.Context, _, p string) (Reply, error) {
	s.prompts = append(s.prompts, p)
	switch {
	case strings.Contains(p, "step 1 of"):
		return Reply{Text: "TYPE: list\n- 2023-05-20 | A | hiked"}, nil
	case strings.Contains(p, "steps 2 and 3"):
		return Reply{Text: "working...\nFINAL: hiking"}, nil
	}
	return Reply{Text: "plain"}, nil
}

func corpus() *Prepared {
	return Prepare(context.Background(), []Doc{{ID: "d1", Title: "Session 1 — 23 May 2023", Date: time.Date(2023, 5, 23, 0, 0, 0, 0, time.UTC),
		Lines: []Line{{Speaker: "A", Text: "I hiked last Saturday."}, {Speaker: "B", Text: "Nice."}}}}, nil)
}

func TestDirectAndProcedure(t *testing.T) {
	p := corpus()
	llm := &scripted{}
	a := &Answerer{LLM: llm}
	r, err := a.Answer(context.Background(), "What did A do?", WholeSource{P: p, Annotate: true})
	if err != nil || r.Answer != "plain" || r.Calls != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if !strings.Contains(llm.prompts[0], "last Saturday [= Sat 20 May 2023]") {
		t.Fatal("annotation missing from prompt")
	}
	llm2 := &scripted{}
	a = &Answerer{LLM: llm2, Opt: Options{Procedure: true}}
	r, err = a.Answer(context.Background(), "What did A do?", WholeSource{P: p})
	if err != nil || r.Answer != "hiking" || r.Calls != 2 || !strings.Contains(r.Evidence, "hiked") {
		t.Fatalf("%+v %v", r, err)
	}
	if strings.Contains(llm2.prompts[0], "Saturday [=") {
		t.Fatal("annotation present with Annotate off")
	}
	if !strings.Contains(llm2.prompts[1], "- 2023-05-20 | A | hiked") {
		t.Fatal("evidence not passed to step 2")
	}
}

func TestOriginalTextUnchangedInPrepared(t *testing.T) {
	p := corpus()
	if p.Docs[0].Lines[0].Text != "I hiked last Saturday." {
		t.Fatal("original text modified")
	}
	if !strings.Contains(p.Render(RenderOpts{}), "I hiked last Saturday.\n") {
		t.Fatal(p.Render(RenderOpts{}))
	}
}

func TestExtractFinal(t *testing.T) {
	for in, want := range map[string]string{"a\nFINAL: x y": "x y", "no marker": "no marker", "FINAL: **7 May 2023**": "7 May 2023", "final: a\nFINAL: b": "b"} {
		if got := ExtractFinal(in); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}

func TestTimelineSelect(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2023, 5, day, 0, 0, 0, 0, time.UTC) }
	tl := &Timeline{Events: []Event{
		{Entities: []string{"Dave"}, Text: "Dave played cards with friends", Start: d(3), End: d(3), Dated: true},
		{Entities: []string{"Calvin"}, Text: "Calvin bought a car", Start: d(4), End: d(4), Dated: true},
		{Entities: []string{"Dave"}, Text: "Dave opened a shop", Start: d(1), End: d(1), Dated: true},
	}}
	got := tl.Select("What has Dave done with friends?", 10)
	if len(got) != 2 || got[0].Text != "Dave opened a shop" {
		t.Fatalf("%+v", got)
	}
	if len(tl.Select("Unrelated zebra question", 10)) != 0 {
		t.Fatal("unrelated events returned")
	}
	if len(tl.Select("What has Dave done?", 1)) != 1 {
		t.Fatal("cap not applied")
	}
	if !strings.Contains(RenderEvents(got), "- [2023-05-01] Dave opened a shop") {
		t.Fatal(RenderEvents(got))
	}
}

func TestRetrievalSourceAnnotatesPerPassage(t *testing.T) {
	src := RetrievalSource{Annotate: true, Fetch: func(context.Context, string) ([]Passage, error) {
		return []Passage{{Title: "Log", Date: time.Date(2023, 5, 23, 0, 0, 0, 0, time.UTC), Text: "Shipped it yesterday."}}, nil
	}}
	r, _ := src.Records(context.Background(), "q")
	if !strings.Contains(r.Text, "yesterday [= Mon 22 May 2023]") || !strings.Contains(r.Text, "## Log — 23 May 2023") {
		t.Fatal(r.Text)
	}
}

func TestTimelineFromBankFacts(t *testing.T) {
	facts := []bank.RecallFact{
		{Text: "Ana ran a 5K", Entities: []string{"Ana"}, OccurredStart: "2023-05-20", OccurredEnd: "2023-05-20", MentionedAt: "2023-05-23T10:00:00Z"},
		{Text: "Ana likes tea", Entities: []string{"Ana"}, MentionedAt: "2023-05-23T10:00:00Z"},
		{Text: "orphan with no date", Entities: []string{"Ana"}},
		{Text: "removed", OccurredStart: "2023-01-01", DocRemoved: true},
	}
	tl := TimelineFromFacts(facts)
	if len(tl.Events) != 2 || !tl.Events[0].Dated || tl.Events[0].Text != "Ana ran a 5K" || !tl.Events[1].State {
		t.Fatalf("%+v", tl.Events)
	}
	if !strings.Contains(RenderEvents(tl.Events), "- [2023-05-20] Ana ran a 5K") {
		t.Fatal(RenderEvents(tl.Events))
	}
}
