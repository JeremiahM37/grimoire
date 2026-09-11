package ai

import (
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"testing"
)

func TestModelCannotEraseARecognizedHumanChallenge(t *testing.T) {
	server, calls := fakeOllama(t, "UPDATE 0")
	client := New(mapSettings{"ollama_url": server.URL}, nil)
	decision := client.DecideMemoryAs("the deployment port is 5432", "", false, []memory.Entry{
		{ID: "human-port", Text: "the deployment port is 6432", Human: true},
		{ID: "other", Text: "the staging port is 5432"},
	})
	if decision.Challenges != "human-port" || decision.Op != memory.OpAdd || len(*calls) != 0 {
		t.Fatalf("lost protected correction: %+v (%d model calls)", decision, len(*calls))
	}
}
