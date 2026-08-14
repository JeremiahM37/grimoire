package crdt

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/compat"
)

type crdtFixture struct {
	Base  int `json:"base"`
	Cases []struct {
		Name      string          `json:"name"`
		Site      string          `json:"site"`
		Edits     []string        `json:"edits"`
		FinalText string          `json:"final_text"`
		JSON      json.RawMessage `json:"json"`
	} `json:"cases"`
	MergeCases []struct {
		Name       string          `json:"name"`
		AJSON      json.RawMessage `json:"a_json"`
		BJSON      json.RawMessage `json:"b_json"`
		MergedText string          `json:"merged_text"`
	} `json:"merge_cases"`
	KeyBetweenCases []struct {
		Left    []int `json:"left"`
		Right   []int `json:"right"`
		Between []int `json:"between"`
	} `json:"key_between_cases"`
}

func load(t *testing.T) crdtFixture {
	t.Helper()
	var fx crdtFixture
	compat.Load(t, "crdt.json", &fx)
	return fx
}

// normalize compares JSON semantically — field order and escaping are not
// meaningful, the values are.
func normalize(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	return v
}

func TestBaseMatches(t *testing.T) {
	if fx := load(t); fx.Base != Base {
		t.Errorf("Base: fixture=%d go=%d", fx.Base, Base)
	}
}

func TestKeyBetweenMatchesPython(t *testing.T) {
	for _, c := range load(t).KeyBetweenCases {
		got := KeyBetween(c.Left, c.Right)
		if !reflect.DeepEqual(got, c.Between) {
			t.Errorf("KeyBetween(%v, %v) = %v, want %v", c.Left, c.Right, got, c.Between)
		}
	}
}

// The strongest statement of compatibility: replaying the same edit sequence
// must produce the same atom identifiers, not merely the same text. That only
// holds if the diff decomposition matches Python's SequenceMatcher.
func TestEditSequencesProduceIdenticalDocuments(t *testing.T) {
	for _, c := range load(t).Cases {
		doc := New(c.Site)
		for _, e := range c.Edits {
			doc.LocalEdit(e)
		}
		if got := doc.Text(); got != c.FinalText {
			t.Errorf("%s: text = %q, want %q", c.Name, got, c.FinalText)
			continue
		}
		got := normalize(t, []byte(doc.ToJSON()))
		want := normalize(t, c.JSON)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: serialized document differs from Python\n got: %s\nwant: %s",
				c.Name, doc.ToJSON(), string(c.JSON))
		}
	}
}

func TestFromJSONLoadsPythonDocuments(t *testing.T) {
	for _, c := range load(t).Cases {
		doc, err := FromJSON(string(c.JSON), "reader")
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		if got := doc.Text(); got != c.FinalText {
			t.Errorf("%s: loaded text = %q, want %q", c.Name, got, c.FinalText)
		}
	}
}

func TestMergeConvergesWithPython(t *testing.T) {
	for _, c := range load(t).MergeCases {
		a, err := FromJSON(string(c.AJSON), "alpha")
		if err != nil {
			t.Fatal(err)
		}
		b, err := FromJSON(string(c.BJSON), "beta")
		if err != nil {
			t.Fatal(err)
		}
		if got := a.Merge(b).Text(); got != c.MergedText {
			t.Errorf("%s: merged text = %q, want %q", c.Name, got, c.MergedText)
		}
	}
}

// The CRDT join must be commutative and idempotent, or replicas diverge
// depending on the order edits happen to arrive.
func TestMergeIsCommutativeAndIdempotent(t *testing.T) {
	base := FromText("shared base", "alpha")
	a, _ := FromJSON(base.ToJSON(), "alpha")
	b, _ := FromJSON(base.ToJSON(), "beta")
	a.LocalEdit("shared base plus alpha")
	b.LocalEdit("beta prefix, shared base")

	ab, _ := FromJSON(a.ToJSON(), "x")
	bCopy, _ := FromJSON(b.ToJSON(), "y")
	ab.Merge(bCopy)

	ba, _ := FromJSON(b.ToJSON(), "y")
	aCopy, _ := FromJSON(a.ToJSON(), "x")
	ba.Merge(aCopy)

	if ab.Text() != ba.Text() {
		t.Errorf("merge is not commutative:\n a∪b = %q\n b∪a = %q", ab.Text(), ba.Text())
	}

	before := ab.Text()
	again, _ := FromJSON(b.ToJSON(), "y")
	if got := ab.Merge(again).Text(); got != before {
		t.Errorf("merge is not idempotent: %q became %q", before, got)
	}
}

func TestSerializationIsCanonical(t *testing.T) {
	doc := New("site-with-a-string")
	doc.LocalEdit("hello world")
	doc.LocalEdit("hello brave world")
	doc.LocalEdit("hello brave")

	first := doc.ToJSON()
	for i := 0; i < 20; i++ {
		reloaded, err := FromJSON(first, "site-with-a-string")
		if err != nil {
			t.Fatal(err)
		}
		if got := reloaded.ToJSON(); got != first {
			t.Fatalf("round trip %d changed the bytes:\n got: %s\nwant: %s", i, got, first)
		}
	}
}

func TestPythonStringEscaping(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", `"plain"`},
		{"quote\"and\\slash", `"quote\"and\\slash"`},
		{"tab\tnl\n", `"tab\tnl\n"`},
		{"\u00dc", `"\u00dc"`}, // ensure_ascii=True turns non-ASCII into \uXXXX
		{"\u65e5", `"\u65e5"`},
		{"\U0001f510", `"\ud83d\udd10"`}, // astral plane -> UTF-16 surrogate pair
		{"<b>&</b>", `"<b>&</b>"`},       // Python does NOT html-escape; Go's encoder would
		{"\u0001", `"\u0001"`},
	} {
		if got := pyJSONString(tc.in); got != tc.want {
			t.Errorf("pyJSONString(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
