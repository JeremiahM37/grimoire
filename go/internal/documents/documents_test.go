package documents

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

type fakeIndex struct{ upserts, removes []string }

func (f *fakeIndex) Upsert(p string) (*vault.Note, error) {
	f.upserts = append(f.upserts, p)
	return nil, nil
}
func (f *fakeIndex) Remove(p string) error { f.removes = append(f.removes, p); return nil }

func TestImportUpdatePreservesManualNote(t *testing.T) {
	v, err := vault.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ix := &fakeIndex{}
	s := New(v, ix)
	r, err := s.ImportBytes("/external/a.txt", "a.txt", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Write(r.Path, "# a\n\nmanual", nil); err != nil {
		t.Fatal(err)
	}
	originalBytes := []byte("first")
	if _, err := s.ImportBytes("/external/a.txt", "a.txt", originalBytes); !errors.Is(err, ErrManualEdit) {
		t.Fatalf("conflict error = %v", err)
	}
	n, err := v.Read(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	if n.Body == "second" || n.Body == "" {
		t.Fatalf("manual body was overwritten: %q", n.Body)
	}
	op, err := s.internalOriginal(".grimoire/document-originals/" + pathID("/external/a.txt") + ".txt")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(op)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != string(originalBytes) {
		t.Fatalf("original changed during conflict: %q", saved)
	}
}

func TestUnchangedImportSkipsUpsert(t *testing.T) {
	v, _ := vault.New(t.TempDir())
	ix := &fakeIndex{}
	s := New(v, ix)
	data := []byte("stable")
	if _, err := s.ImportBytes("/external/stable.txt", "stable.txt", data); err != nil {
		t.Fatal(err)
	}
	count := len(ix.upserts)
	if _, err := s.ImportBytes("/external/stable.txt", "stable.txt", data); err != nil {
		t.Fatal(err)
	}
	if len(ix.upserts) != count {
		t.Fatalf("unchanged import upserts = %d, want %d", len(ix.upserts), count)
	}
}

func TestUnchangedImportRepairsMissingGeneratedNote(t *testing.T) {
	v, _ := vault.New(t.TempDir())
	indexer := &fakeIndex{}
	store := New(v, indexer)
	data := []byte("repair me")
	result, err := store.ImportBytes("/external/repair.txt", "repair.txt", data)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Delete(result.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportBytes("/external/repair.txt", "repair.txt", data); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Read(result.Path); err != nil {
		t.Fatalf("generated note was not repaired: %v", err)
	}
}

func TestInitialTargetCollisionDoesNotOverwriteManualNote(t *testing.T) {
	v, _ := vault.New(t.TempDir())
	store := New(v, &fakeIndex{})
	target := store.TargetPath("/external/collision.txt", "collision.txt")
	if _, err := v.Write(target, "# Manual\n\nkeep", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportBytesAt("/external/collision.txt", "collision.txt", []byte("incoming"), target); !errors.Is(err, ErrPathCollision) {
		t.Fatalf("collision error = %v", err)
	}
	note, err := v.Read(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note.Body, "keep") {
		t.Fatal("manual collision note changed")
	}
}

func TestReplacementExtensionRotatesPreservedOriginal(t *testing.T) {
	v, _ := vault.New(t.TempDir())
	store := New(v, &fakeIndex{})
	result, err := store.ImportBytes("/external/change", "change.txt", []byte("text"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportBytesAt("/external/change", "change.md", []byte("# markdown"), result.Path); err != nil {
		t.Fatal(err)
	}
	oldOriginal := filepath.Join(v.Root, ".grimoire", "document-originals", pathID("/external/change")+".txt")
	newOriginal := filepath.Join(v.Root, ".grimoire", "document-originals", pathID("/external/change")+".md")
	if _, err := os.Stat(oldOriginal); !os.IsNotExist(err) {
		t.Fatalf("old original still exists: %v", err)
	}
	if _, err := os.Stat(newOriginal); err != nil {
		t.Fatalf("new original missing: %v", err)
	}
}

func TestMarkdownFrontmatterIsPreserved(t *testing.T) {
	v, _ := vault.New(t.TempDir())
	s := New(v, &fakeIndex{})
	result, err := s.ImportBytes("/external/meta.md", "meta.md", []byte("---\ntags: [alpha, beta]\nprivate: true\n---\n\n# Heading\n\n[[Related]]\n"))
	if err != nil {
		t.Fatal(err)
	}
	note, err := v.Read(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !note.Private || len(note.Tags) != 2 || !strings.Contains(note.Body, "[[Related]]") {
		t.Fatalf("frontmatter/links lost: private=%v tags=%v body=%q", note.Private, note.Tags, note.Body)
	}
}

func TestCompressedPDFExtraction(t *testing.T) {
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	_, _ = writer.Write([]byte("BT /F1 18 Tf 72 720 Td (Compressed PDF fixture) Tj ET"))
	_ = writer.Close()
	pdf := makePDF(compressed.Bytes())
	text, err := Extract("pdf", pdf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Compressed PDF fixture") {
		t.Fatalf("PDF text = %q", text)
	}
}

func TestDeleteManualMarkerRemovalPreservesImport(t *testing.T) {
	v, _ := vault.New(t.TempDir())
	s := New(v, &fakeIndex{})
	result, err := s.ImportBytes("/external/marked.txt", "marked.txt", []byte("original"))
	if err != nil {
		t.Fatal(err)
	}
	note, _ := v.Read(result.Path)
	metadata := note.Frontmatter.Clone()
	metadata.Delete("document_generated")
	if _, err := v.Write(result.Path, "# Human\n\nkeep", metadata); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSource("/external/marked.txt"); !errors.Is(err, ErrManualEdit) {
		t.Fatalf("delete error = %v", err)
	}
	if _, err := v.Read(result.Path); err != nil {
		t.Fatalf("manual note removed: %v", err)
	}
	originalPath := filepath.Join(v.Root, ".grimoire", "document-originals", pathID("/external/marked.txt")+".txt")
	if _, err := os.Stat(originalPath); err != nil {
		t.Fatalf("original removed: %v", err)
	}
}

func TestWatchRecursesAndReconcilesChanges(t *testing.T) {
	v, _ := vault.New(t.TempDir())
	sourceRoot := t.TempDir()
	ix := &fakeIndex{}
	s := New(v, ix)
	initial := filepath.Join(sourceRoot, "initial.txt")
	if err := os.WriteFile(initial, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	watchDone := make(chan error, 1)
	go func() { watchDone <- s.Watch(ctx, sourceRoot) }()
	waitFor(t, func() bool { records, _ := s.List(); return len(records) == 1 })
	nested := filepath.Join(sourceRoot, "nested", "new.txt")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { records, _ := s.List(); return len(records) == 2 })
	if err := os.WriteFile(nested, []byte("updated"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		records, _ := s.List()
		for _, record := range records {
			if record.Source == nested && record.Hash == hashBytes([]byte("updated")) {
				return true
			}
		}
		return false
	})
	if err := os.Remove(initial); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { records, _ := s.List(); return len(records) == 1 })
	cancel()
	if err := <-watchDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("watch stop = %v", err)
	}
}

func TestWatchReconcilesDeletionAfterRestart(t *testing.T) {
	v, _ := vault.New(t.TempDir())
	sourceRoot := t.TempDir()
	source := filepath.Join(sourceRoot, "restart.txt")
	_ = os.WriteFile(source, []byte("persisted"), 0o644)
	s := New(v, &fakeIndex{})
	firstCtx, firstCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Watch(firstCtx, sourceRoot) }()
	waitFor(t, func() bool { records, _ := s.List(); return len(records) == 1 })
	firstCancel()
	<-done
	_ = os.Remove(source)
	secondCtx, secondCancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() { secondDone <- s.Watch(secondCtx, sourceRoot) }()
	waitFor(t, func() bool { records, _ := s.List(); return len(records) == 0 })
	secondCancel()
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("restart watch stop = %v", err)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition not reached before timeout")
}

func makePDF(stream []byte) []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d /Filter /FlateDecode >>\nstream\n%s\nendstream", len(stream), stream),
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	offsets := []int{0}
	for index, object := range objects {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

func TestImportDeleteOnlyOwnedNoteAndReservedOriginal(t *testing.T) {
	v, _ := vault.New(t.TempDir())
	s := New(v, &fakeIndex{})
	r, err := s.ImportBytes("/external/a.txt", "a.txt", []byte("text"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Write("documents/manual.md", "# manual", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSource("/external/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Read(r.Path); err == nil {
		t.Fatal("owned generated note was not deleted")
	}
	if _, err := v.Read("documents/manual.md"); err != nil {
		t.Fatal("manual note was deleted")
	}
	if entries, err := os.ReadDir(filepath.Join(v.Root, ".grimoire", "document-originals")); err == nil && len(entries) != 0 {
		t.Fatal("preserved original was not deleted")
	}
}

func TestDOCXExtractionAndLimit(t *testing.T) {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, _ := z.Create("word/document.xml")
	_, _ = f.Write([]byte(`<?xml version="1.0"?><document><body><p><r><t>Hello</t></r></p><p><r><t>World</t></r></p></body></document>`))
	_ = z.Close()
	text, err := Extract("docx", b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if text != "Hello\nWorld" {
		t.Fatalf("got %q", text)
	}
}

func TestDOCXExtractionRejectsOversizedXML(t *testing.T) {
	var archive bytes.Buffer
	zipWriter := zip.NewWriter(&archive)
	file, err := zipWriter.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(`<document><body><p><t>` + strings.Repeat("x", MaxExtractBytes) + `</t></p></body></document>`)); err != nil {
		t.Fatal(err)
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract("docx", archive.Bytes()); err == nil || !strings.Contains(err.Error(), "extraction limit") {
		t.Fatalf("oversized DOCX error = %v", err)
	}
}

func TestWatchRejectsSymlinkSource(t *testing.T) {
	v, _ := vault.New(t.TempDir())
	s := New(v, &fakeIndex{})
	outside := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(outside, []byte("secret"), 0o644)
	link := filepath.Join(t.TempDir(), "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip(err)
	}
	if _, err := s.ImportFile(link); err == nil {
		t.Fatal("symlink source accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Watch(ctx, filepath.Dir(link)); err == nil || err != context.Canceled {
		t.Fatalf("watch cancellation = %v", err)
	}
}
