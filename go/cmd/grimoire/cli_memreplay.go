package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/replay"
)

const replayUsage = `usage:
  grimoire memory replay                          corpus size and settings
  grimoire memory replay --diff --note PATH --with FILE   replay a note becoming FILE
  grimoire memory replay --diff --change FILE.json        replay {notes,edits,remove,merge,remap}
  grimoire memory replay --diff --index [--dir STORE]     replay a regenerated MEMORY.md
  grimoire memory replay seed FILE.json                   load seed cases: {"cases":[...]} or the
                                                          memory-use benchmark's cases.json
  add --json for the raw report, --brief for counts only

Replays past context requests (docs/MEMORY_REPLAY.md) against the store as it
is and as it would be, and lists the useful recalls the change would lose.`

func memoryReplayCmd(e *env, args []string) int {
	if len(args) > 0 && args[0] == "seed" {
		if len(args) < 2 {
			return fail("usage: grimoire memory replay seed FILE.json")
		}
		raw, err := os.ReadFile(args[1])
		if err != nil {
			return fail("%v", err)
		}
		var body any
		if json.Unmarshal(raw, &body) != nil {
			return fail("%s is not JSON", args[1])
		}
		// The benchmark's own case file (a JSON array) is accepted as is.
		if _, isList := body.([]any); isList {
			seeds, err := replay.SeedsFromBenchmark(raw)
			if err != nil {
				return fail("%v", err)
			}
			cases := make([]map[string]any, 0, len(seeds))
			for _, sd := range seeds {
				cases = append(cases, map[string]any{"text": sd.Text, "stage": sd.Stage, "min_rel": sd.MinRel,
					"limit": sd.Limit, "budget": sd.Budget, "expect": sd.Expect, "avoid": sd.Avoid})
			}
			body = map[string]any{"cases": cases}
		}
		status, out := e.callBody("POST", "/api/memory/replay/seed", body)
		if status != http.StatusOK {
			return fail("seed failed: %s", out)
		}
		fmt.Println(out)
		return 0
	}
	if !hasFlag(args, "--diff") {
		status, out := e.call("GET", "/api/memory/replay")
		if status != http.StatusOK {
			return fail("replay failed: %s", out)
		}
		fmt.Println(out)
		return 0
	}
	var body map[string]any
	switch {
	case flagOr(args, "--change", "") != "":
		raw, err := os.ReadFile(flagOr(args, "--change", ""))
		if err != nil {
			return fail("%v", err)
		}
		if json.Unmarshal(raw, &body) != nil {
			return fail("--change file is not a JSON object")
		}
	case flagOr(args, "--note", "") != "":
		raw, err := os.ReadFile(flagOr(args, "--with", ""))
		if err != nil {
			return fail("--with: %v", err)
		}
		body = map[string]any{"notes": []map[string]string{{"path": flagOr(args, "--note", ""), "text": string(raw)}}}
	case hasFlag(args, "--index"):
		path, text, err := regeneratedIndex(e, args)
		if err != nil {
			return fail("%v", err)
		}
		body = map[string]any{"notes": []map[string]string{{"path": path, "text": text}}}
	default:
		return fail("%s", replayUsage)
	}
	status, out := e.callBody("POST", "/api/memory/replay", body)
	if status != http.StatusOK {
		return fail("replay failed: %s", out)
	}
	if hasFlag(args, "--json") {
		fmt.Println(out)
		return 0
	}
	var res struct {
		Verdict struct {
			Checked bool   `json:"checked"`
			Hold    bool   `json:"hold"`
			Reason  string `json:"reason"`
		} `json:"verdict"`
		Report *replay.Report `json:"report"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return fail("%v", err)
	}
	fmt.Print(replayText(res.Report, res.Verdict.Checked, res.Verdict.Hold, res.Verdict.Reason, hasFlag(args, "--brief")))
	if res.Verdict.Hold {
		return 2
	}
	return 0
}

// regeneratedIndex builds the MEMORY.md `memory index` would write.
func regeneratedIndex(e *env, args []string) (path, text string, err error) {
	store := e.storeDir(args)
	text, err = buildIndexText(e, store, args)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(store, "MEMORY.md"), text, nil
}

// replayText renders a report for a terminal.
func replayText(rep *replay.Report, checked, hold bool, reason string, brief bool) string {
	if rep == nil {
		return "no situations on file yet; nothing to replay against\n" + reasonLine(reason)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "replayed %d situation(s) in %d ms: %d with a useful recall on file\n", rep.Situations, rep.Millis, rep.Checked)
	fmt.Fprintf(&b, "  lost %d useful recall(s) in %d situation(s); gained %d (%d new false fire(s)); moved %d; retained %.1f%%\n",
		rep.LostMemories, rep.LostSituations, rep.Gained, rep.NewFalseFires, rep.Moved, 100*rep.Score)
	switch {
	case !checked:
		b.WriteString("  NOT CHECKED: " + reason + "\n")
	case hold:
		b.WriteString("  HOLD: " + reason + "\n")
	default:
		b.WriteString("  ok\n")
	}
	if brief {
		return b.String()
	}
	shown := 0
	for _, d := range rep.Diffs {
		if len(d.Lost) == 0 && len(d.FalseFire) == 0 {
			continue
		}
		if shown++; shown > 25 {
			b.WriteString("  ...\n")
			break
		}
		fmt.Fprintf(&b, "\n  [%s] %q\n", d.Stage, d.Text)
		for _, m := range d.Lost {
			fmt.Fprintf(&b, "    lost    %s (%s, relevance %.2f -> %.2f, weight %.1f)\n", m.Target, m.Why, m.Before, m.After, m.Weight)
		}
		for _, t := range d.FalseFire {
			fmt.Fprintf(&b, "    noise   %s now fires where nothing useful did\n", t)
		}
	}
	return b.String()
}

func reasonLine(r string) string {
	if r == "" {
		return ""
	}
	return r + "\n"
}
