package trust

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
)

// TestSpotlightOverheadFences writes the fenced form of each benign text, once
// with the flag off and once with it on, so the token cost of datamarking can
// be counted with the model's own tokenizer (benchmarks/injection/
// guard_overhead.py). Skipped unless the harness sets the two env vars.
//
//	GUARD_OVERHEAD_IN   JSONL of {"id", "text"}
//	GUARD_OVERHEAD_OUT  JSONL of {"id", "off", "on"} (fenced strings)
func TestSpotlightOverheadFences(t *testing.T) {
	in, out := os.Getenv("GUARD_OVERHEAD_IN"), os.Getenv("GUARD_OVERHEAD_OUT")
	if in == "" || out == "" {
		t.Skip("set GUARD_OVERHEAD_IN and GUARD_OVERHEAD_OUT (run benchmarks/injection/guard_overhead.py)")
	}
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	o, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	w := bufio.NewWriter(o)
	defer w.Flush()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var it struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(sc.Bytes(), &it); err != nil {
			t.Fatal(err)
		}
		restore := exp.Set()
		off := Fence(1, "slack #ops", it.Text)
		restore()
		restore = exp.Set("guard")
		on := Fence(1, "slack #ops", it.Text)
		restore()
		b, _ := json.Marshal(map[string]string{"id": it.ID, "off": off, "on": on})
		w.Write(append(b, '\n'))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}
