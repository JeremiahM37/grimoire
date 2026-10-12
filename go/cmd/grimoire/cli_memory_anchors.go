package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// `grimoire memory anchors` and `grimoire memory refs` are clients of the two
// admin routes in internal/api/memory_anchors.go. They print what the server
// says and nothing else: the server is the one that runs the probes.

// memoryAnchorsCmd runs every stored anchor and lists the claims that no longer
// hold. --json prints the server's report unchanged.
func memoryAnchorsCmd(e *env, args []string) int {
	status, raw := e.call("GET", "/api/memory/anchors")
	if status != http.StatusOK {
		return fail("anchors failed: %s", raw)
	}
	if hasFlag(args, "--json") {
		fmt.Println(raw)
		return 0
	}
	var out struct {
		Facts              int            `json:"facts"`
		Events             int            `json:"events"`
		Preferences        int            `json:"preferences"`
		Claims             int            `json:"claims"`
		AnchoredClaims     int            `json:"anchored_claims"`
		AutoAnchoredClaims int            `json:"auto_anchored_claims"`
		Anchors            int            `json:"anchors"`
		Pass               int            `json:"pass"`
		Fail               int            `json:"fail"`
		Error              int            `json:"error"`
		Skipped            int            `json:"skipped"`
		SkippedReasons     map[string]int `json:"skipped_reasons"`
		ByHost             map[string]struct {
			Claims         int `json:"claims"`
			AnchoredClaims int `json:"anchored_claims"`
		} `json:"by_host"`
		Procedures struct {
			Total    int `json:"total"`
			WithSpec int `json:"with_spec"`
			PrePass  int `json:"pre_pass"`
			PreFail  int `json:"pre_fail"`
		} `json:"procedures"`
		Failing []struct {
			ID      string `json:"id"`
			Path    string `json:"path"`
			Anchor  string `json:"anchor"`
			Result  string `json:"result"`
			Message string `json:"message"`
		} `json:"failing"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fail("anchors: unreadable response: %v", err)
	}
	fmt.Printf("facts %d  events %d  preferences %d  claims %d  anchored claims %d\n",
		out.Facts, out.Events, out.Preferences, out.Claims, out.AnchoredClaims)
	fmt.Printf("auto-anchored claims %d\n", out.AutoAnchoredClaims)
	fmt.Printf("anchors %d  pass %d  fail %d  error %d  skipped %d\n",
		out.Anchors, out.Pass, out.Fail, out.Error, out.Skipped)
	if out.Skipped > 0 {
		fmt.Printf("skipped: %s\n", strings.Join(skipReasonLines(out.SkippedReasons), "  "))
	}
	fmt.Printf("procedures %d  with spec %d  pre pass %d  pre fail %d\n",
		out.Procedures.Total, out.Procedures.WithSpec, out.Procedures.PrePass, out.Procedures.PreFail)
	hosts := make([]string, 0, len(out.ByHost))
	for h := range out.ByHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		fmt.Printf("host %s: claims %d  anchored %d\n", h, out.ByHost[h].Claims, out.ByHost[h].AnchoredClaims)
	}
	if len(out.Failing) == 0 {
		return 0
	}
	fmt.Println("\nno longer hold:")
	for _, f := range out.Failing {
		fmt.Printf("  %s  %s  %s  %s  %s\n", f.ID, f.Path, f.Anchor, f.Result, f.Message)
	}
	return 0
}

// memoryRefsCmd lists the references in the vault that no longer resolve.
// --json prints the server's report unchanged.
func memoryRefsCmd(e *env, args []string) int {
	status, raw := e.call("GET", "/api/memory/refs")
	if status != http.StatusOK {
		return fail("refs failed: %s", raw)
	}
	if hasFlag(args, "--json") {
		fmt.Println(raw)
		return 0
	}
	var out struct {
		Notes     int `json:"notes"`
		Refs      int `json:"refs"`
		DeadTotal int `json:"dead_total"`
		Errors    int `json:"errors"`
		Dead      []struct {
			Path string `json:"path"`
			Ref  string `json:"ref"`
		} `json:"dead"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fail("refs: unreadable response: %v", err)
	}
	fmt.Printf("notes %d  refs %d  dead %d  could not check %d\n",
		out.Notes, out.Refs, out.DeadTotal, out.Errors)
	for _, d := range out.Dead {
		fmt.Printf("  %s -> %s\n", d.Path, strings.TrimSpace(d.Ref))
	}
	return 0
}
