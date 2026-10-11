package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/distill"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// `grimoire memory distill`: select the facts that are safe to bake into a small
// model and write the training and held-out data for it. Read-only: it reads
// every fact from the index and writes only to --out. See docs/PARAMETRIC.md.

const memoryDistillUsage = `  grimoire memory distill --out DIR [--min-age-days N] [--min-uses N]`

func memoryDistillCmd(e *env, args []string) int {
	out := flagOr(args, "--out", "")
	if out == "" {
		return fail("usage: %s", memoryDistillUsage)
	}
	minAge, err := nonNegativeFlag(args, "--min-age-days", distill.DefaultMinAgeDays)
	if err != nil {
		return fail("%v", err)
	}
	minUses, err := nonNegativeFlag(args, "--min-uses", 0)
	if err != nil {
		return fail("%v", err)
	}

	// Every fact, including the hidden ones: the selection must see them to
	// report why it left them out. Nothing here is written back to the index.
	now := vault.Now()
	hits, err := e.index.MemoryEntries(index.MemoryQuery{
		Filter:            index.Filter{IncludePrivate: true, IgnoreACLs: true},
		IncludePrivate:    true,
		IncludeSuperseded: true,
		IncludeExpired:    true,
		Now:               now,
		Limit:             1 << 30,
		ScanLimit:         1 << 30,
	})
	if err != nil {
		return fail("reading memory: %v", err)
	}
	cands := make([]distill.Candidate, 0, len(hits))
	for _, h := range hits {
		cands = append(cands, distill.Candidate{Entry: h.Entry, Note: h.Note, Uses: h.Uses})
	}

	m, err := distill.Run(cands, distill.Options{Now: now, MinAgeDays: minAge, MinUses: minUses}, out)
	if err != nil {
		return fail("distill: %v", err)
	}
	fmt.Printf("selected %d of %d facts (%d excluded)\n", m.Counts.Selected, m.Counts.Candidates, m.Counts.Excluded)
	fmt.Printf("train %d examples (%d abstain), eval %d examples (%d abstain)\n",
		m.Counts.TrainExamples, m.Counts.TrainAbstain, m.Counts.EvalExamples, m.Counts.EvalAbstain)
	fmt.Printf("wrote %s, %s, %s, %s\n", distill.TrainFile, distill.EvalFile, distill.ExclusionsFile, distill.ManifestFile)
	fmt.Printf("content %s\n", m.ContentSHA256[:16])
	return 0
}

// nonNegativeFlag reads an integer flag, rejecting anything that is not a
// whole number of zero or more. A bad threshold must fail loudly: a silently
// ignored --min-age-days 0x would bake in facts nobody meant to admit.
func nonNegativeFlag(args []string, name string, def int) (int, error) {
	v, ok := flagValue(args, name)
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s wants a whole number of zero or more, got %q", name, v)
	}
	return n, nil
}
