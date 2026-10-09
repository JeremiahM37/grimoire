package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/agentprofile"
)

// grimoire agent profiles | new NAME
//
// An agent is a JSON profile. These two commands are the whole authoring
// surface: see what exists, and start a new one from the generic template.

func listProfiles(home string) int {
	profiles, errs := agentprofile.All(home)
	for _, p := range profiles {
		var events []string
		for _, e := range agentprofile.Events {
			if p.Hooks.Events[e] != "" {
				events = append(events, e)
			}
		}
		sort.Strings(events)
		state := "not detected"
		switch {
		case p.Hooks.File == "":
			state = "no hooks file (set hooks.file to install)"
		case p.Detected():
			state = "detected"
		}
		fmt.Printf("%-14s %-8s %s\n", p.Name, p.Source, state)
		fmt.Printf("  %s\n", p.Description)
		fmt.Printf("  events: %s; output: %s; mcp: %s\n", orNone(strings.Join(events, ", ")), p.Output,
			orNone(strings.TrimSpace(p.MCP.Format+" "+p.MCP.File)))
	}
	for _, err := range errs {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	fmt.Printf("\nyour profiles: %s\n", agentprofile.UserDir(home))
	if len(errs) > 0 {
		return 1
	}
	return 0
}

func newProfile(pos []string, home string) int {
	if len(pos) != 1 {
		return fail("usage: grimoire agent new NAME")
	}
	name := pos[0]
	text, err := agentprofile.Starter(name)
	if err != nil {
		return fail("%v", err)
	}
	path := filepath.Join(agentprofile.UserDir(home), name+".json")
	if _, err := os.Stat(path); err == nil {
		return fail("%s already exists; edit it, or remove it first", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fail("%v", err)
	}
	if err := writeAtomic(path, text, 0o644); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("wrote %s\n\nEdit it to describe %s (hooks.file, hooks.events, event_fields, actions),\n"+
		"then: grimoire agent install --agent %s --memory\n", path, name, name)
	return 0
}
