// Package documents imports external documents into the vault without making
// the external folder part of the vault's trust boundary.  Each generated
// note and preserved original is ownership-marked; deletes therefore cannot
// remove an operator-authored note, even if a source file has the same name.
package documents

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
	"github.com/fsnotify/fsnotify"
)

const (
	DefaultMaxBytes = 25 << 20
	MaxExtractBytes = 8 << 20
	manifestName    = "documents/.imports.json"
	originalsDir    = ".grimoire/document-originals"
	generatedDir    = "documents"
)

var ErrUnsupported = errors.New("unsupported document format")
var ErrManualEdit = errors.New("manual edit preserved")
var ErrPathCollision = errors.New("document target already exists and is not importer-owned")

type Result struct {
	Path       string `json:"path"`
	SourcePath string `json:"source_path"`
	Title      string `json:"title"`
	Format     string `json:"format"`
}

type Record struct {
	Result
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
	Hash          string `json:"hash"`
	Source        string `json:"source"`
	Original      string `json:"original"`
	GeneratedHash string `json:"generated_hash"`
}

type Indexer interface {
	Upsert(string) (*vault.Note, error)
	Remove(string) error
}

type Store struct {
	Vault    *vault.Vault
	Index    Indexer
	MaxBytes int64
	mu       sync.Mutex
	targets  map[string]string
}

type manifest struct {
	Records map[string]Record `json:"records"`
}

func New(v *vault.Vault, ix Indexer) *Store {
	return &Store{Vault: v, Index: ix, MaxBytes: DefaultMaxBytes, targets: map[string]string{}}
}

func (s *Store) load() (manifest, error) {
	m := manifest{Records: map[string]Record{}}
	p, err := s.Vault.SafeRawPath(manifestName)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if len(b) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("read document manifest: %w", err)
	}
	if m.Records == nil {
		m.Records = map[string]Record{}
	}
	return m, nil
}

func (s *Store) save(m manifest) error {
	p, err := s.Vault.SafeRawPath(manifestName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *Store) internalOriginal(rel string) (string, error) {
	if !strings.HasPrefix(rel, originalsDir+"/") {
		return "", errors.New("document original outside reserved store")
	}
	root := filepath.Join(s.Vault.Root, ".grimoire")
	p := filepath.Clean(filepath.Join(s.Vault.Root, filepath.FromSlash(rel)))
	if p != root && !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return "", errors.New("document original escapes vault")
	}
	return p, nil
}

func atomicOriginalWrite(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".document-original-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func formatFor(name string) (string, error) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md":
		return "md", nil
	case ".txt":
		return "txt", nil
	case ".pdf":
		return "pdf", nil
	case ".docx":
		return "docx", nil
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupported, filepath.Ext(name))
	}
}

func titleOf(name, text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if strings.HasPrefix(line, "#") {
			return strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
		if line != "" {
			return strings.TrimSpace(line)
		}
	}
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	return base
}

func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func pathID(source string) string {
	h := sha256.Sum256([]byte(source))
	return hex.EncodeToString(h[:])[:12]
}
func publicSource(source string) string { return "source:" + pathID(source) }

func (s *Store) ImportBytes(source string, name string, b []byte) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.importBytes(source, name, b)
}

func (s *Store) ImportBytesAt(source, name string, b []byte, path string) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.targets == nil {
		s.targets = map[string]string{}
	}
	s.targets[source] = path
	defer delete(s.targets, source)
	return s.importBytes(source, name, b)
}

