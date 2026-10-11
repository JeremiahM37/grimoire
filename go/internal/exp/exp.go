// Package exp names experimental behaviours that are off unless asked for.
//
// A ranking or write-path idea that has not yet earned its place ships behind
// a name here, so it can be measured alone and in combination with the others
// on the same build, and removed without touching its neighbours. A behaviour
// graduates by losing its flag, not by having its default flipped quietly.
//
// GRIMOIRE_EXP is a comma-separated list of names, read once at start:
//
//	GRIMOIRE_EXP=header,ppr grimoire
package exp

import (
	"os"
	"strings"
	"sync"
)

var (
	once sync.Once
	on   map[string]bool
)

func load() {
	on = map[string]bool{}
	for _, name := range strings.Split(os.Getenv("GRIMOIRE_EXP"), ",") {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			on[name] = true
		}
	}
}

// On reports whether the named experiment was requested.
func On(name string) bool {
	once.Do(load)
	return on[name]
}

// Set turns experiments on or off for a test and returns a function that
// restores the previous state. Tests that call it must not run in parallel.
func Set(names ...string) (restore func()) {
	once.Do(load)
	prev := on
	next := map[string]bool{}
	for _, n := range names {
		next[strings.ToLower(n)] = true
	}
	on = next
	return func() { on = prev }
}
