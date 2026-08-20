package memory

import "testing"

func entries(texts ...string) []Entry {
	var out []Entry
	for i, t := range texts {
		out = append(out, Entry{ID: string(rune('a' + i)), Text: t,
			Stamp: "2026-08-14 09:00", Agent: "claude"})
	}
	return out
}

func TestDecideSupersedesChangedAttribute(t *testing.T) {
	// The case the whole engine exists for: the same attribute of the same
	// subject takes a new value, and the old one must stop being recalled.
	on := entries("user prefers spaces")
	got := Decide("user prefers tabs", on)
	if got.Op != OpUpdate {
		t.Fatalf("op = %s, want UPDATE (%s)", got.Op, got.Why)
	}
	if got.Target != on[0].ID {
		t.Errorf("target = %q, want %q", got.Target, on[0].ID)
	}
	if got.Text != "user prefers tabs" {
		t.Errorf("text = %q", got.Text)
	}
}

func TestDecideSupersedesAcrossDifferentWording(t *testing.T) {
	// Lexically distant, same belief — a similarity threshold alone would miss
	// this, which is why subject+predicate is checked first.
	on := entries("jeremiah works at montana state university")
	got := Decide("jeremiah works at a startup in bozeman", on)
	if got.Op != OpUpdate {
		t.Fatalf("op = %s, want UPDATE (%s)", got.Op, got.Why)
	}
}

func TestDecideNoopsOnRestatement(t *testing.T) {
	on := entries("user prefers tabs")
	if got := Decide("User prefers tabs.", on); got.Op != OpNoop {
		t.Fatalf("op = %s, want NOOP (%s)", got.Op, got.Why)
	}
	// Near-identical phrasing with no attribute structure must also dedupe.
	on2 := entries("the deploy script lives under /usr/local/bin")
	if got := Decide("the deploy script lives under /usr/local/bin", on2); got.Op != OpNoop {
		t.Fatalf("op = %s, want NOOP (%s)", got.Op, got.Why)
	}
}

func TestDecideAddsUnrelatedFact(t *testing.T) {
	on := entries("user prefers tabs", "the server runs proxmox")
	got := Decide("the cat is named marmalade", on)
	if got.Op != OpAdd {
		t.Fatalf("op = %s, want ADD (%s)", got.Op, got.Why)
	}
}

func TestDecideRetractsOnNegation(t *testing.T) {
	on := entries("user prefers tabs")
	got := Decide("user no longer prefers tabs", on)
	if got.Op != OpDelete {
		t.Fatalf("op = %s, want DELETE (%s)", got.Op, got.Why)
	}
	if got.Target != on[0].ID {
		t.Errorf("target = %q, want %q", got.Target, on[0].ID)
	}
	if got.Text != "" {
		t.Errorf("DELETE should store nothing, got %q", got.Text)
	}
}

func TestDecideRetractsUnstructuredFact(t *testing.T) {
	on := entries("the nightly backup job runs at three in the morning")
	got := Decide("the nightly backup job no longer runs at three in the morning", on)
	if got.Op != OpDelete {
		t.Fatalf("op = %s, want DELETE (%s)", got.Op, got.Why)
	}
}

func TestDecideNeverTouchesImmutableEntries(t *testing.T) {
	on := entries("user prefers tabs")
	on[0].Immutable = true
	if got := Decide("user prefers spaces", on); got.Op != OpAdd {
		t.Fatalf("immutable entry was superseded: %s (%s)", got.Op, got.Why)
	}
	if got := Decide("user no longer prefers tabs", on); got.Op == OpDelete {
		t.Fatalf("immutable entry was retracted: %s (%s)", got.Op, got.Why)
	}
}

func TestDecideIgnoresAlreadySupersededEntries(t *testing.T) {
	// Superseding a fact that was already replaced would rewrite history: the
	// live belief would keep pointing at a dead one.
	on := entries("user prefers spaces", "user prefers tabs")
	on[0].SupersededBy = on[1].ID
	got := Decide("user prefers two-space indentation", on)
	if got.Op != OpUpdate {
		t.Fatalf("op = %s, want UPDATE (%s)", got.Op, got.Why)
	}
	if got.Target != on[1].ID {
		t.Errorf("superseded the dead entry %q instead of the live one %q",
			got.Target, on[1].ID)
	}
}

func TestDecideOnEmptyMemory(t *testing.T) {
	if got := Decide("first fact ever", nil); got.Op != OpAdd {
		t.Fatalf("op = %s, want ADD", got.Op)
	}
	if got := Decide("   ", entries("something")); got.Op != OpNoop {
		t.Fatalf("empty fact: op = %s, want NOOP", got.Op)
	}
}

func TestDecideDoesNotConflateDifferentSubjects(t *testing.T) {
	on := entries("priya prefers tabs")
	got := Decide("marco prefers spaces", on)
	if got.Op != OpAdd {
		t.Fatalf("two people's preferences were merged: %s (%s)", got.Op, got.Why)
	}
}

