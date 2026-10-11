package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// memoryAgentsCmd lists each agent's record of correction signals and the
// conservative reliability derived from it. Admin only, like the endpoint.
//
// The reliability is a lower bound on the share of an agent's corrected facts
// that stood. It measures how often an agent was corrected, so it says nothing
// about whether a fact is true, and an agent with few events shows a value near
// the prior, not a verdict.
func memoryAgentsCmd(e *env, args []string) int {
	status, raw := e.call("GET", "/api/memory/agents/reliability")
	if status != http.StatusOK {
		return fail("agents failed: %s", raw)
	}
	var out struct {
		Agents []struct {
			Agent       string  `json:"agent"`
			Confirmed   int     `json:"confirmed"`
			Challenged  int     `json:"challenged"`
			Superseded  int     `json:"superseded_by_human"`
			Deleted     int     `json:"deleted"`
			Mean        float64 `json:"mean"`
			Reliability float64 `json:"reliability"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fail("agents: unreadable response: %v", err)
	}
	if len(out.Agents) == 0 {
		fmt.Println("no agent has a correction signal yet")
		return 0
	}
	fmt.Printf("%-20s %9s %10s %11s %8s %6s %11s\n",
		"AGENT", "CONFIRMED", "CHALLENGED", "SUPERSEDED", "DELETED", "MEAN", "RELIABILITY")
	for _, a := range out.Agents {
		fmt.Printf("%-20s %9d %10d %11d %8d %6.2f %11.2f\n",
			a.Agent, a.Confirmed, a.Challenged, a.Superseded, a.Deleted, a.Mean, a.Reliability)
	}
	fmt.Println("\nreliability is a 95% lower bound from correction events; it is not a measure of truth")
	return 0
}
