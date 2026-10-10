package memory

import (
	"strings"
	"testing"
)

// The derivation table is the contract. Each row is one way a fact can come to
// be on file, and the basis it must read as. A change to Basis() that moves a
// row is a change to what the store claims to know, so it has to be deliberate.
func TestBasisDerivationTable(t *testing.T) {
	const stamp = "2026-10-01 09:00"
	// An id minted for "old" text, then the text was edited without the id:
	// the signature of a person correcting a bullet in their editor.
	edited := Entry{ID: DeriveID(stamp, "claude", "old text"), Stamp: stamp,
		Agent: "claude", Text: "new text"}

	cases := []struct {
		name  string
		entry Entry
		want  Basis
	}{
		{"by=human is stated", Entry{Human: true, Agent: "claude", Text: "x"}, BasisStated},
		{"a bullet with no trailer is stated",
			mustParse(t, "- **2026-10-01 09:00 · me** — the deploy host is prod-1"), BasisStated},
		{"an id that no longer matches its text is stated", edited, BasisStated},
		{"import:* origin is imported", Entry{Origin: "import:mem0", Agent: "import:mem0"}, BasisImported},
		{"import wins over a human claim", Entry{Origin: "import:mem0", Human: true}, BasisImported},
		{"a connector origin is pulled", Entry{Origin: "connector:slack:C123", Agent: "claude"}, BasisPulled},
		{"a web origin is pulled", Entry{Origin: "web:example.com", Agent: "claude"}, BasisPulled},
		{"pulled beats a human claim, as AuthorityOf does",
			Entry{Origin: "connector:jira", Human: true}, BasisPulled},
		{"agent write with evidence is observed",
			Entry{Agent: "claude", Text: "x", Evidence: "memory/src.md"}, BasisObserved},
		{"observation category is observed", Entry{Agent: "claude", Category: "Tool_Result"}, BasisObserved},
		{"bare agent assertion is inferred", Entry{Agent: "claude", Text: "x"}, BasisInferred},
		{"self origin is still an assertion", Entry{Origin: "self", Agent: "claude"}, BasisInferred},
		{"consolidation output is inferred even with evidence",
			Entry{Agent: "claude", Task: "consolidate nightly", Evidence: "memory/a.md"}, BasisInferred},
		{"reflect and dream agents are inferred",
			Entry{Agent: "reflect-pass", Evidence: "x"}, BasisInferred},
		{"synthesis category is inferred", Entry{Agent: "claude", Category: "synthesis", Evidence: "x"}, BasisInferred},
		{"an empty entry is inferred", Entry{}, BasisInferred},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.entry.Basis(); got != c.want {
				t.Fatalf("Basis() = %q, want %q", got, c.want)
			}
		})
	}
}

func mustParse(t *testing.T, line string) Entry {
	t.Helper()
	e, ok := ParseLine(line)
	if !ok {
		t.Fatalf("not a bullet: %q", line)
	}
	return e
}

// Basis is derived, so it must never be written. A bullet that carried a basis
// field would make the file the source of a claim the file cannot check.
func TestBasisIsNeverWrittenToTheBullet(t *testing.T) {
	e := Entry{ID: "0123456789ab", Text: "x", Agent: "claude", Stamp: "2026-10-01 09:00",
		Evidence: "memory/a.md", Origin: "web:x.com"}
	if got := e.Format(); strings.Contains(got, "basis") {
		t.Fatalf("basis leaked into the bullet: %s", got)
	}
}

func TestEvidenceRoundTripsThroughTheTrailer(t *testing.T) {
	// The id must hash the content, or the parser reads the line as a hand edit
	// and the basis is stated: that is the real behaviour, not a test artefact.
	e := Entry{Text: "the cache is redis", Agent: "claude", Stamp: "2026-10-01 09:00",
		Evidence: "memory/ops.md,https://x.io/a b>c"}
	e.ID = DeriveID(e.Stamp, e.Agent, e.Text)
	back, ok := ParseLine(e.Format())
	if !ok {
		t.Fatalf("formatted line does not parse: %s", e.Format())
	}
	if back.Evidence != e.Evidence {
		t.Fatalf("evidence = %q, want %q (line %s)", back.Evidence, e.Evidence, e.Format())
	}
	if back.Basis() != BasisObserved {
		t.Fatalf("round-tripped basis = %q, want observed", back.Basis())
	}
}

func TestNoEvidenceWritesNoTrailerField(t *testing.T) {
	e := Entry{ID: "0123456789ab", Text: "x", Agent: "claude", Stamp: "2026-10-01 09:00"}
	if strings.Contains(e.Format(), "ev=") {
		t.Fatalf("an entry with no evidence wrote an ev field: %s", e.Format())
	}
}

func TestNormalizeEvidence(t *testing.T) {
	got, err := NormalizeEvidence([]string{"  memory/a.md ", "", "https://x.io"})
	if err != nil || len(got) != 2 || got[0] != "memory/a.md" {
		t.Fatalf("NormalizeEvidence = %v, %v", got, err)
	}
	if _, err := NormalizeEvidence([]string{"a,b"}); err == nil {
		t.Error("a comma inside an item must be refused: it would split the item")
	}
	if _, err := NormalizeEvidence([]string{"a\nb"}); err == nil {
		t.Error("a line break inside an item must be refused")
	}
	many := make([]string, MaxEvidence+1)
	for i := range many {
		many[i] = "x"
	}
	if _, err := NormalizeEvidence(many); err == nil {
		t.Error("more than MaxEvidence items must be refused")
	}
}

func TestParseBases(t *testing.T) {
	got, err := ParseBases(" Stated, observed ,")
	if err != nil || len(got) != 2 || got[0] != BasisStated || got[1] != BasisObserved {
		t.Fatalf("ParseBases = %v, %v", got, err)
	}
	if got, err := ParseBases(""); err != nil || got != nil {
		t.Fatalf("empty filter = %v, %v; want no filter", got, err)
	}
	if _, err := ParseBases("stated,bogus"); err == nil {
		t.Error("an unknown basis must be an error, not a filter that matches nothing")
	}
}

func TestParseRecallMode(t *testing.T) {
	for in, want := range map[string]RecallMode{"": RecallAll, "all": RecallAll,
		"Factual": RecallFactual, "personal": RecallPersonal} {
		got, err := ParseRecallMode(in)
		if err != nil || got != want {
			t.Errorf("ParseRecallMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseRecallMode("facts"); err == nil {
		t.Error("an unknown mode must be an error")
	}
}

func TestPersonalCategorySetIsTheDefaultAndConfigurable(t *testing.T) {
	t.Setenv(PersonalCategoriesEnv, "")
	for _, c := range []string{"preference", "persona", "style", "likes", "LIKES"} {
		if !IsPersonal(c) {
			t.Errorf("%q should be personal by default", c)
		}
	}
	for _, c := range []string{"fact", "procedure", "rule", "", "gotcha"} {
		if IsPersonal(c) {
			t.Errorf("%q must not be personal by default", c)
		}
	}

	t.Setenv(PersonalCategoriesEnv, " Quirk , voice ")
	if !IsPersonal("quirk") || !IsPersonal("VOICE") {
		t.Error("GRIMOIRE_PERSONAL_CATEGORIES did not replace the set")
	}
	if IsPersonal("preference") {
		t.Error("an overridden set must replace the default, not extend it")
	}

	t.Setenv(PersonalCategoriesEnv, " , ")
	if !IsPersonal("persona") {
		t.Error("a blank override must fall back to the default set")
	}
}
