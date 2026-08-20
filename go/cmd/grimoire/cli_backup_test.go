package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// inArchive reports whether a path is present in the backup.
func inArchive(t *testing.T, archive, want string) bool {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == want {
			return true
		}
	}
}

// "The vault is plain files and the index is rebuildable, so recovery is
// restore-and-reindex." That was a claim with nothing behind it. This is the
// round trip: back up a working vault, restore it somewhere else, and check
// that what comes back is the same vault — including the parts a naive tar of
// the notes directory would silently miss.

func TestBackupRestoreRoundTrip(t *testing.T) {
	source := t.TempDir()
	restored := filepath.Join(t.TempDir(), "restored")

	// A vault with notes, a private note, an attachment, and a sealed secret
	// store — the last is the part that cannot be rebuilt from the notes.
	t.Setenv("GRIMOIRE_VAULT", source)
	t.Setenv("GRIMOIRE_LOCAL_EMBED", "off")
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(source, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("runbook.md", "# Runbook\n\nrollback with force-recreate\n")
	write("team/onboarding.md", "# Onboarding\n\nread the runbook first\n")
	write("private.md", "---\nprivate: true\n---\n\n# Private\n\nkestrel\n")
	write("attachments/diagram.png", "\x89PNG\r\n\x1a\nfake")
	write(".grimoire/secrets.json", `{"salt":"c2FsdA","verifier":"dg","kdf":"argon2id","secrets":"enc"}`)

	if code := cmdReindex(nil); code != 0 {
		t.Fatalf("seeding the index failed: %d", code)
	}

	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if code := cmdBackup([]string{"--out", archive}); code != 0 {
		t.Fatalf("backup failed: %d", code)
	}
	info, err := os.Stat(archive)
	if err != nil || info.Size() == 0 {
		t.Fatalf("no archive written: %v", err)
	}

	// Restore into a DIFFERENT directory, the way a recovery actually happens.
	t.Setenv("GRIMOIRE_VAULT", restored)
	if code := cmdRestore([]string{archive, "--into", restored}); code != 0 {
		t.Fatalf("restore failed: %d", code)
	}

	for _, rel := range []string{
		"runbook.md", "team/onboarding.md", "private.md",
		"attachments/diagram.png",
		// The sealed store: small, not reproducible from the notes, and the
		// thing a backup of *.md alone loses without saying so.
		".grimoire/secrets.json",
	} {
		if _, err := os.Stat(filepath.Join(restored, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s did not survive the round trip", rel)
		}
	}
	// The index is rebuildable and large, so it is not IN the archive — but it
	// exists after a restore, because the restore rebuilds it.
	if inArchive(t, archive, ".grimoire/index.db") {
		t.Error("the archive carries the index, which a restore rebuilds anyway")
	}
	if _, err := os.Stat(filepath.Join(restored, ".grimoire", "index.db")); err != nil {
		t.Error("the index was not rebuilt after the restore")
	}

	// And the restored vault answers: the notes are indexed and searchable.
	e, err := openEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()
	n, err := e.index.DB.Count("SELECT count(*) FROM notes")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("restored index holds %d notes, want 3", n)
	}
	hits, err := e.index.Retrieve("force-recreate", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("a restored vault returns nothing for text that is in it")
	}
}

// Restoring over a vault that already has notes is how a backup becomes data
// loss, so it has to be asked for explicitly.
func TestRestoreRefusesToOverwriteWithoutForce(t *testing.T) {
	source := t.TempDir()
	t.Setenv("GRIMOIRE_VAULT", source)
	t.Setenv("GRIMOIRE_LOCAL_EMBED", "off")
	if err := os.WriteFile(filepath.Join(source, "a.md"), []byte("# A"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	if code := cmdBackup([]string{"--out", archive}); code != 0 {
		t.Fatal("backup failed")
	}

	occupied := t.TempDir()
	if err := os.WriteFile(filepath.Join(occupied, "mine.md"), []byte("# Mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := cmdRestore([]string{archive, "--into", occupied}); code == 0 {
		t.Fatal("restored over an occupied vault without --force")
	}
	if _, err := os.Stat(filepath.Join(occupied, "mine.md")); err != nil {
		t.Fatal("the refused restore destroyed the notes that were there")
	}
}
