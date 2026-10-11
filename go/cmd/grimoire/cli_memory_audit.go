package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// `grimoire memory audit-supersessions` lists past supersessions that the gate
// now judges distinct, with ids, so a person can review them. Read-only unless
// --restore ID is given. The gate runs on the server, which holds the decision
// settings; this command only asks and prints.
func memoryAuditCmd(e *env, args []string) int {
	if id, ok := flagValue(args, "--restore"); ok {
		path, _ := flagValue(args, "--path")
		status, raw := e.callBody("POST", "/api/memory/supersessions/restore",
			map[string]string{"id": strings.TrimSpace(id), "path": path})
		if status != http.StatusOK {
			return fail("restore failed (%d): %s", status, raw)
		}
		fmt.Println(raw)
		return 0
	}
	status, raw := e.call("GET", "/api/memory/supersessions")
	if status != http.StatusOK {
		return fail("audit failed (%d): %s", status, raw)
	}
	var out struct {
		Supersessions []struct {
			ID       string  `json:"id"`
			Path     string  `json:"path"`
			Old      string  `json:"old"`
			New      string  `json:"new"`
			Source   string  `json:"source"`
			PReplace float64 `json:"p_replace"`
			Judged   bool    `json:"judged_distinct"`
		} `json:"supersessions"`
		Undecided      int  `json:"undecided"`
		GateConfigured bool `json:"gate_configured"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fail("audit: unreadable answer: %v", err)
	}
	if !out.GateConfigured {
		fmt.Println("no decision server is configured: nothing can be judged distinct (set cascade_laya_url or decision_url)")
	}
	n := 0
	for _, s := range out.Supersessions {
		if !s.Judged {
			continue
		}
		n++
		fmt.Printf("%s  %s  (%s, p(replace)=%.2f)\n  was: %s\n  now: %s\n", s.ID, s.Path, s.Source, s.PReplace, s.Old, s.New)
	}
	fmt.Printf("%d judged distinct, %d not decided (no gate answered)\n", n, out.Undecided)
	fmt.Println("restore one with: grimoire memory audit-supersessions --restore ID --path NOTE")
	return 0
}
