package rerank

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"sort"
	"testing"
	"time"
)

// The memory-gate cross-encoder (benchmarks/memory_use/round2/gatemodel_*.py)
// is the MS MARCO MiniLM architecture fine-tuned on Jev's verdicts. These
// tests need the fine-tuned model: set GRIMOIRE_GATE_MODEL_DIR to its
// directory. The reference scores come from the HF implementation on 20
// synthetic pairs (gatemodel_fixture.py).

func gateModelDir(t testing.TB) string {
	t.Helper()
	dir := os.Getenv("GRIMOIRE_GATE_MODEL_DIR")
	if dir == "" || !complete(dir) {
		t.Skip("set GRIMOIRE_GATE_MODEL_DIR to the fine-tuned gate model directory to run")
	}
	return dir
}

func TestGateModelMatchesReference(t *testing.T) {
	l, err := LoadLocal(gateModelDir(t), LocalConfig{MaxLen: 192})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/gate_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Cases []refCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	var worst float64
	for _, c := range ref.Cases {
		got, err := l.Score(context.Background(), c.Query, []string{c.Doc})
		if err != nil {
			t.Fatal(err)
		}
		d := math.Abs(float64(got[0]) - c.Score)
		worst = math.Max(worst, d)
		if d > parityTolerance {
			t.Errorf("%.30q / %.30q: got %.6f want %.6f", c.Query, c.Doc, got[0], c.Score)
		}
	}
	t.Logf("max |Go - HF| = %.3g over %d pairs", worst, len(ref.Cases))
}

// TestGateRound2 scores the round-2 band pairs in Go (GRIMOIRE_GATE_EVAL_JSON,
// the eval_distilled_model.json written by gatemodel_eval.py): it checks the
// Go scores equal Python's and measures Go latency for 1, 3 and 5 candidates.
func TestGateRound2(t *testing.T) {
	dir := gateModelDir(t)
	f := os.Getenv("GRIMOIRE_GATE_EVAL_JSON")
	if f == "" {
		t.Skip("set GRIMOIRE_GATE_EVAL_JSON for the round-2 check")
	}
	raw, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	var rows2 []struct {
		Prompt string  `json:"prompt"`
		Mem    string  `json:"mem"`
		Kind   string  `json:"kind"`
		S      float64 `json:"s"`
	}
	if err := json.Unmarshal(raw, &rows2); err != nil {
		t.Fatal(err)
	}
	l, err := LoadLocal(dir, LocalConfig{MaxLen: 192})
	if err != nil {
		t.Fatal(err)
	}
	var worst float64
	for _, r := range rows2 {
		got, err := l.Score(context.Background(), r.Prompt, []string{r.Mem})
		if err != nil {
			t.Fatal(err)
		}
		worst = math.Max(worst, math.Abs(float64(got[0])-r.S))
	}
	t.Logf("round-2 pairs %d: max |Go - Python| = %.3g", len(rows2), worst)
	if worst > 5e-3 {
		t.Errorf("Go and Python scores diverge by %.3g", worst)
	}
	for _, n := range []int{1, 3, 5} {
		var ms []float64
		for i := 0; i+n <= len(rows2) && len(ms) < 100; i += n {
			docs := make([]string, n)
			for k := range docs {
				docs[k] = rows2[i+k].Mem
			}
			t0 := time.Now()
			if _, err := l.Score(context.Background(), rows2[i].Prompt, docs); err != nil {
				t.Fatal(err)
			}
			ms = append(ms, float64(time.Since(t0).Microseconds())/1000)
		}
		sort.Float64s(ms)
		t.Logf("%d candidates: p50 %.1f ms  p95 %.1f ms  (n=%d)", n, ms[len(ms)/2], ms[len(ms)*95/100], len(ms))
	}
}
