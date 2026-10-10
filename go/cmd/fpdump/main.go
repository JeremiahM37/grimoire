// fpdump selects memory fingerprints offline, for the benchmark in
// benchmarks/memory_use/round2/fp_eval.py. It reads
//
//	{"corpus": ["memory text", ...],
//	 "items": [{"id": "...", "text": "...", "situation": "..."}],
//	 "min_score": 2.0, "max_df": 0}
//
// on stdin and writes {"id": [{"token","kind","score","df"}, ...]} on stdout,
// using the same code the server runs at injection time.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/JeremiahM37/grimoire/go/internal/fingerprint"
)

func main() {
	var in struct {
		Corpus   []string `json:"corpus"`
		MinScore float64  `json:"min_score"`
		MaxDF    int      `json:"max_df"`
		Items    []struct {
			ID, Text, Situation string
		} `json:"items"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	df := fingerprint.BuildDF(in.Corpus)
	opt := fingerprint.Options{MinScore: in.MinScore, MaxDF: in.MaxDF}
	out := map[string][]fingerprint.Fingerprint{}
	for _, it := range in.Items {
		fps := fingerprint.Select(it.Text, df, fingerprint.Set(it.Situation), opt)
		if fps == nil {
			fps = []fingerprint.Fingerprint{}
		}
		out[it.ID] = fps
	}
	json.NewEncoder(os.Stdout).Encode(out)
}
