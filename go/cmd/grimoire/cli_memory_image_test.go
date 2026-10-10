package main

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePNG(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 3, 3))); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "diagram.png")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// grimoire memory image stores the picture and reports what happened to it,
// including whether it is searchable at all.
func TestMemoryImageCommandStoresAndReports(t *testing.T) {
	vaultDir(t)
	out, code := runCmd(t, "memory", "image", writePNG(t), "--caption", "rack wiring diagram", "--topic", "infra")
	if code != 0 {
		t.Fatalf("memory image = %d: %s", code, out)
	}
	if !strings.Contains(out, "stored ") || !strings.Contains(out, "image/png") ||
		!strings.Contains(out, "caption (stated): rack wiring diagram") {
		t.Fatalf("unexpected report:\n%s", out)
	}
}

func TestMemoryImageCommandWithoutCaptionSaysNotSearchable(t *testing.T) {
	vaultDir(t)
	out, code := runCmd(t, "memory", "image", writePNG(t))
	if code != 0 {
		t.Fatalf("memory image = %d: %s", code, out)
	}
	if !strings.Contains(out, "NOT searchable") {
		t.Fatalf("an uncaptioned picture must say it is not searchable:\n%s", out)
	}
}

func TestMemoryImageCommandRefusesNonImages(t *testing.T) {
	vaultDir(t)
	p := filepath.Join(t.TempDir(), "notes.svg")
	if err := os.WriteFile(p, []byte("<svg/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := runCmd(t, "memory", "image", p, "--caption", "x"); code == 0 {
		t.Fatalf("an svg was stored as a picture: %s", out)
	}
}