func (s *Store) importBytes(source string, name string, b []byte) (Result, error) {
	if s.Vault == nil {
		return Result{}, errors.New("documents: nil vault")
	}
	max := s.MaxBytes
	if max <= 0 {
		max = DefaultMaxBytes
	}
	if int64(len(b)) > max {
		return Result{}, fmt.Errorf("document too large: %d bytes (max %d)", len(b), max)
	}
	m, err := s.load()
	if err != nil {
		return Result{}, err
	}
	old := m.Records[source]
	hash := hashBytes(b)
	if old.Path != "" && old.Hash == hash {
		if existing, readErr := s.Vault.Read(old.Path); readErr == nil {
			if old.GeneratedHash != "" && existing.Hash != old.GeneratedHash {
				return Result{}, fmt.Errorf("%w: source %q", ErrManualEdit, source)
			}
			return old.Result, nil
		}
	}
	format, err := formatFor(name)
	if err != nil {
		return Result{}, err
	}
	text, err := Extract(format, b)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(text) == "" {
		return Result{}, errors.New("document extraction produced no text; scanned/image-only files are not OCRed")
	}
	path := old.Path
	if path == "" {
		path = s.targets[source]
	}
	if path == "" {
		path = filepath.ToSlash(filepath.Join(generatedDir, vault.Slugify(strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)))+"-"+pathID(source)+".md"))
	}
	original := old.Original
	if original == "" || (old.Original != "" && filepath.Ext(strings.ToLower(original)) != filepath.Ext(strings.ToLower(name))) {
		original = filepath.ToSlash(filepath.Join(originalsDir, pathID(source)+filepath.Ext(strings.ToLower(name))))
	}
	if old.Path != "" && old.GeneratedHash != "" {
		if existing, readErr := s.Vault.Read(old.Path); readErr == nil && existing.Hash != old.GeneratedHash {
			return Result{}, fmt.Errorf("%w: source %q", ErrManualEdit, source)
		}
	}
	op, err := s.internalOriginal(original)
	if err != nil {
		return Result{}, err
	}
	if existing, readErr := s.Vault.Read(path); readErr == nil {
		owned := old.Path == path && old.GeneratedHash != "" && existing.Hash == old.GeneratedHash
		if !owned {
			return Result{}, fmt.Errorf("%w: %s", ErrPathCollision, path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(op), 0o755); err != nil {
		return Result{}, err
	}
	if err := atomicOriginalWrite(op, b); err != nil {
		return Result{}, err
	}
	sourceFM, sourceBody := markdown.ParseFrontmatter(text)
	if format != "md" {
		sourceFM = nil
		sourceBody = text
	}
	fm := markdown.NewFrontmatter()
	if sourceFM != nil {
		fm = sourceFM.Clone()
	}
	if old.Path != "" {
		if existing, e := s.Vault.Read(old.Path); e == nil && existing.Frontmatter != nil {
			fm = existing.Frontmatter.Clone()
		}
	}
	fm.Set("document_generated", true)
	fm.Set("document_source", source)
	fm.Set("document_hash", hash)
	fm.Set("document_format", format)
	fm.Set("document_original", original)
	body := "# " + titleOf(name, sourceBody) + "\n\n" + strings.TrimSpace(sourceBody) + "\n"
	note, err := s.Vault.Write(path, body, fm)
	if err != nil {
		return Result{}, err
	}
	if s.Index != nil {
		if _, err := s.Index.Upsert(note.Path); err != nil {
			return Result{}, err
		}
	}
	r := Record{Result: Result{Path: note.Path, SourcePath: publicSource(source), Title: note.Title, Format: format}, Status: "ready", Hash: hash, Source: source, Original: original, GeneratedHash: note.Hash}
	if old.Path != "" && old.Path != path && s.Index != nil {
		_ = s.Index.Remove(old.Path)
	}
	if old.Original != "" && old.Original != original {
		if oldPath, oldErr := s.internalOriginal(old.Original); oldErr == nil {
			_ = os.Remove(oldPath)
		}
	}
	m.Records[source] = r
	if err := s.save(m); err != nil {
		return Result{}, err
	}
	return r.Result, nil
}

func (s *Store) ImportFile(source string) (Result, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return Result{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Result{}, errors.New("document source symlink refused")
	}
	if info.Size() > DefaultMaxBytes {
		return Result{}, fmt.Errorf("document too large: %d bytes (max %d)", info.Size(), DefaultMaxBytes)
	}
	f, err := os.Open(source)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, DefaultMaxBytes+1))
	if err != nil {
		return Result{}, err
	}
	if int64(len(b)) > DefaultMaxBytes {
		return Result{}, fmt.Errorf("document too large: %d bytes (max %d)", len(b), DefaultMaxBytes)
	}
	return s.ImportBytes(source, filepath.Base(source), b)
}

