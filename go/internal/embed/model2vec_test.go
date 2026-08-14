package embed

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/compat"
)

type embedFixture struct {
	Tolerance float64 `json:"tolerance"`
	Backends  map[string][]struct {
		Text   string    `json:"text"`
		Vector []float64 `json:"vector"`
	} `json:"backends"`
	Tokenizer map[string][]struct {
		Text string  `json:"text"`
		IDs  []int32 `json:"ids"`
	} `json:"tokenizer"`
}

const modelName = "minishlab/potion-base-8M"

// modelDir locates the HuggingFace snapshot the Python side already downloaded.
// Skipping (rather than failing) keeps the suite runnable on a machine without
// the model cached, which is the normal case in CI.
func modelDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("GRIMOIRE_MODEL_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to locate the model cache")
	}
	base := filepath.Join(home, ".cache", "huggingface", "hub",
		"models--minishlab--potion-base-8M", "snapshots")
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) == 0 {
		t.Skipf("model not cached at %s — run the Python side once to fetch it", base)
	}
	return filepath.Join(base, entries[0].Name())
}

func loadModel(t *testing.T) *Model2Vec {
	t.Helper()
	m, err := LoadModel2Vec(modelDir(t), modelName)
	if err != nil {
		t.Fatalf("loading model: %v", err)
	}
	return m
}

// Token ids first: a tokenizer divergence is the likeliest cause of a vector
// mismatch, and this says so directly instead of leaving it to be inferred.
func TestTokenizerMatchesPython(t *testing.T) {
	var fx embedFixture
	compat.Load(t, "embed.json", &fx)
	cases, ok := fx.Tokenizer[modelName]
	if !ok {
		t.Skip("no tokenizer fixtures for this model")
	}
	m := loadModel(t)

	for _, c := range cases {
		// tokenize(), not the raw encoder: the fixture holds StaticModel.tokenize
		// output, which has already had unknown tokens dropped.
		got := m.tokenize(c.Text)
		want := c.IDs
		if len(got) != len(want) {
			t.Errorf("token count for %q: got %d, want %d\n got: %v\nwant: %v",
				truncate(c.Text), len(got), len(want), got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("token %d for %q: got %d, want %d\n got: %v\nwant: %v",
					i, truncate(c.Text), got[i], want[i], got, want)
				break
			}
		}
	}
}

func TestModel2VecVectorsMatchPython(t *testing.T) {
	var fx embedFixture
	compat.Load(t, "embed.json", &fx)
	sig := "model2vec:" + modelName
	cases, ok := fx.Backends[sig]
	if !ok {
		t.Skipf("no %s vectors in fixture", sig)
	}
	m := loadModel(t)
	if m.Signature() != sig {
		t.Fatalf("signature = %q, want %q", m.Signature(), sig)
	}

	for _, c := range cases {
		got := m.Embed([]string{c.Text})[0]
		if len(got) != len(c.Vector) {
			t.Errorf("dim for %q: got %d, want %d", truncate(c.Text), len(got), len(c.Vector))
			continue
		}
		worst, at := 0.0, -1
		for i := range got {
			if d := math.Abs(float64(got[i]) - c.Vector[i]); d > worst {
				worst, at = d, i
			}
		}
		if worst > fx.Tolerance {
			t.Errorf("vector for %q drifted by %g at component %d (tolerance %g)",
				truncate(c.Text), worst, at, fx.Tolerance)
		}
	}
}

// A normalized vector must be unit length — except the all-zero vector an empty
// input produces, which stays zero rather than becoming NaN.
func TestNormalizationInvariants(t *testing.T) {
	m := loadModel(t)
	for _, text := range []string{"a normal sentence", "x", "日本語"} {
		v := m.Embed([]string{text})[0]
		var sum float64
		for _, x := range v {
			sum += float64(x) * float64(x)
		}
		if n := math.Sqrt(sum); math.Abs(n-1) > 1e-5 {
			t.Errorf("norm of %q = %g, want 1", text, n)
		}
	}
	for _, text := range []string{"", "   "} {
		for i, x := range m.Embed([]string{text})[0] {
			if x != 0 || math.IsNaN(float64(x)) {
				t.Errorf("empty input %q produced a non-zero component at %d: %v", text, i, x)
				break
			}
		}
	}
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}
