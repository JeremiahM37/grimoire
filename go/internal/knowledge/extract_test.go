package knowledge

import (
	"errors"
	"strings"
	"testing"
)

func TestExtractTriplesValidatesEvidenceAndParsesFence(t *testing.T) {
	doc := "Ada Lovelace wrote the Analytical Engine notes. Charles Babbage designed the machine."
	triples, err := ExtractTriples(doc, func(prompt string) (string, error) {
		if !strings.Contains(prompt, "DOCUMENT DATA") {
			t.Fatal("prompt did not identify document as data")
		}
		return "```json\n[{\"subject\":\"Ada Lovelace\",\"relation\":\"wrote\",\"object\":\"Analytical Engine notes\",\"quote\":\"Ada Lovelace wrote the Analytical Engine notes.\"},{\"subject\":\"Invented\",\"relation\":\"knows\",\"object\":\"Nobody\",\"quote\":\"not in document\"}]\n```", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(triples) != 1 || triples[0].Subject != "Ada Lovelace" {
		t.Fatalf("unexpected triples: %#v", triples)
	}
}

func TestExtractTriplesDeduplicatesAndRejectsHallucinatedFields(t *testing.T) {
	doc := "The API stores notes in SQLite."
	triples, err := ExtractTriples(doc, func(string) (string, error) {
		return `[{"subject":"the api","relation":"stores","object":"SQLite","quote":"The API stores notes in SQLite."},{"subject":"the api","relation":"stores","object":"SQLite","quote":"The API stores notes in SQLite."},{"subject":"API","relation":"uses","object":"Postgres","quote":"The API stores notes in SQLite."}]`, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(triples) != 1 || triples[0].Quote != "The API stores notes in SQLite." {
		t.Fatalf("unexpected triples: %#v", triples)
	}
}

func TestExtractionErrorsAreHonest(t *testing.T) {
	for _, tc := range []struct{ name, response string }{
		{"malformed", "not json"},
		{"oversized", strings.Repeat("x", maxCompletionOutput+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ExtractTriples("Alice met Bob.", func(string) (string, error) { return tc.response, nil }); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if _, err := ExtractTriples("Alice met Bob.", func(string) (string, error) { return "", errors.New("backend down") }); err == nil {
		t.Fatal("expected completion error")
	}
}

func TestExpandQueryBoundsAndDeduplicates(t *testing.T) {
	got, err := ExpandQuery("how to configure SQLite", func(prompt string) (string, error) {
		if !strings.Contains(prompt, "QUESTION DATA") {
			t.Fatal("prompt did not identify question as data")
		}
		return `["how to configure SQLite","SQLite setup","how to configure SQLite"]`, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"how to configure SQLite", "SQLite setup"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestExpandQueryMalformedAndNil(t *testing.T) {
	if got, err := ExpandQuery("SQLite setup", nil); err != nil || len(got) != 1 || got[0] != "SQLite setup" {
		t.Fatalf("nil completion: %#v %v", got, err)
	}
	if _, err := ExpandQuery("SQLite setup", func(string) (string, error) { return "{}", nil }); err == nil {
		t.Fatal("expected malformed output error")
	}
	if _, err := ExpandQuery("SQLite setup", func(string) (string, error) { return "", errors.New("backend down") }); err == nil {
		t.Fatal("expected completion error")
	}
}

func TestExtractionRejectsUnrelatedQuote(t *testing.T) {
	text := "Alice owns Atlas. Bob reviews Zephyr."
	triples, err := ExtractTriples(text, func(string) (string, error) {
		return `[{"subject":"Alice","relation":"owns","object":"Zephyr","quote":"Bob reviews Zephyr."}]`, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(triples) != 0 {
		t.Fatal("a quote about a different subject was accepted as evidence")
	}
}

func TestEmptyJSONFenceReturnsError(t *testing.T) {
	_, err := ExtractTriples("Alice owns Atlas.", func(string) (string, error) {
		return "```\njson\n```", nil
	})
	if err == nil {
		t.Fatal("empty JSON fence was accepted")
	}
}
