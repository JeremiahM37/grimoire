package fts

import "testing"

// The invariant these protect: user input must never reach FTS as syntax.
// Before this package the same rule was hand-rolled at three call sites with
// two different conventions, so a fix at one was not a fix at the others.

func TestOperatorsAreNeutralized(t *testing.T) {
	// each of these is FTS5 syntax if it arrives unquoted
	for _, hostile := range []string{
		"OR", "AND", "NOT", "NEAR", "foo*", "^start", "a:b", "(x)", `"`, `""`,
	} {
		if got := Phrase(hostile); got[0] != '"' || got[len(got)-1] != '"' {
			t.Errorf("Phrase(%q) = %q — not wrapped", hostile, got)
		}
		for _, got := range []string{Terms(hostile), PrefixTerms([]string{hostile}, And)} {
			if got == hostile {
				t.Errorf("%q passed through unquoted", hostile)
			}
		}
	}
}

func TestPhraseDoublesQuotes(t *testing.T) {
	// FTS5 escapes a literal quote by doubling it, as SQL string literals do
	if got, want := Phrase(`say "hi"`), `"say ""hi"""`; got != want {
		t.Errorf("Phrase = %q, want %q", got, want)
	}
	if got, want := Phrase(""), `""`; got != want {
		t.Errorf("Phrase(empty) = %q, want %q", got, want)
	}
}

func TestTermsQuotesEachWord(t *testing.T) {
	// implicit AND with each word free to appear anywhere — a single phrase was
	// too strict for recall, missing any query whose words weren't adjacent
	if got, want := Terms("deploy vpn"), `"deploy" "vpn"`; got != want {
		t.Errorf("Terms = %q, want %q", got, want)
	}
	if got, want := Terms(`a "b`), `"a" """b"`; got != want {
		t.Errorf("Terms = %q, want %q", got, want)
	}
	if got := Terms("   "); got != "" {
		t.Errorf("Terms(blank) = %q, want empty", got)
	}
}

func TestPrefixTermsMatchSearchAsYouType(t *testing.T) {
	if got, want := PrefixTerms([]string{"gate", "way"}, And), `"gate"* "way"*`; got != want {
		t.Errorf("PrefixTerms = %q, want %q", got, want)
	}
	if got, want := PrefixTerms([]string{"a", "b"}, Or), `"a"* OR "b"*`; got != want {
		t.Errorf("PrefixTerms(Or) = %q, want %q", got, want)
	}
	// no terms must match nothing rather than error
	if got, want := PrefixTerms(nil, And), `""`; got != want {
		t.Errorf("PrefixTerms(nil) = %q, want %q", got, want)
	}
}

func TestPrefixTermsDropQuotesRatherThanDoubleThem(t *testing.T) {
	// deliberate divergence from Terms: `"""foo"*` is a phrase containing a
	// literal quote and matches nothing, so for a PREFIX token dropping the
	// character is what still finds what the user meant
	if got, want := PrefixTerms([]string{`"foo`}, And), `"foo"*`; got != want {
		t.Errorf("PrefixTerms = %q, want %q", got, want)
	}
}
