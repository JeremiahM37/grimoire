package memory

import (
	"strings"
	"testing"
)

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestEntitiesFindsNamesAndIdentifiers(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"Priya runs the deploy", []string{"priya"}},
		{"the box is a Strix Halo machine", []string{"strix halo"}},
		{"config lives at /etc/grimoire/config.yaml", []string{"/etc/grimoire/config.yaml"}},
		{"see [[Deploy Runbook]] for the steps", []string{"deploy runbook"}},
		{"run `grimoire reindex` after upgrading", []string{"grimoire reindex"}},
		{"tagged #homelab by @jeremiah", []string{"homelab", "jeremiah"}},
		{"the API returns JSON", []string{"api", "json"}},
		{"reachable at 100.96.103.31", []string{"100.96.103.31"}},
		{"the host is aiserver.tail878d9e.ts.net", []string{"aiserver.tail878d9e.ts.net"}},
	}
	for _, c := range cases {
		got := Entities(c.text)
		for _, w := range c.want {
			if !has(got, w) {
				t.Errorf("Entities(%q) = %q, missing %q", c.text, got, w)
			}
		}
	}
}

func TestEntitiesDropsSentenceInitialCommonWords(t *testing.T) {
	// Without this every fact is "about" its first word, which matches
	// everything and therefore ranks nothing.
	for _, text := range []string{
		"The user prefers tabs",
		"When the disk fills, alert",
		"Always run the verifier first",
		"This is a note",
	} {
		for _, got := range Entities(text) {
			if got == "the" || got == "when" || got == "always" || got == "this" ||
				got == "user" {
				t.Errorf("Entities(%q) = %q — grammar capital treated as a name", text, got)
			}
		}
	}
}

func TestEntitiesKeepsMultiWordNamesTogether(t *testing.T) {
	got := Entities("Priya Sharma approved the change")
	if !has(got, "priya sharma") {
		t.Fatalf("multi-word name split: %q", got)
	}
	for _, e := range got {
		if e == "sharma" {
			t.Errorf("name fragment leaked as its own entity: %q", got)
		}
	}
}

func TestEntitiesKeepsTailOfAPhraseStartingWithAnArticle(t *testing.T) {
	got := Entities("The Homelab API is documented")
	if !has(got, "homelab api") {
		t.Fatalf("leading article ate the name: %q", got)
	}
}

func TestEntitiesDeduplicates(t *testing.T) {
	got := Entities("Grimoire indexes Grimoire's own notes with `grimoire reindex`")
	seen := map[string]int{}
	for _, e := range got {
		seen[e]++
	}
	for e, n := range seen {
		if n > 1 {
			t.Errorf("entity %q reported %d times: %q", e, n, got)
		}
	}
}

func TestEntitiesOnEmptyAndPlainText(t *testing.T) {
	if got := Entities(""); len(got) != 0 {
		t.Errorf("Entities(\"\") = %q", got)
	}
	if got := Entities("nothing here is capitalized at all"); len(got) != 0 {
		t.Errorf("plain lowercase text produced entities: %q", got)
	}
}

func TestEntitiesIsSorted(t *testing.T) {
	got := Entities("Zeta and Alpha and Mu all shipped")
	if !sortedStrings(got) {
		t.Errorf("entities not sorted: %q", got)
	}
}

func sortedStrings(s []string) bool {
	for i := 1; i < len(s); i++ {
		if strings.Compare(s[i-1], s[i]) > 0 {
			return false
		}
	}
	return true
}

func TestEntityOverlap(t *testing.T) {
	fact := Entities("Priya Sharma runs the deploy on AIServer")
	if got := EntityOverlap(Entities("what did Priya do"), fact); got == 0 {
		t.Errorf("query naming the subject scored 0 (fact entities %q)", fact)
	}
	if got := EntityOverlap(Entities("what about Marco"), fact); got != 0 {
		t.Errorf("unrelated name scored %v", got)
	}
	if got := EntityOverlap(nil, fact); got != 0 {
		t.Errorf("empty query scored %v", got)
	}
	if got := EntityOverlap(Entities("Priya"), nil); got != 0 {
		t.Errorf("empty fact scored %v", got)
	}
}

func TestEntityOverlapIsAsymmetric(t *testing.T) {
	// A one-entity query fully matched by a many-entity fact must score 1: a
	// symmetric measure would divide by the fact's length and rank a short
	// irrelevant fact above a long relevant one.
	fact := Entities("Priya Sharma and Marco Diaz and Ada Lovelace shipped AIServer")
	if got := EntityOverlap([]string{"priya sharma"}, fact); got != 1 {
		t.Errorf("overlap = %v, want 1 (fact entities %q)", got, fact)
	}
}

func TestEntityOverlapMatchesPartOfAName(t *testing.T) {
	fact := Entities("Priya Sharma approved it")
	if got := EntityOverlap([]string{"priya"}, fact); got != 1 {
		t.Errorf("first name did not match full name: %v (%q)", got, fact)
	}
}
