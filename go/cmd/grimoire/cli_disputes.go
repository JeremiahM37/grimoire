package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// `grimoire memory disputes` lists the disagreements nobody has settled, with
// both texts and where each came from. `grimoire memory resolve` settles one.
//
// A dispute is settled by a person, and the server refuses an agent caller, so
// running this from an agent's environment (GRIMOIRE_AGENT_NAME set) fails on
// purpose. The CLI does not try to get around that.

// disputeResolutions maps the words a person types to the API's names.
var disputeResolutions = map[string]string{
	"keep":   "keep",
	"accept": "accept_challenger",
	"merge":  "merge",
}

// disputeResolveArgs reads `<id> <keep|accept|merge> [text...]` plus the
// --challenger and --path flags. It is pure so the grammar is testable.
func disputeResolveArgs(args []string) (body map[string]any, usage string, ok bool) {
	const usageLine = "usage: grimoire memory resolve <id> keep|accept|merge [text...] [--challenger ID] [--path P]"
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			i++ // every flag takes a value
			continue
		}
		positional = append(positional, a)
	}
	if len(positional) < 2 {
		return nil, usageLine, false
	}
	res, known := disputeResolutions[strings.ToLower(positional[1])]
	if !known {
		return nil, usageLine, false
	}
	body = map[string]any{"id": positional[0], "resolution": res}
	if res == "merge" {
		text := strings.TrimSpace(strings.Join(positional[2:], " "))
		if text == "" {
			return nil, "merge needs the new fact as text: " + usageLine, false
		}
		body["text"] = text
	}
	if v, ok := flagValue(args, "--challenger"); ok {
		body["challenger"] = v
	}
	if v, ok := flagValue(args, "--path"); ok {
		body["path"] = v
	}
	return body, "", true
}

func memoryDisputesCmd(e *env, args []string) int {
	status, raw := e.call("GET", "/api/memory/disputes")
	if status != http.StatusOK {
		return fail("disputes failed: %s", raw)
	}
	var open []struct {
		ID       string `json:"id"`
		Path     string `json:"path"`
		Disputed struct {
			Text      string   `json:"text"`
			Authority string   `json:"authority"`
			Stamp     string   `json:"stamp"`
			Agent     string   `json:"agent"`
			Evidence  []string `json:"evidence"`
		} `json:"disputed"`
		Challengers []struct {
			ID        string   `json:"id"`
			Text      string   `json:"text"`
			Authority string   `json:"authority"`
			Stamp     string   `json:"stamp"`
			Agent     string   `json:"agent"`
			Evidence  []string `json:"evidence"`
		} `json:"challengers"`
	}
	if err := json.Unmarshal([]byte(raw), &open); err != nil {
		return fail("disputes: unreadable answer: %v", err)
	}
	if len(open) == 0 {
		fmt.Println("no open disputes")
		return 0
	}
	_ = args
	for _, d := range open {
		fmt.Printf("%s  %s\n", d.ID, d.Path)
		fmt.Printf("  kept    (%s, %s · %s): %s\n", d.Disputed.Authority, d.Disputed.Stamp, d.Disputed.Agent, d.Disputed.Text)
		if len(d.Disputed.Evidence) > 0 {
			fmt.Printf("          evidence: %s\n", strings.Join(d.Disputed.Evidence, ", "))
		}
		for _, c := range d.Challengers {
			fmt.Printf("  %s  (%s, %s · %s): %s\n", "contests", c.Authority, c.Stamp, c.Agent, c.Text)
			fmt.Printf("          id %s", c.ID)
			if len(c.Evidence) > 0 {
				fmt.Printf(", evidence: %s", strings.Join(c.Evidence, ", "))
			}
			fmt.Println()
		}
	}
	fmt.Printf("\n%d open. Settle with:\n"+
		"  grimoire memory resolve ID keep                  (the person's fact stands)\n"+
		"  grimoire memory resolve ID accept [--challenger ID]   (the contesting fact wins)\n"+
		"  grimoire memory resolve ID merge \"the new fact\"    (replaces both)\n", len(open))
	return 0
}

func memoryResolveCmd(e *env, args []string) int {
	body, usage, ok := disputeResolveArgs(args)
	if !ok {
		return fail("%s", usage)
	}
	status, raw := e.callBody("POST", "/api/memory/disputes/resolve", body)
	if status != http.StatusOK {
		return fail("resolve failed (%d): %s", status, raw)
	}
	fmt.Println(raw)
	return 0
}
