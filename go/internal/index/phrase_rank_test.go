package index

import (
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

func TestPhraseScoreSeparatesMiddleInitials(t *testing.T) {
	q := memory.Tokens("what does Priya A. Schmidt own")
	gold := phraseScore(q, "Priya A. Schmidt owns the backup rotation")
	other := phraseScore(q, "Priya N. Schmidt works in UTC")
	if gold <= 0 {
		t.Fatalf("gold with the same full name scored %v, want > 0", gold)
	}
	if other != 0 {
		t.Errorf("a different middle initial scored %v, want 0 (only the first name is shared)", other)
	}
}

func TestPhraseScoreNeedsARunOfTwo(t *testing.T) {
	// One shared word is not identity: "deploy" alone must not score.
	if got := phraseScore(memory.Tokens("deploy"), "deploy the server"); got != 0 {
		t.Errorf("single-word query scored %v, want 0", got)
	}
	// Both words appear, but not next to each other: no run of two.
	if got := phraseScore(memory.Tokens("priya schmidt"), "schmidt said priya is out"); got != 0 {
		t.Errorf("non-contiguous words scored %v, want 0", got)
	}
}

func TestPhraseScoreIsFractionOfTheQuery(t *testing.T) {
	q := memory.Tokens("priya schmidt owns")
	got := phraseScore(q, "priya schmidt owns the rotation")
	if want := 3.0 / 3.0; got != want {
		t.Errorf("full run = %v, want %v", got, want)
	}
	got = phraseScore(q, "priya schmidt works in utc")
	if want := 2.0 / 3.0; got != want {
		t.Errorf("run of two = %v, want %v", got, want)
	}
}

func TestPhraseRankingPutsTheSameNameFirst(t *testing.T) {
	// The distractors say exactly what the target says except for the middle
	// initial, and are newer, so without the phrase term the recency tie-break
	// would put one of them first. Keyword and entity scores cannot tell them
	// apart: only the run of the full name can.
	ix := testIndex(t)
	memNote(t, ix, "memory/people.md",
		entry("other1", "2026-08-15 09:00", "Priya N. Schmidt owns the backup rotation"),
		entry("other2", "2026-08-15 08:00", "Priya M. Schmidt owns the backup rotation"),
		entry("other3", "2026-08-15 07:00", "Priya F. Schmidt owns the backup rotation"),
		entry("gold", "2026-08-14 09:00", "Priya A. Schmidt owns the backup rotation"))
	hits, err := ix.MemoryEntries(MemoryQuery{Query: "what does Priya A. Schmidt own", LexicalOnly: true, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].ID != "gold" {
		t.Errorf("same-name fact is not first: %v", ids(hits))
	}
}
