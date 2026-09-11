package memory

import "testing"

func TestUnrelatedHumanFactDoesNotQuarantineNewKnowledge(t *testing.T) {
	decision := DecideAs("the kestrel deployment uses copper certificates", "", false, []Entry{
		{ID: "human", Text: "the kestrel dashboard uses a violet theme", Human: true},
	})
	if decision.Challenges != "" || decision.Op != OpAdd {
		t.Fatalf("unrelated knowledge quarantined: %+v", decision)
	}
}

func TestProtectedValueChangeIsNotMistakenForNearDuplicate(t *testing.T) {
	previous := "The production kestrel gateway deployment with the regional failover configuration takes 12 minutes"
	next := "The production kestrel gateway deployment with the regional failover configuration takes 13 minutes"
	decision := DecideAs(next, "", false, []Entry{{ID: "human", Text: previous, Human: true}})
	if decision.Op != OpAdd || decision.Challenges != "human" {
		t.Fatalf("lost correction: %+v", decision)
	}
}
