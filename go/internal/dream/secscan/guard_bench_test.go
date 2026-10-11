package secscan

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
	"github.com/JeremiahM37/grimoire/go/internal/trust/normalize"
)

// TestGuardScannerFlags is the pattern-scanner half of the `guard` lab
// (benchmarks/injection/guard_eval.py). It is skipped unless the harness sets
// the two env vars. It applies the same injection check the bench uses
// (scanWith + normalize.Expand + hasInjection) to every item, one document per
// item, and writes one JSON line per item: {"id": ..., "scanner": bool}.
//
//	GUARD_SCAN_IN   JSONL of {"id", "text"}
//	GUARD_SCAN_OUT  JSONL output
func TestGuardScannerFlags(t *testing.T) {
	in, out := os.Getenv("GUARD_SCAN_IN"), os.Getenv("GUARD_SCAN_OUT")
	if in == "" || out == "" {
		t.Skip("set GUARD_SCAN_IN and GUARD_SCAN_OUT (run benchmarks/injection/guard_eval.py)")
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
		doc := dream.Doc{Path: "guard/" + it.ID, Body: it.Text, Kind: dream.KindFileMemory}
		flagged := hasInjection(scanWith([]dream.Doc{doc}, nil, normalize.Expand))
		b, _ := json.Marshal(map[string]any{"id": it.ID, "scanner": flagged})
		w.Write(append(b, '\n'))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}
