package bank

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func texts(cs []Chunk) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Text
	}
	return out
}

func TestShortTextIsOneUnmodifiedChunk(t *testing.T) {
	in := "  hello there  \n"
	cs := Chunks(in, 100)
	if len(cs) != 1 || cs[0].Text != in {
		t.Fatalf("got %q", texts(cs))
	}
	if cs[0].Hash != HashChunk(in) || len(cs[0].Hash) != 64 {
		t.Error("hash must be the sha256 of the chunk text")
	}
	if len(Chunks("   \n ", 100)) != 0 {
		t.Error("blank text has no chunks")
	}
}

func TestSeparatorsStayWithTheFollowingPiece(t *testing.T) {
	got := texts(Chunks("Alpha one. Beta two. Gamma three.", 12))
	want := []string{"Alpha one", ". Beta two", ". Gamma", "three."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParagraphsPackGreedily(t *testing.T) {
	in := "para one is here\n\npara two is here\n\npara three here"
	got := texts(Chunks(in, 40))
	want := []string{"para one is here\n\npara two is here", "para three here"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q", got)
	}
}

func longProse(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "Sentence number %d talks about topic %d, briefly; ", i, i%7)
		if i%5 == 4 {
			b.WriteString("\n")
		}
		if i%17 == 16 {
			b.WriteString("\n\n")
		}
	}
	return b.String()
}

func TestChunkingIsBoundedDeterministicAndIdempotent(t *testing.T) {
	in := longProse(20000)
	a, b := Chunks(in, 3000), Chunks(in, 3000)
	if len(a) < 7 {
		t.Fatalf("only %d chunks", len(a))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("chunking is not deterministic")
		}
		if runeLen(a[i].Text) > 3000 {
			t.Errorf("chunk %d has %d chars", i, runeLen(a[i].Text))
		}
		again := Chunks(a[i].Text, 3000)
		if len(again) != 1 || again[0].Text != a[i].Text {
			t.Errorf("chunk %d is not a fixed point of chunking", i)
		}
	}
}

func TestOversizedWordFallsToCharacters(t *testing.T) {
	cs := Chunks(strings.Repeat("x", 25), 10)
	got := texts(cs)
	if len(got) != 3 || got[0] != strings.Repeat("x", 10) || got[2] != "xxxxx" {
		t.Errorf("got %q", got)
	}
}

func TestConversationsSplitOnTurns(t *testing.T) {
	var turns []map[string]string
	for i := 0; i < 60; i++ {
		turns = append(turns, map[string]string{"speaker": "Alice", "text": fmt.Sprintf("message %d %s", i, strings.Repeat("y", 40))})
	}
	raw, _ := json.Marshal(turns)
	cs := Chunks(string(raw), 1000)
	if len(cs) < 3 {
		t.Fatalf("got %d chunks", len(cs))
	}
	total := 0
	for _, c := range cs {
		var part []map[string]string
		if err := json.Unmarshal([]byte(c.Text), &part); err != nil {
			t.Fatalf("chunk is not a JSON array of turns: %v\n%s", err, c.Text)
		}
		if runeLen(c.Text) > 1000 {
			t.Errorf("chunk too long: %d", runeLen(c.Text))
		}
		if part[0]["text"] != fmt.Sprintf("message %d %s", total, strings.Repeat("y", 40)) {
			t.Errorf("turn order lost at %d", total)
		}
		total += len(part)
		// Rechunking a produced chunk returns it.
		if again := Chunks(c.Text, 1000); len(again) != 1 || again[0].Text != c.Text {
			t.Error("conversation chunk is not a fixed point")
		}
	}
	if total != 60 {
		t.Errorf("turns = %d", total)
	}
	if !strings.Contains(renderForModel(cs[0].Text), "Alice: message 0") {
		t.Errorf("render = %q", renderForModel(cs[0].Text))
	}
}

func TestJSONLinesPackByLine(t *testing.T) {
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf(`{"n":%d,"pad":"%s"}`, i, strings.Repeat("z", 30)))
	}
	cs := Chunks(strings.Join(lines, "\n"), 200)
	n := 0
	for _, c := range cs {
		for _, ln := range strings.Split(c.Text, "\n") {
			var v map[string]any
			if err := json.Unmarshal([]byte(ln), &v); err != nil {
				t.Fatalf("a line was cut: %q", ln)
			}
			n++
		}
	}
	if n != 30 {
		t.Errorf("lines = %d", n)
	}
}

func TestChunkIDsRoundTrip(t *testing.T) {
	id := ChunkID("team:a_b", "doc_1~x", 3)
	b, d, i, ok := ParseChunkID(id)
	if !ok || b != "team:a_b" || d != "doc_1~x" || i != 3 {
		t.Errorf("ParseChunkID(%q) = %q %q %d %v", id, b, d, i, ok)
	}
}

func TestBankIDsAndPaths(t *testing.T) {
	for id, ok := range map[string]bool{
		"a": true, "coding-agent:grimoire": true, "x.y_z": true, "a__b": false, "a_:b": false, "Upper": false,
		"": false, "-lead": false, "a..b": false, strings.Repeat("a", 65): false,
	} {
		if ValidID(id) != ok {
			t.Errorf("ValidID(%q) = %v", id, !ok)
		}
	}
	if p := FactsPath("coding-agent:x", "s1"); p != "banks/coding-agent__x/facts/s1.md" {
		t.Errorf("path = %s", p)
	}
	if id, kind := ParsePath("banks/coding-agent__x/facts/s1.md"); id != "coding-agent:x" || kind != FactsKind {
		t.Errorf("ParsePath = %q %v", id, kind)
	}
	if p := DocumentPath("b", "https://x.y/a b?c"); !strings.HasPrefix(p, "banks/b/documents/https-x-y-a-b-c-") {
		t.Errorf("unsafe doc id mapped to %s", p)
	}
	if DocumentPath("b", "A/B") == DocumentPath("b", "A-B") {
		t.Error("two ids that slug alike must not share a file")
	}
}
