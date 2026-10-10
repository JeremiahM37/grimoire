package replay

import (
	"encoding/json"
	"fmt"
	"strings"
)

// BenchTarget maps a memory-use benchmark memory id (benchmarks/memory_use) to
// the target an injection logs: am:<file>#n is a note under Agent Memory/,
// cm:<key> an instruction note, gm:<id> a stored fact.
func BenchTarget(memID string) string {
	store, key, _ := strings.Cut(memID, ":")
	switch store {
	case "am":
		return "note:Agent Memory/" + strings.SplitN(key, "#", 2)[0] + ".md"
	case "cm":
		return "note:Instructions/cm_" + key + ".md"
	}
	return "fact:" + key
}

// SeedsFromBenchmark turns the benchmark's case file (a JSON array of
// {mem_id, kind, prompt, tool, input}) into seed situations: a positive case
// must fire its memory, a near-miss must not. Prompts are sent as the user
// request; an action case is "<tool> <input>" at the action stage with the
// hook's stricter floor, item limit and budget.
func SeedsFromBenchmark(raw []byte) ([]Seed, error) {
	var cases []struct {
		MemID  string `json:"mem_id"`
		Kind   string `json:"kind"`
		Prompt string `json:"prompt"`
		Tool   string `json:"tool"`
		Input  string `json:"input"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		return nil, fmt.Errorf("benchmark cases: %w", err)
	}
	var out []Seed
	for _, c := range cases {
		if c.MemID == "" {
			continue
		}
		sd := Seed{Text: c.Prompt}
		target := BenchTarget(c.MemID)
		switch c.Kind {
		case "pos_prompt":
			sd.Expect = []string{target}
		case "pos_action":
			sd.Text, sd.Stage = strings.TrimSpace(c.Tool+" "+c.Input), "action"
			sd.MinRel, sd.Limit, sd.Budget = 0.7, 2, 1200
			sd.Expect = []string{target}
		case "neg_prompt":
			sd.Avoid = []string{target}
		default:
			continue
		}
		out = append(out, sd)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no benchmark cases found")
	}
	return out, nil
}
