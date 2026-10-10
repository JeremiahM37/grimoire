package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
)

// cmdDream asks the running server for a dream: the sweep over agent memory
// that reports duplicates, broken indexes, expired facts, credentials and
// injected instructions. It goes through the server rather than opening the
// vault itself because the sweep matches stored secret values, and only the
// server holds the unlocked credential vault.
//
// Exit status is 1 when anything high-severity was found, so it works from
// cron and CI the way `grimoire secret scan` does.
func cmdDream(args []string) int {
	f, err := parseBankFlags(args)
	if err != nil {
		return fail("%v", err)
	}
	if f.on["--help"] || f.on["-h"] {
		fmt.Println("usage: grimoire dream [--apply] [--json]")
		fmt.Println()
		fmt.Println("Sweeps memory notes, memory banks and file-based agent memory.")
		fmt.Println("--apply makes the mechanical, reversible fixes (index repair) and")
		fmt.Println("queues due bank consolidations. Needs GRIMOIRE_ADMIN_TOKEN when the")
		fmt.Println("server sets one.")
		return 0
	}
	c := newBankClient(f)
	var rep dream.Report
	if err := c.do("POST", "/api/dream", map[string]any{"apply": f.on["--apply"]}, &rep); err != nil {
		if routeMissing(err) {
			return fail("this server has no dream endpoint; upgrade it")
		}
		return fail("%v", err)
	}
	if f.on["--json"] {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		printDream(&rep)
	}
	for _, x := range rep.Findings {
		if x.Severity == dream.High {
			return 1
		}
	}
	return 0
}

func printDream(rep *dream.Report) {
	counts := map[dream.Severity]int{}
	for _, x := range rep.Findings {
		counts[x.Severity]++
	}
	for _, x := range rep.Findings {
		loc := x.Path
		if x.Line > 0 {
			loc = fmt.Sprintf("%s:%d", x.Path, x.Line)
		}
		line := fmt.Sprintf("%-6s %-20s %s  %s", x.Severity, x.Check, loc, x.Message)
		if x.Excerpt != "" {
			line += "  (" + x.Excerpt + ")"
		}
		fmt.Println(strings.TrimRight(line, " "))
	}
	for _, a := range rep.Applied {
		fmt.Printf("fixed  %s %s line %d\n", a.Kind, a.Path, a.Line)
	}
	for _, h := range rep.Held {
		fmt.Printf("held   %s (%d fix(es)): %s\n", h.Path, h.Fixes, h.Reason)
		for _, l := range h.Lost {
			fmt.Printf("         would stop firing for: %q\n", l)
		}
	}
	for _, a := range rep.Actions {
		fmt.Println("done  ", a)
	}
	fmt.Printf("\n%d documents: %d high, %d medium, %d low, %d info; %d fix(es) applied, %d held by memory replay\n",
		rep.Docs, counts[dream.High], counts[dream.Medium], counts[dream.Low], counts[dream.Info], len(rep.Applied), len(rep.Held))
}