func (s *Store) TargetPath(source, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, err := s.load(); err == nil {
		if r, ok := m.Records[source]; ok && r.Path != "" {
			return r.Path
		}
	}
	return filepath.ToSlash(filepath.Join(generatedDir, vault.Slugify(strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)))+"-"+pathID(source)+".md"))
}

func (s *Store) SourceForPath(path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return "", false
	}
	for source, r := range m.Records {
		if r.Path == path {
			return source, true
		}
	}
	return "", false
}

func (s *Store) Refresh(path string) (Result, error) {
	s.mu.Lock()
	m, err := s.load()
	s.mu.Unlock()
	if err != nil {
		return Result{}, err
	}
	for source, r := range m.Records {
		if r.Path == path {
			p, e := s.internalOriginal(r.Original)
			if e != nil {
				return Result{}, e
			}
			b, e := os.ReadFile(p)
			if e != nil {
				return Result{}, fmt.Errorf("saved original unavailable: %w", e)
			}
			name := filepath.Base(r.Original)
			return s.ImportBytes(source, name, b)
		}
	}
	return Result{}, fmt.Errorf("document not owned by importer: %s", path)
}

// Original returns a preserved attachment only after the caller has checked
// access to the owning generated note. The reserved path is never returned.
func (s *Store) Original(path string) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil, "", err
	}
	for _, r := range m.Records {
		if r.Path == path {
			p, e := s.internalOriginal(r.Original)
			if e != nil {
				return nil, "", e
			}
			b, e := os.ReadFile(p)
			if e != nil {
				return nil, "", e
			}
			return b, filepath.Ext(r.Original), nil
		}
	}
	return nil, "", fmt.Errorf("document not owned by importer: %s", path)
}

func (s *Store) List() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(m.Records))
	for _, r := range m.Records {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (s *Store) DeleteSource(source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	r, ok := m.Records[source]
	if !ok {
		return nil
	}
	if r.Path != "" {
		if n, e := s.Vault.Read(r.Path); e == nil && n.Frontmatter.BoolVal("document_generated") && n.Frontmatter.StringVal("document_source") == source && n.Hash == r.GeneratedHash {
			_ = s.Vault.Delete(r.Path)
			if s.Index != nil {
				_ = s.Index.Remove(r.Path)
			}
		} else {
			return fmt.Errorf("%w: source %q", ErrManualEdit, source)
		}
	}
	if r.Original != "" {
		if p, e := s.internalOriginal(r.Original); e == nil {
			_ = os.Remove(p)
		}
	}
	delete(m.Records, source)
	return s.save(m)
}

// Watch performs an initial reconciliation and then watches a folder until ctx
// is cancelled. It only deletes paths present in the importer manifest.
func (s *Store) InitialReconcile(ctx context.Context, folder string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	abs, err := filepath.Abs(folder)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("watch folder: %w", err)
	}
	watchRoot, _ := filepath.EvalSymlinks(abs)
	vaultRoot, _ := filepath.EvalSymlinks(s.Vault.Root)
	if watchRoot == vaultRoot || strings.HasPrefix(watchRoot, vaultRoot+string(filepath.Separator)) || strings.HasPrefix(vaultRoot, watchRoot+string(filepath.Separator)) {
		return errors.New("watch folder overlaps vault; choose a separate source directory")
	}
	return s.reconcile(abs)
}

func (s *Store) Watch(ctx context.Context, folder string) error {
	abs, err := filepath.Abs(folder)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("watch folder: %w", err)
	}
	watchRoot, _ := filepath.EvalSymlinks(abs)
	vaultRoot, _ := filepath.EvalSymlinks(s.Vault.Root)
	if watchRoot == vaultRoot || strings.HasPrefix(watchRoot, vaultRoot+string(filepath.Separator)) || strings.HasPrefix(vaultRoot, watchRoot+string(filepath.Separator)) {
		return errors.New("watch folder overlaps vault; choose a separate source directory")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	if err := s.watchDirs(w, abs); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if err := s.reconcile(abs); err != nil {
		return err
	}
	var timer *time.Timer
	var tick <-chan time.Time
	schedule := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.NewTimer(200 * time.Millisecond)
		tick = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return ctx.Err()
		case <-tick:
			tick = nil
			if err := s.reconcile(abs); err != nil {
				return err
			}
		case e, ok := <-w.Events:
			if !ok {
				return nil
			}
			if e.Op&fsnotify.Create != 0 {
				if info, e2 := os.Stat(e.Name); e2 == nil && info.IsDir() {
					if e2 = s.watchDirs(w, e.Name); e2 != nil {
						return e2
					}
				}
			}
			if e.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
				schedule()
			}
		case e, ok := <-w.Errors:
			if !ok {
				return nil
			}
			return e
		}
	}
}

