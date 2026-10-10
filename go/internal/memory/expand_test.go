package memory

import (
	"reflect"
	"testing"
)

func TestStemVariantStripsOnlyKnownInflections(t *testing.T) {
	cases := map[string]string{
		"owns": "own", "owned": "own", "deploying": "deploy", "running": "run",
		"stopped": "stop", "dates": "date", "policies": "policy", "replied": "reply",
		// Words the rules must leave alone.
		"class": "class", "glass": "glass", "bus": "bus", "is": "is",
		"proxy": "proxy", "need": "need", "feed": "feed", "call": "call",
	}
	for in, want := range cases {
		if got := StemVariant(in); got != want {
			t.Errorf("StemVariant(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpansionKeywordsDropQuestionWords(t *testing.T) {
	got := ExpansionKeywords("What does the Dana Kim team know about billing?")
	want := []string{"dana", "kim", "team", "billing"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpansionKeywords = %v, want %v", got, want)
	}
}

func TestFirstNameAliasesNeedAUniqueFullName(t *testing.T) {
	names := []string{"priya sharma", "dana kim", "dana lee", "marco"}
	got := FirstNameAliases([]string{"Priya", "Dana", "marco", "the"}, names)
	if got["priya"] != "priya sharma" {
		t.Errorf("priya -> %q, want priya sharma", got["priya"])
	}
	if _, ok := got["dana"]; ok {
		t.Errorf("dana is ambiguous (two full names) and must not alias: %v", got)
	}
	if _, ok := got["marco"]; ok {
		t.Errorf("marco has no full name and must not alias: %v", got)
	}
}
