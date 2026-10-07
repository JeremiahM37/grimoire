package rerank

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"
)

// The parity fixture was produced by the HF reference implementation
// (transformers BertForSequenceClassification, fp32, fast tokenizer with
// truncation=True) for pairs chosen to cover accents, CJK, emoji, control
// characters, literal special tokens, empty sides and every truncation path.
// The model itself is not committed: set GRIMOIRE_RERANK_MODEL_DIR to a
// directory holding config.json, tokenizer.json and model.safetensors.

type refCase struct {
	Query        string  `json:"query"`
	Doc          string  `json:"doc"`
	MaxLen       int     `json:"max_len"`
	InputIDs     []int32 `json:"input_ids"`
	TokenTypeIDs []int32 `json:"token_type_ids"`
	Score        float64 `json:"score"`
}

const parityTolerance = 1e-3

func loadReference(t testing.TB) []refCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Cases []refCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	return ref.Cases
}

func testModelDir(t testing.TB) string {
	t.Helper()
	dir := os.Getenv("GRIMOIRE_RERANK_MODEL_DIR")
	if dir == "" || !complete(dir) {
		t.Skip("set GRIMOIRE_RERANK_MODEL_DIR to a cross-encoder/ms-marco-MiniLM-L-6-v2 snapshot to run")
	}
	return dir
}

func TestTokenizerMatchesReference(t *testing.T) {
	dir := testModelDir(t)
	tok, err := loadPairTokenizer(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range loadReference(t) {
		ids, types := tok.encodePair(c.Query, c.Doc, c.MaxLen)
		if !equalIDs(ids, c.InputIDs) {
			t.Errorf("case %d (%.30q): ids\n got %v\nwant %v", i, c.Query, ids, c.InputIDs)
		}
		if !equalIDs(types, c.TokenTypeIDs) {
			t.Errorf("case %d (%.30q): token types differ", i, c.Query)
		}
	}
}

func TestScoresMatchReference(t *testing.T) {
	dir := testModelDir(t)
	cases := loadReference(t)
	// one batch per max length, so batching and packing are exercised too
	byLen := map[int][]int{}
	for i, c := range cases {
		byLen[c.MaxLen] = append(byLen[c.MaxLen], i)
	}
	var worst float64
	for maxLen, idx := range byLen {
		for _, cfg := range []LocalConfig{
			{MaxLen: maxLen},
			{MaxLen: maxLen, Threads: 1, BatchTokens: 64}, // many small batches
		} {
			l, err := LoadLocal(dir, cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range uniqueQueries(cases, idx) {
				var docs []string
				var want []float64
				for _, i := range idx {
					if cases[i].Query == q {
						docs = append(docs, cases[i].Doc)
						want = append(want, cases[i].Score)
					}
				}
				got, err := l.Score(context.Background(), q, docs)
				if err != nil {
					t.Fatal(err)
				}
				for k := range got {
					d := math.Abs(float64(got[k]) - want[k])
					worst = math.Max(worst, d)
					if d > parityTolerance {
						t.Errorf("max_len %d %.30q / %.30q: got %.6f want %.6f (diff %.2g)",
							maxLen, q, docs[k], got[k], want[k], d)
					}
				}
			}
		}
	}
	t.Logf("max |logit - reference| = %.3g over %d cases", worst, len(cases))
}

func uniqueQueries(cases []refCase, idx []int) []string {
	seen := map[string]bool{}
	var out []string
	for _, i := range idx {
		if !seen[cases[i].Query] {
			seen[cases[i].Query] = true
			out = append(out, cases[i].Query)
		}
	}
	return out
}

func equalIDs(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBenchDocsAreTheAdvertisedLength(t *testing.T) {
	dir := testModelDir(t)
	tok, err := loadPairTokenizer(dir)
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := tok.encodePair("how are the cluster backups verified", benchDocs(1, 52)[0], DefaultMaxLen)
	t.Logf("typical pair = %d tokens", len(ids))
	if len(ids) < 60 || len(ids) > 80 {
		t.Errorf("typical bench pair is %d tokens", len(ids))
	}
}