func TestDecideDoesNotConflateDifferentPredicates(t *testing.T) {
	on := entries("the user lives in bozeman")
	got := Decide("the user works at bozeman brewing", on)
	if got.Op != OpAdd {
		t.Fatalf("different predicates merged: %s (%s)", got.Op, got.Why)
	}
}

func TestAttribute(t *testing.T) {
	cases := []struct {
		in                   string
		subject, pred, value string
		ok                   bool
	}{
		{"user prefers tabs", "user", "prefers", "tabs", true},
		{"Jeremiah works at Montana State", "jeremiah", "works at", "montana state", true},
		{"the server is a proxmox node", "server", "is", "proxmox node", true},
		{"just some prose with no verb", "", "", "", false},
		{"prefers tabs", "", "", "", false},     // no subject
		{"the user prefers", "", "", "", false}, // no value
	}
	for _, c := range cases {
		s, p, v, ok := Attribute(c.in)
		if ok != c.ok || s != c.subject || p != c.pred || v != c.value {
			t.Errorf("Attribute(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				c.in, s, p, v, ok, c.subject, c.pred, c.value, c.ok)
		}
	}
}

func TestAttributePrefersTheLongerPredicate(t *testing.T) {
	// "works at" contains no "is", but "is called" contains "is": the longer
	// match has to win or the value becomes "called ...".
	_, pred, value, ok := Attribute("the box is called aiserver")
	if !ok || pred != "is called" || value != "aiserver" {
		t.Fatalf("got pred=%q value=%q ok=%v", pred, value, ok)
	}
}

func TestIndexWordRespectsBoundaries(t *testing.T) {
	if indexWord("this is fine", "is") != 5 {
		t.Errorf("matched inside 'this' instead of the standalone word")
	}
	if indexWord("thistle", "is") != -1 {
		t.Error("matched a substring with no boundaries")
	}
}

func TestNormalizeAndSimilarity(t *testing.T) {
	if Normalize("  User PREFERS   tabs!! ") != "user prefers tabs" {
		t.Errorf("normalize = %q", Normalize("  User PREFERS   tabs!! "))
	}
	if s := Similarity("user prefers tabs", "user prefers tabs"); s != 1 {
		t.Errorf("identical similarity = %v, want 1", s)
	}
	if s := Similarity("user prefers tabs", "the cat is orange"); s > 0.1 {
		t.Errorf("unrelated similarity = %v, want ~0", s)
	}
	if s := Similarity("", "anything"); s != 0 {
		t.Errorf("empty similarity = %v, want 0", s)
	}
}

func TestSimilarityKeepsNegationWords(t *testing.T) {
	// If "not" were a stopword these two would be identical, and a retraction
	// would dedupe against the thing it retracts.
	if s := Similarity("the disk is failing", "the disk is not failing"); s >= 1 {
		t.Errorf("negation was normalized away: similarity = %v", s)
	}
}

func TestExtractFacts(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"user prefers tabs", []string{"user prefers tabs"}},
		{"User prefers tabs. The server runs proxmox.",
			[]string{"User prefers tabs", "The server runs proxmox"}},
		{"one fact\ntwo fact", []string{"one fact", "two fact"}},
		{"- bulleted fact\n- another one", []string{"bulleted fact", "another one"}},
		// Conjunctions are NOT split: "and spaces" alone loses its subject.
		{"the user prefers tabs and dislikes spaces",
			[]string{"the user prefers tabs and dislikes spaces"}},
		{"   ", nil},
	}
	for _, c := range cases {
		got := ExtractFacts(c.in)
		if len(got) != len(c.want) {
			t.Errorf("ExtractFacts(%q) = %q, want %q", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("ExtractFacts(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestExtractFactsKeepsDecimalsAndPathsIntact(t *testing.T) {
	// A naive split on "." shatters versions and file names, and a shattered
	// fact is unrecallable.
	got := ExtractFacts("the box runs grimoire v2.4.7 from /usr/local/bin/grimoire")
	if len(got) != 1 {
		t.Fatalf("split a single fact into %d: %q", len(got), got)
	}
}

func TestDecideIgnoresArticleDifferences(t *testing.T) {
	on := entries("the user prefers dark mode")
	got := Decide("user prefers light mode", on)
	if got.Op != OpUpdate {
		t.Fatalf("an article blocked reconciliation: %s (%s)", got.Op, got.Why)
	}
}

func TestDecideKeepsPossessivesApart(t *testing.T) {
	// Dropping "my"/"her" the way articles are dropped would merge two
	// people's facts into one belief.
	on := entries("my cat is named marmalade")
	got := Decide("her cat is named biscuit", on)
	if got.Op != OpAdd {
		t.Fatalf("possessive was stripped, merging two subjects: %s (%s)", got.Op, got.Why)
	}
}
