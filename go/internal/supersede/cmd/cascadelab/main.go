// Command cascadelab is the lab driver for the `cascade` experiment. It is not
// part of the grimoire binary. Subcommands:
//
//	gen    -seed S -n N -out FILE     write labelled pairs as JSONL
//	rules  -in FILE                    rules today vs rules + contested, per split
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/cascade"
	"github.com/JeremiahM37/grimoire/go/internal/decide"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/supersede"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cascadelab gen|rules [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "gen":
		fs := flag.NewFlagSet("gen", flag.ExitOnError)
		seed := fs.Int64("seed", 1, "seed")
		n := fs.Int("n", 60, "pairs per category per split")
		out := fs.String("out", "", "output JSONL")
		fs.Parse(os.Args[2:])
		if *out == "" {
			fatal("need -out")
		}
		if err := writePairs(*out, supersede.Generate(*seed, *n)); err != nil {
			fatal(err.Error())
		}
	case "rules":
		fs := flag.NewFlagSet("rules", flag.ExitOnError)
		in := fs.String("in", "", "pairs JSONL")
		fs.Parse(os.Args[2:])
		pairs, err := readPairs(*in)
		if err != nil {
			fatal(err.Error())
		}
		for _, split := range []string{supersede.Train, supersede.Calib, supersede.Test} {
			var sp []supersede.Pair
			for _, p := range pairs {
				if p.Split == split {
					sp = append(sp, p)
				}
			}
			off := supersede.Tally(sp, func(p supersede.Pair) bool { return supersede.RulesVerdict(p, false).Supersedes() })
			on := supersede.Tally(sp, func(p supersede.Pair) bool { return supersede.RulesVerdict(p, true).Supersedes() })
			fmt.Printf("== split %s (n=%d)\n", split, len(sp))
			for i := range off {
				fmt.Printf("%-20s n=%3d keep=%3d sup=%3d  rules false=%.3f miss=%.3f  | +contested false=%.3f miss=%.3f\n",
					off[i].Category, off[i].N, off[i].NKeep, off[i].NSup,
					off[i].FalseRate, off[i].MissRate, on[i].FalseRate, on[i].MissRate)
			}
		}
	case "population":
		// The gate population: pairs the rules would supersede that are
		// contested, i.e. the ones the cascade decides. Writes them with their
		// generator label (replace = the truth is supersede).
		fs := flag.NewFlagSet("population", flag.ExitOnError)
		in := fs.String("in", "", "pairs JSONL")
		out := fs.String("out", "", "gate population JSONL")
		fs.Parse(os.Args[2:])
		pairs, err := readPairs(*in)
		if err != nil {
			fatal(err.Error())
		}
		var gate []supersede.Pair
		counts := map[string][2]int{}
		for _, p := range pairs {
			v := supersede.RulesVerdict(p, true)
			if !v.Contested {
				continue
			}
			gate = append(gate, p)
			c := counts[p.Split]
			if p.Supersede {
				c[0]++
			} else {
				c[1]++
			}
			counts[p.Split] = c
		}
		for _, sp := range []string{supersede.Train, supersede.Calib, supersede.Test} {
			fmt.Printf("%-5s gate population: replace=%d distinct=%d\n", sp, counts[sp][0], counts[sp][1])
		}
		if *out != "" {
			if err := writePairs(*out, gate); err != nil {
				fatal(err.Error())
			}
		}
	case "jev":
		// Label the gate population with Jev through the ledger. The key comes
		// from JEV_API_KEY (exported by the wrapper from ~/.ai-secrets; never
		// printed here).
		fs := flag.NewFlagSet("jev", flag.ExitOnError)
		in := fs.String("in", "", "gate population JSONL")
		cache := fs.String("cache", "", "answer cache JSONL")
		ledgerPath := fs.String("ledger", "", "ledger JSONL")
		base := fs.String("base", "https://api.typesafe.ai", "Jev base URL")
		model := fs.String("model", "jev-latest", "model name sent to the server")
		workers := fs.Int("workers", 4, "parallel calls")
		limit := fs.Int("limit", 0, "label at most this many uncached pairs (0 = all)")
		fs.Parse(os.Args[2:])
		pairs, err := readPairs(*in)
		if err != nil {
			fatal(err.Error())
		}
		key := os.Getenv("JEV_API_KEY")
		if key == "" && *ledgerPath != "" {
			fatal("JEV_API_KEY is not set")
		}
		var led *Ledger
		if *ledgerPath != "" {
			if led, err = OpenLedger(*ledgerPath); err != nil {
				fatal(err.Error())
			}
		}
		if *limit > 0 {
			pairs = pairs[:min(*limit, len(pairs))]
		}
		client := &decide.Client{BaseURL: *base, Model: *model, APIKey: key,
			HTTP: &http.Client{Timeout: 45 * time.Second}}
		done, failed, err := runJev(client, pairs, *cache, led, *workers)
		fmt.Printf("jev: labelled=%d failed=%d ledger=%d/%d\n", done, failed, led.Count(), jevCap)
		if err != nil {
			fatal(err.Error())
		}
	case "score":
		// Score cached gate answers on the calibration and test splits. Platt
		// is fitted on calib and applied to both; the fitted constants are
		// printed so they can be written down.
		fs := flag.NewFlagSet("score", flag.ExitOnError)
		in := fs.String("in", "", "gate population JSONL")
		cache := fs.String("cache", "", "answer cache JSONL")
		fs.Parse(os.Args[2:])
		pairs, err := readPairs(*in)
		if err != nil {
			fatal(err.Error())
		}
		ans, err := loadCache(*cache)
		if err != nil {
			fatal(err.Error())
		}
		yOf := func(ps []supersede.Pair) []bool {
			y := make([]bool, len(ps))
			for i, p := range ps {
				y[i] = p.Supersede
			}
			return y
		}
		cp, craw := labelled(pairs, ans, supersede.Calib)
		tp, traw := labelled(pairs, ans, supersede.Test)
		platt := FitPlatt(craw, yOf(cp))
		fmt.Printf("platt fitted on calib: a=%.3f b=%.3f\n", platt.A, platt.B)
		for _, sp := range []struct {
			name string
			ps   []supersede.Pair
			raw  []float64
		}{{"calib", cp, craw}, {"test", tp, traw}} {
			m := computeMetrics(sp.raw, yOf(sp.ps))
			printMetrics(sp.name+" raw", m)
			cal := make([]float64, len(sp.raw))
			for i, q := range sp.raw {
				cal[i] = platt.Apply(q)
			}
			mc := computeMetrics(cal, yOf(sp.ps))
			printMetrics(sp.name+" calibrated", mc)
			printReliability(mc)
			byCat := map[string][]int{}
			for i, p := range sp.ps {
				byCat[p.Category] = append(byCat[p.Category], i)
			}
			for _, c := range supersede.Categories {
				idx := byCat[c]
				if len(idx) == 0 {
					continue
				}
				var cr []float64
				var cy []bool
				for _, i := range idx {
					cr = append(cr, cal[i])
					cy = append(cy, sp.ps[i].Supersede)
				}
				printMetrics("  "+c, computeMetrics(cr, cy))
			}
		}
	case "laya-data":
		// Training rows for the Laya head, from the TRAIN split of the gate
		// population: one with generator labels, one with Jev labels.
		fs := flag.NewFlagSet("laya-data", flag.ExitOnError)
		in := fs.String("in", "", "gate population JSONL")
		cache := fs.String("cache", "", "Jev answer cache JSONL")
		outTruth := fs.String("out-truth", "", "rows labelled by the generator")
		outJev := fs.String("out-jev", "", "rows labelled by Jev (p>=0.5 is replace)")
		fs.Parse(os.Args[2:])
		pairs, err := readPairs(*in)
		if err != nil {
			fatal(err.Error())
		}
		ans, err := loadCache(*cache)
		if err != nil {
			fatal(err.Error())
		}
		var truthRows, jevRows []map[string]any
		for _, p := range pairs {
			if p.Split != supersede.Train {
				continue
			}
			row := func(label string) map[string]any {
				return map[string]any{"state": gateState(p),
					"questions": map[string]any{"replace": map[string]any{"type": "choice",
						"instructions": gateQuestion["replace"].Instructions,
						"criteria":     gateQuestion["replace"].Criteria}},
					"expected": map[string]any{"replace": label}}
			}
			tl := "distinct"
			if p.Supersede {
				tl = "replace"
			}
			truthRows = append(truthRows, row(tl))
			jl := "distinct"
			if a, ok := ans[p.ID]; ok && a.Replace >= 0.5 {
				jl = "replace"
			}
			jevRows = append(jevRows, row(jl))
		}
		if err := writeRows(*outTruth, truthRows); err != nil {
			fatal(err.Error())
		}
		if err := writeRows(*outJev, jevRows); err != nil {
			fatal(err.Error())
		}
		fmt.Printf("laya-data: %d train rows each\n", len(truthRows))
	case "cascade":
		fs := flag.NewFlagSet("cascade", flag.ExitOnError)
		in := fs.String("in", "", "pairs JSONL (full population)")
		layaCache := fs.String("laya", "", "Laya answers JSONL")
		jevCache := fs.String("jev", "", "Jev answers JSONL")
		fs.Parse(os.Args[2:])
		pairs, err := readPairs(*in)
		if err != nil {
			fatal(err.Error())
		}
		la, err := loadCache(*layaCache)
		if err != nil {
			fatal(err.Error())
		}
		ja, err := loadCache(*jevCache)
		if err != nil {
			fatal(err.Error())
		}
		// Caches are keyed by pair id; the gate's askers are keyed by state.
		// Walk the pairs in file order, not the cache map, so that when two
		// pairs share a statement the same answer is kept on every run.
		layaBy, jevBy := cacheAsker{}, cacheAsker{}
		for _, p := range pairs {
			if a, ok := la[p.ID]; ok {
				if _, dup := layaBy[cascadeState(p)]; !dup {
					layaBy[cascadeState(p)] = a.Replace
				}
			}
			if a, ok := ja[p.ID]; ok {
				if _, dup := jevBy[cascadeState(p)]; !dup {
					jevBy[cascadeState(p)] = a.Replace
				}
			}
		}
		cascadeCmd(pairs, layaBy, jevBy)
	case "realverdicts":
		// Counts only: for each private pair, the rule verdicts with the flag
		// off and on, and the label. No statement text is printed.
		fs := flag.NewFlagSet("realverdicts", flag.ExitOnError)
		in := fs.String("in", "", "private pairs JSONL")
		fs.Parse(os.Args[2:])
		pairs, err := readPairs(*in)
		if err != nil {
			fatal(err.Error())
		}
		for _, p := range pairs {
			off := supersede.RulesVerdict(p, false)
			on := supersede.RulesVerdict(p, true)
			fmt.Printf("%s label_replace=%v rules_off=%s rules_on=%s contested=%v contested_raw=%v\n",
				p.ID, p.Supersede, off.Op, on.Op, on.Contested, memory.Contested(p.New, p.Old))
		}
	case "realgate":
		// Counts only, for the private check. The calibrations and band are the
		// ones fitted and chosen on the calibration split (see the cascade
		// subcommand); nothing here is tuned on the private pairs.
		fs := flag.NewFlagSet("realgate", flag.ExitOnError)
		in := fs.String("in", "", "private contested pairs JSONL")
		layaCache := fs.String("laya", "", "Laya answers JSONL")
		jevCache := fs.String("jev", "", "Jev answers JSONL")
		fs.Parse(os.Args[2:])
		pairs, err := readPairs(*in)
		if err != nil {
			fatal(err.Error())
		}
		la, _ := loadCache(*layaCache)
		ja, _ := loadCache(*jevCache)
		layaBy, jevBy := cacheAsker{}, cacheAsker{}
		for _, p := range pairs {
			if a, ok := la[p.ID]; ok {
				layaBy[cascadeState(p)] = a.Replace
			}
			if a, ok := ja[p.ID]; ok {
				jevBy[cascadeState(p)] = a.Replace
			}
		}
		lcal := cascade.Platt{A: 0.812, B: -0.710}
		jcal := cascade.Platt{A: 1.876, B: -0.304}
		arms := map[string]cascade.Gate{
			"(c) +Jev only":  {Jev: jevBy, JevCal: jcal},
			"(d) +Laya only": {Laya: layaBy, LayaCal: lcal, Lo: 0.5, Hi: 0.5},
			"(e) cascade":    {Laya: layaBy, LayaCal: lcal, Jev: jevBy, JevCal: jcal, Lo: 0.05, Hi: 0.65},
		}
		for _, name := range []string{"(c) +Jev only", "(d) +Laya only", "(e) cascade"} {
			g := arms[name]
			prevented, blocked, distinct, replace := 0, 0, 0, 0
			for _, p := range pairs {
				v := g.Decide(context.Background(), p.Old, p.New)
				if p.Supersede {
					replace++
					if !v.Replace {
						blocked++
					}
				} else {
					distinct++
					if !v.Replace {
						prevented++
					}
				}
			}
			fmt.Printf("%-16s distinct kept (false supersessions prevented)=%d/%d  replace kept (true updates wrongly blocked)=%d/%d\n",
				name, prevented, distinct, blocked, replace)
		}
	case "latency":
		// Sequential single-call latency of one decision server on the first N
		// test-split gate pairs. Jev calls are booked to the ledger.
		fs := flag.NewFlagSet("latency", flag.ExitOnError)
		in := fs.String("in", "", "gate population JSONL")
		base := fs.String("base", "", "server base URL")
		model := fs.String("model", "jev-latest", "model name")
		n := fs.Int("n", 100, "calls")
		ledgerPath := fs.String("ledger", "", "ledger JSONL (Jev only)")
		fs.Parse(os.Args[2:])
		pairs, err := readPairs(*in)
		if err != nil {
			fatal(err.Error())
		}
		var led *Ledger
		if *ledgerPath != "" {
			if led, err = OpenLedger(*ledgerPath); err != nil {
				fatal(err.Error())
			}
		}
		key := os.Getenv("JEV_API_KEY")
		c := &decide.Client{BaseURL: *base, Model: *model, APIKey: key,
			HTTP: &http.Client{Timeout: 45 * time.Second}}
		var ms []float64
		for _, p := range pairs {
			if p.Split != supersede.Test || len(ms) >= *n {
				continue
			}
			if err := led.Reserve(p.ID, p.Split, "latency"); err != nil {
				fatal(err.Error())
			}
			t0 := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			_, err := askGate(ctx, c, p)
			cancel()
			if err != nil {
				fatal(err.Error())
			}
			ms = append(ms, float64(time.Since(t0).Microseconds())/1000)
		}
		sort.Float64s(ms)
		fmt.Printf("latency %s n=%d p50=%.0fms p90=%.0fms max=%.0fms ledger=%d\n", *base+*model, len(ms),
			ms[len(ms)/2], ms[int(float64(len(ms))*0.9)], ms[len(ms)-1], led.Count())
	default:
		fatal("unknown subcommand " + os.Args[1])
	}
}

func writePairs(path string, pairs []supersede.Pair) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, p := range pairs {
		b, _ := json.Marshal(p)
		w.Write(b)
		w.WriteByte('\n')
	}
	return w.Flush()
}

func readPairs(path string) ([]supersede.Pair, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []supersede.Pair
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var p supersede.Pair
		if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, sc.Err()
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func writeRows(path string, rows []map[string]any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, r := range rows {
		b, _ := json.Marshal(r)
		w.Write(b)
		w.WriteByte('\n')
	}
	return w.Flush()
}

func cascadeState(p supersede.Pair) string { return cascade.State(p.Old, p.New) }
