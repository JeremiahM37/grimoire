package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every subcommand, asked for --help, must print usage and exit 0 without
// touching the vault, the index, the network or the home directory.
func TestEveryCommandHelpHasNoSideEffects(t *testing.T) {
	home, vaultDir := t.TempDir(), filepath.Join(t.TempDir(), "vault")
	if err := os.Mkdir(vaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Reserve a port number, then free it: nothing may listen on it afterwards.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	t.Setenv("HOME", home)
	t.Setenv("GRIMOIRE_VAULT", vaultDir)
	t.Setenv("GRIMOIRE_PORT", strings.Split(addr, ":")[1])

	names := []string{"serve", "version"}
	for n := range commands() {
		names = append(names, n)
	}
	for _, name := range names {
		for _, argv := range [][]string{{name, "--help"}, {name, "-h"}, {"help", name}} {
			out := captureStdout(t, func() {
				handled, code := runCLI(argv)
				if !handled || code != 0 {
					t.Errorf("%v: handled=%v code=%d, want handled exit 0", argv, handled, code)
				}
			})
			if !strings.Contains(out, "grimoire") || (name != "version" && !strings.Contains(out, "usage") && !strings.Contains(out, name)) {
				t.Errorf("%v: no usage printed: %q", argv, out)
			}
		}
	}
	for _, dir := range []string{vaultDir, home} {
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Errorf("%s was modified by --help: %v", dir, ents)
		}
	}
	if c, err := net.Dial("tcp", addr); err == nil {
		c.Close()
		t.Errorf("something is listening on %s", addr)
	}
}

func TestSeedDemoRefusesUnchosenVault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GRIMOIRE_VAULT", "")
	if code := cmdSeedDemo(nil); code == 0 {
		t.Fatal("seed-demo ran without an explicit vault")
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Fatalf("default vault touched: %v", ents)
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}
