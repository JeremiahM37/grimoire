// Package compat loads the cross-language fixtures in compat/fixtures/.
//
// Those files are frozen output from the Python implementation. Every package
// in this port proves itself against them, so "same functionality" is measured
// rather than asserted.
package compat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Dir returns the fixtures directory, located relative to this source file so
// tests work regardless of the working directory they are run from.
func Dir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "..", "compat", "fixtures")
}

// Load unmarshals a fixture file into v, skipping the test if the fixtures have
// not been generated yet.
func Load(t *testing.T, name string, v any) {
	t.Helper()
	p := filepath.Join(Dir(), name)
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		t.Skipf("%s not generated — run: .venv/bin/python compat/generate.py", name)
	}
	if err != nil {
		t.Fatalf("reading %s: %v", p, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("parsing %s: %v", p, err)
	}
}