func (s *Store) watchDirs(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return w.Add(path)
		}
		return nil
	})
}

func (s *Store) reconcile(root string) error {
	seen := map[string]bool{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if _, e := formatFor(path); e != nil {
			return nil
		}
		seen[path] = true
		_, e := s.ImportFile(path)
		if e != nil && !errors.Is(e, ErrUnsupported) {
			return fmt.Errorf("import %s: %w", path, e)
		}
		return nil
	})
	if err != nil {
		return err
	}
	recs, err := s.List()
	if err != nil {
		return err
	}
	for _, r := range recs {
		if strings.HasPrefix(r.Source, root+string(filepath.Separator)) && !seen[r.Source] {
			if err := s.DeleteSource(r.Source); err != nil {
				return err
			}
		}
	}
	return nil
}

// Extract converts a supported file into plain searchable text.
func Extract(format string, b []byte) (string, error) {
	switch format {
	case "md", "txt":
		return string(b), nil
	case "docx":
		return extractDOCX(b)
	case "pdf":
		return extractPDF(b)
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupported, format)
	}
}

type docxPara struct {
	Text string `xml:",chardata"`
}

func extractDOCX(b []byte) (string, error) {
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", fmt.Errorf("docx: invalid zip: %w", err)
	}
	var f *zip.File
	for _, x := range z.File {
		if x.Name == "word/document.xml" {
			f = x
			break
		}
	}
	if f == nil {
		return "", errors.New("docx: missing word/document.xml")
	}
	r, err := f.Open()
	if err != nil {
		return "", err
	}
	defer r.Close()
	xmlBytes, readErr := io.ReadAll(io.LimitReader(r, MaxExtractBytes+1))
	if readErr != nil {
		return "", readErr
	}
	if len(xmlBytes) > MaxExtractBytes {
		return "", errors.New("docx text exceeds extraction limit")
	}
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	var out strings.Builder
	inText := false
	for {
		tok, e := dec.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return "", fmt.Errorf("docx XML: %w", e)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "p":
				if out.Len() > 0 {
					out.WriteString("\n")
				}
			case "tab":
				out.WriteString("\t")
			}
		case xml.CharData:
			if inText {
				out.Write(t)
			}
		case xml.EndElement:
			if t.Name.Local == "t" {
				inText = false
			}
		}
	}
	return strings.TrimSpace(out.String()), nil
}

func extractPDF(b []byte) (string, error) {
	if len(b) < 5 || string(b[:5]) != "%PDF-" {
		return "", errors.New("pdf: invalid header")
	}
	tmp, err := os.CreateTemp("", "grimoire-pdf-*.pdf")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pdftotext, err := exec.LookPath("pdftotext")
	if err != nil {
		return "", errors.New("pdf extraction unavailable: pdftotext is required")
	}
	cmd := exec.CommandContext(ctx, pdftotext, "-layout", name, "-")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(out, MaxExtractBytes+1))
	tooLarge := len(data) > MaxExtractBytes
	if tooLarge && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return "", errors.New("pdf extraction timed out")
	}
	if readErr != nil {
		return "", readErr
	}
	if tooLarge {
		return "", errors.New("pdf text exceeds extraction limit")
	}
	if waitErr != nil {
		return "", fmt.Errorf("pdf extraction failed: %w", waitErr)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", errors.New("pdf extraction produced no text; scanned/image-only PDFs are not OCRed")
	}
	return strings.TrimSpace(string(data)), nil
}
