package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Backup and restore, and the reason they are here rather than left to the
// operator's own tar command.
//
// "The vault is plain files and the index is rebuildable, so recovery is
// restore-and-reindex" was true and untested — which is the same as untrue when
// it matters. What makes it non-obvious is what must NOT be in the archive and
// what must: the index is disposable and large, so it is skipped; the sealed
// secret store and the CRDT state are small, not reproducible from the notes,
// and are the two things a restored vault is silently missing if a naive `tar`
// of the notes directory is all that was kept.
//
// The archive is a plain .tar.gz of the vault. Not a format: the files are the
// backup, and anyone can open it with tools they already have — including
// twenty years from now, which is the point of keeping notes in markdown.

// skipDuringBackup are the parts of .grimoire that a restore rebuilds.
var skipDuringBackup = []string{"index.db", "index.db-wal", "index.db-shm", "models"}

func cmdBackup(args []string) int {
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	out := "grimoire-backup-" + time.Now().UTC().Format("20060102-150405") + ".tar.gz"
	if v, ok := flagValue(args, "--out"); ok {
		out = v
	}
	f, err := os.Create(out)
	if err != nil {
		return fail("%v", err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	notes, other := 0, 0
	root := e.vault.Root

	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		for _, skip := range skipDuringBackup {
			if strings.HasPrefix(rel, ".grimoire/"+skip) {
				return nil // rebuildable: see skipDuringBackup
			}
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil // an unreadable file must not abort the backup
		}
		hdr := &tar.Header{Name: rel, Mode: int64(info.Mode().Perm()),
			Size: int64(len(body)), ModTime: info.ModTime(), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(body); err != nil {
			return err
		}
		if strings.HasSuffix(rel, ".md") {
			notes++
		} else {
			other++
		}
		return nil
	})
	if walkErr != nil {
		return fail("%v", walkErr)
	}
	if err := tw.Close(); err != nil {
		return fail("%v", err)
	}
	if err := gz.Close(); err != nil {
		return fail("%v", err)
	}
	size, _ := f.Stat()
	fmt.Printf("wrote %s — %d notes, %d other files, %.1f MB\n",
		out, notes, other, float64(size.Size())/(1<<20))
	fmt.Println("the index is not included; a restore rebuilds it")
	return 0
}

func cmdRestore(args []string) int {
	rest, force := flagOut(args, "--force")
	if len(rest) == 0 {
		return fail("usage: grimoire restore ARCHIVE.tar.gz [--force]")
	}
	target := envOr("GRIMOIRE_VAULT", filepath.Join(os.Getenv("HOME"), "notes"))
	if v, ok := flagValue(rest, "--into"); ok {
		target = v
	}

	// Restoring over notes that are already there is how a backup turns into
	// data loss. It has to be asked for.
	if entries, err := os.ReadDir(target); err == nil && len(entries) > 0 && !force {
		return fail("%s is not empty — restore into an empty directory, or pass --force", target)
	}

	f, err := os.Open(rest[0])
	if err != nil {
		return fail("%v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fail("%s is not a gzip archive: %v", rest[0], err)
	}
	defer gz.Close()

	v, err := vault.New(target)
	if err != nil {
		return fail("%v", err)
	}
	tr := tar.NewReader(gz)
	files := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail("reading the archive: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// The archive is data, and an archive that can write outside the
		// directory it is restored into is the oldest trick there is.
		clean := filepath.Clean(filepath.Join(v.Root, filepath.FromSlash(hdr.Name)))
		if clean != v.Root && !strings.HasPrefix(clean, v.Root+string(filepath.Separator)) {
			return fail("the archive contains a path outside the vault: %q", hdr.Name)
		}
		if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
			return fail("%v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return fail("%v", err)
		}
		mode := os.FileMode(hdr.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(clean, body, mode); err != nil {
			return fail("%v", err)
		}
		files++
	}

	fmt.Printf("restored %d files into %s\n", files, v.Root)
	fmt.Println("rebuilding the index…")
	e, err := newEnv(false)
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	n, err := e.index.Reindex()
	if err != nil {
		return fail("%v", err)
	}
	if err := e.index.RecordSignature(); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("indexed %d notes — the vault is ready\n", n)
	return 0
}
