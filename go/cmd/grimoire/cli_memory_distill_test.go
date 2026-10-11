package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// writeFact appends one bullet to a memory note, minted the way the write path
// mints ids, so the fact reads back as a person's assertion or an agent's.
func writeFact(t *testing.T, dir, topic string, e memory.Entry) {
	t.Helper()
	e.ID = memory.DeriveID(e.Stamp, e.Agent, e.Text)
	path := filepath.Join(dir, "memory", topic+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	prev, _ := os.ReadFile(path)
	body := memory.Append(string(prev), e)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// vaultHashes digests every note in the vault, so a test can prove the command
// left the memory alone. The .grimoire directory is skipped: it holds the
// derived index, which the first command run creates and is not memory.
func vaultHashes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".grimoire" {
				return filepath.SkipDir
			}
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		out[p] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMemoryDistillSelectsAndLeavesVaultUnchanged(t *testing.T) {
	dir := vaultDir(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)
	old := vault.Now
	vault.Now = func() time.Time { return now }
	t.Cleanup(func() { vault.Now = old })

	ago := func(days int) string { return now.AddDate(0, 0, -days).Format(memory.StampFormat) }
	writeFact(t, dir, "prefs", memory.Entry{Text: "the user prefers tabs", Stamp: ago(60), Agent: "claude-code", Category: "preference", Human: true})
	writeFact(t, dir, "prefs", memory.Entry{Text: "the user keeps a cat named Miso", Stamp: ago(60), Agent: "claude-code", Category: "fact", Human: true, Visibility: memory.VisPrivate})
	writeFact(t, dir, "ops", memory.Entry{Text: "the deploy window is fridays", Stamp: ago(2), Agent: "claude-code", Category: "fact", Human: true})

	// Warm the index first so its one-time creation is not counted as a change.
	runCmd(t, "memory", "distill", "--out", t.TempDir())
	before := vaultHashes(t, dir)
	out := filepath.Join(t.TempDir(), "distill")
	stdout, code := runCmd(t, "memory", "distill", "--out", out)
	if code != 0 {
		t.Fatalf("distill = %d: %s", code, stdout)
	}
	if !strings.Contains(stdout, "selected 1 of 3 facts (2 excluded)") {
		t.Errorf("summary = %q", stdout)
	}
	if after := vaultHashes(t, dir); len(after) != len(before) {
		t.Fatalf("vault file count changed: %d -> %d", len(before), len(after))
	} else {
		for p, h := range before {
			if after[p] != h {
				t.Errorf("distill changed %s", p)
			}
		}
	}

	train, err := os.ReadFile(filepath.Join(out, "train.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(train), "tabs") {
		t.Errorf("selected fact missing from training data:\n%s", train)
	}
	if strings.Contains(string(train), "Miso") {
		t.Errorf("private fact reached training data")
	}
	excl, _ := os.ReadFile(filepath.Join(out, "exclusions.json"))
	if !strings.Contains(string(excl), `"reason": "private"`) || !strings.Contains(string(excl), `"reason": "too_young"`) {
		t.Errorf("exclusions missing reasons:\n%s", excl)
	}
}

func TestMemoryDistillRequiresOutAndSaneThresholds(t *testing.T) {
	vaultDir(t)
	if _, code := runCmd(t, "memory", "distill"); code == 0 {
		t.Error("distill without --out succeeded")
	}
	out := filepath.Join(t.TempDir(), "d")
	if _, code := runCmd(t, "memory", "distill", "--out", out, "--min-age-days", "-3"); code == 0 {
		t.Error("negative --min-age-days accepted")
	}
	if _, code := runCmd(t, "memory", "distill", "--out", out, "--min-uses", "lots"); code == 0 {
		t.Error("non-numeric --min-uses accepted")
	}
}
