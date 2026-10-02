package cloudsync

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/crdtstore"
	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

const pass = "correct horse battery staple"

func init() {
	// Real backups use 64 MiB Argon2id; tests derive keys dozens of times.
	DefaultKDF = KDFParams{Name: "argon2id", Time: 1, MemoryKiB: 64, Threads: 1}
}

type memSettings struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memSettings) Get(k string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[k]
}

func (s *memSettings) UpdateInternal(p map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range p {
		s.m[k] = v
	}
	return nil
}

type device struct {
	t    *testing.T
	name string
	root string
	e    *Engine
	set  *memSettings
	ix   *index.Index
}

func newDevice(t *testing.T, name string) *device {
	t.Helper()
	root := t.TempDir()
	v, err := vault.New(root)
	if err != nil {
		t.Fatal(err)
	}
	gdir := filepath.Join(v.Root, ".grimoire")
	database, err := db.Open(filepath.Join(gdir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ix := index.New(database, v, embed.Hash{})
	set := &memSettings{m: map[string]string{"device_name": name}}
	e := New(v, ix, crdtstore.New(gdir), set, gdir)
	return &device{t: t, name: name, root: v.Root, e: e, set: set, ix: ix}
}

func (d *device) setup(folder string, create bool) {
	d.t.Helper()
	if _, err := d.e.Setup(SetupOptions{Folder: folder, Passphrase: pass, Create: create}); err != nil {
		d.t.Fatalf("%s setup: %v", d.name, err)
	}
}

func (d *device) write(rel, content string) {
	d.t.Helper()
	p := filepath.Join(d.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		d.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		d.t.Fatal(err)
	}
}

func (d *device) read(rel string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(d.root, filepath.FromSlash(rel)))
	return string(raw), err == nil
}

func (d *device) mustHave(rel, want string) {
	d.t.Helper()
	got, ok := d.read(rel)
	if !ok {
		d.t.Fatalf("%s: %s missing", d.name, rel)
	}
	if got != want {
		d.t.Fatalf("%s: %s = %q, want %q", d.name, rel, got, want)
	}
}

func (d *device) mustLack(rel string) {
	d.t.Helper()
	if _, ok := d.read(rel); ok {
		d.t.Fatalf("%s: %s should not exist", d.name, rel)
	}
}

func (d *device) sync() Stats {
	d.t.Helper()
	st, err := d.e.SyncOnce()
	if err != nil {
		d.t.Fatalf("%s sync: %v", d.name, err)
	}
	return st
}

// files lists the syncable files a device holds, for whole-vault comparison.
func (d *device) files() map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(d.root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(d.root, p)
		rel = filepath.ToSlash(rel)
		if de.IsDir() {
			if strings.HasPrefix(de.Name(), ".") && p != d.root {
				return filepath.SkipDir
			}
			return nil
		}
		if Syncable(rel) {
			raw, _ := os.ReadFile(p)
			out[rel] = string(raw)
		}
		return nil
	})
	return out
}

func syncAll(devs ...*device) {
	for i := 0; i < 2; i++ {
		for _, d := range devs {
			d.sync()
		}
	}
}

func pair(t *testing.T) (folder string, a, b *device) {
	folder = t.TempDir()
	a, b = newDevice(t, "laptop"), newDevice(t, "desktop")
	a.setup(folder, true)
	b.setup(folder, false)
	return folder, a, b
}

func sameVault(t *testing.T, devs ...*device) {
	t.Helper()
	want := devs[0].files()
	for _, d := range devs[1:] {
		got := d.files()
		if len(got) != len(want) {
			t.Fatalf("%s has %v, %s has %v", devs[0].name, names(want), d.name, names(got))
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s differs on %s: %q vs %q", d.name, k, got[k], v)
			}
		}
	}
}

func names(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func conflictCopies(d *device) []string {
	var out []string
	for rel := range d.files() {
		if strings.Contains(rel, "(conflict ") {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- propagation

func TestCreateEditDeletePropagate(t *testing.T) {
	_, a, b := pair(t)

	a.write("projects/plan.md", "# Plan\n\nship it\n")
	a.write("attachments/diagram.png", "\x89PNG fake image bytes")
	a.write("canvases/board.canvas", `{"nodes":[],"edges":[]}`)
	syncAll(a, b)
	b.mustHave("projects/plan.md", "# Plan\n\nship it\n")
	b.mustHave("attachments/diagram.png", "\x89PNG fake image bytes")
	b.mustHave("canvases/board.canvas", `{"nodes":[],"edges":[]}`)
	if n, _ := b.ix.DB.Count("SELECT COUNT(*) FROM notes WHERE path='projects/plan.md'"); n != 1 {
		t.Fatalf("a synced note must be indexed on arrival, got %d rows", n)
	}

	b.write("projects/plan.md", "# Plan\n\nship it on friday\n")
	b.write("canvases/board.canvas", `{"nodes":[{"id":"1"}],"edges":[]}`)
	syncAll(b, a)
	a.mustHave("projects/plan.md", "# Plan\n\nship it on friday\n")
	a.mustHave("canvases/board.canvas", `{"nodes":[{"id":"1"}],"edges":[]}`)
	if c := conflictCopies(a); len(c) != 0 {
		t.Fatalf("a plain later edit must fast-forward, not conflict: %v", c)
	}

	os.Remove(filepath.Join(a.root, "projects/plan.md"))
	os.Remove(filepath.Join(a.root, "attachments/diagram.png"))
	syncAll(a, b)
	b.mustLack("projects/plan.md")
	b.mustLack("attachments/diagram.png")
	if n, _ := b.ix.DB.Count("SELECT COUNT(*) FROM notes WHERE path='projects/plan.md'"); n != 0 {
		t.Fatal("a synced deletion must leave the index")
	}
	sameVault(t, a, b)
}

func TestThreeDevicesConverge(t *testing.T) {
	folder, a, b := pair(t)
	c := newDevice(t, "tablet")
	c.setup(folder, false)
	a.write("a.md", "from a\n")
	b.write("b.md", "from b\n")
	c.write("c.md", "from c\n")
	syncAll(a, b, c)
	sameVault(t, a, b, c)
	if len(a.files()) != 3 {
		t.Fatalf("want 3 notes, got %v", names(a.files()))
	}
	st := a.e.Status()
	if len(st.Devices) != 3 {
		t.Fatalf("status should list 3 devices, got %+v", st.Devices)
	}
	names := map[string]bool{}
	for _, d := range st.Devices {
		names[d.Name] = true
	}
	if !names["desktop"] || !names["tablet"] || !names["laptop"] {
		t.Fatalf("device names missing from status: %+v", st.Devices)
	}
}

// ---------------------------------------------------------------- concurrency

func TestConcurrentEditsToOneNoteMergeViaCRDT(t *testing.T) {
	_, a, b := pair(t)
	a.write("notes/shared.md", "---\ntitle: Shared\n---\nline one\nline two\nline three\n")
	syncAll(a, b)

	// both edit before either syncs
	a.write("notes/shared.md", "---\ntitle: Shared\n---\nline ONE from laptop\nline two\nline three\n")
	b.write("notes/shared.md", "---\ntitle: Shared\n---\nline one\nline two\nline three\nline four from desktop\n")
	syncAll(a, b)

	want := "---\ntitle: Shared\n---\nline ONE from laptop\nline two\nline three\nline four from desktop\n"
	a.mustHave("notes/shared.md", want)
	b.mustHave("notes/shared.md", want)
	if c := conflictCopies(a); len(c) != 0 {
		t.Fatalf("a mergeable edit should not need a conflict copy: %v", c)
	}
	sameVault(t, a, b)
}

func TestConcurrentEditsWithoutHistoryKeepBoth(t *testing.T) {
	_, a, b := pair(t)
	a.write("canvases/board.canvas", `{"v":0}`)
	syncAll(a, b)

	a.write("canvases/board.canvas", `{"v":"laptop"}`)
	b.write("canvases/board.canvas", `{"v":"desktop"}`)
	future := time.Now().Add(time.Minute)
	os.Chtimes(filepath.Join(b.root, "canvases/board.canvas"), future, future) // desktop is newer
	syncAll(a, b)

	a.mustHave("canvases/board.canvas", `{"v":"desktop"}`)
	copies := conflictCopies(a)
	if len(copies) != 1 {
		t.Fatalf("want exactly one conflict copy, got %v", copies)
	}
	a.mustHave(copies[0], `{"v":"laptop"}`)
	if !strings.HasSuffix(copies[0], ".canvas") {
		t.Fatalf("a canvas conflict copy must stay a canvas: %s", copies[0])
	}
	sameVault(t, a, b)
}

// Two devices that start from copies of the same vault have the same text but
// CRDT documents built separately. The first contact must give them shared
// history, so their next concurrent edit merges.
func TestIdenticalStartingVaultsLaterMerge(t *testing.T) {
	_, a, b := pair(t)
	a.write("n.md", "alpha\nbeta\n")
	b.write("n.md", "alpha\nbeta\n")
	syncAll(a, b)
	a.write("n.md", "ALPHA\nbeta\n")
	b.write("n.md", "alpha\nbeta\ngamma\n")
	syncAll(a, b)
	a.mustHave("n.md", "ALPHA\nbeta\ngamma\n")
	sameVault(t, a, b)
	if c := conflictCopies(a); len(c) != 0 {
		t.Fatalf("unexpected conflict copies %v", c)
	}
}

// Delete on one device, edit on the other: the edit wins and the note
// survives on both, with the edited text.
func TestDeleteVersusEditKeepsTheEdit(t *testing.T) {
	_, a, b := pair(t)
	a.write("todo.md", "buy milk\n")
	syncAll(a, b)

	os.Remove(filepath.Join(a.root, "todo.md"))
	b.write("todo.md", "buy milk\nbuy eggs\n")
	syncAll(a, b)

	a.mustHave("todo.md", "buy milk\nbuy eggs\n")
	b.mustHave("todo.md", "buy milk\nbuy eggs\n")

	// and the order of the two rounds does not matter
	a.write("x.md", "one\n")
	syncAll(a, b)
	b.write("x.md", "one\ntwo\n")
	os.Remove(filepath.Join(a.root, "x.md"))
	syncAll(b, a)
	a.mustHave("x.md", "one\ntwo\n")
	b.mustHave("x.md", "one\ntwo\n")
}

// ---------------------------------------------------------------- restore

func TestNewDeviceRestoresWholeVault(t *testing.T) {
	folder, a, b := pair(t)
	a.write("journal/2026-10-01.md", "---\ntags: [daily]\n---\nwrote the sync\n")
	a.write("attachments/photo.jpg", strings.Repeat("\xff\xd8", 1000))
	b.write("ideas.md", "an idea\n")
	syncAll(a, b)

	fresh := newDevice(t, "new laptop")
	fresh.setup(folder, false)
	fresh.sync()
	sameVault(t, a, b, fresh)
	if len(fresh.files()) != 3 {
		t.Fatalf("restore got %v", names(fresh.files()))
	}
}

func TestRestoreDeletedNoteFromFolder(t *testing.T) {
	_, a, b := pair(t)
	a.write("gone.md", "remember this\n")
	syncAll(a, b)
	os.Remove(filepath.Join(a.root, "gone.md"))
	syncAll(a, b)
	b.mustLack("gone.md")

	del, err := b.e.Deleted()
	if err != nil || len(del) != 1 || del[0].Path != "gone.md" || del[0].Device == "" {
		t.Fatalf("deleted list = %+v, %v", del, err)
	}
	if err := b.e.Restore("gone.md"); err != nil {
		t.Fatal(err)
	}
	b.mustHave("gone.md", "remember this\n")
	syncAll(b, a)
	a.mustHave("gone.md", "remember this\n")
	if del, _ := a.e.Deleted(); len(del) != 0 {
		t.Fatalf("a restored note is no longer deleted: %+v", del)
	}
}

// ---------------------------------------------------------------- passphrase

func snapshot(t *testing.T, dir string) map[string][32]byte {
	out := map[string][32]byte{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			raw, _ := os.ReadFile(p)
			out[p] = sha256.Sum256(raw)
		}
		return nil
	})
	return out
}

func TestWrongPassphraseIsClearAndWritesNothing(t *testing.T) {
	folder, a, _ := pair(t)
	a.write("n.md", "x\n")
	a.sync()

	c := newDevice(t, "intruder")
	before := snapshot(t, folder)
	_, err := c.e.Setup(SetupOptions{Folder: folder, Passphrase: "not the passphrase"})
	if !IsCode(err, CodeWrongPass) {
		t.Fatalf("want wrong_passphrase, got %v", err)
	}
	if !strings.Contains(err.Error(), "passphrase does not match") {
		t.Fatalf("message should be plain: %q", err.Error())
	}
	after := snapshot(t, folder)
	if len(before) != len(after) {
		t.Fatal("a wrong passphrase must write nothing to the folder")
	}
	for p, h := range before {
		if after[p] != h {
			t.Fatalf("%s changed", p)
		}
	}
	if _, err := os.Stat(c.e.keyPath()); err == nil {
		t.Fatal("a wrong passphrase must not store a key")
	}
	if c.set.Get("sync_folder") != "" {
		t.Fatal("a wrong passphrase must not enable sync")
	}
	if len(c.files()) != 0 {
		t.Fatal("a wrong passphrase must not write notes")
	}
}

func TestSecondDeviceNeverStartsASecondBackup(t *testing.T) {
	folder := t.TempDir()
	a := newDevice(t, "a")
	_, err := a.e.Setup(SetupOptions{Folder: folder, Passphrase: pass})
	if !IsCode(err, CodeNoBackup) {
		t.Fatalf("joining an empty folder must say there is no backup, got %v", err)
	}
	// devices present but header not downloaded yet
	os.MkdirAll(filepath.Join(folder, rootName, devicesName, strings.Repeat("a", 32)), 0o755)
	_, err = a.e.Setup(SetupOptions{Folder: folder, Passphrase: pass, Create: true})
	if !IsCode(err, CodeNotDownloaded) {
		t.Fatalf("a folder with devices but no header is still downloading, got %v", err)
	}
	_, err = a.e.Setup(SetupOptions{Folder: t.TempDir(), Passphrase: "short", Create: true})
	if !IsCode(err, CodeBadPassphrase) {
		t.Fatalf("want weak_passphrase, got %v", err)
	}
}

func TestKeyStoredPrivately(t *testing.T) {
	_, a, _ := pair(t)
	info, err := os.Stat(a.e.keyPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", info.Mode().Perm())
	}
	if !strings.HasPrefix(a.e.keyPath(), filepath.Join(a.root, ".grimoire")+string(filepath.Separator)) {
		t.Fatalf("key must live in .grimoire: %s", a.e.keyPath())
	}
	if err := a.e.Disable(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.e.keyPath()); err == nil {
		t.Fatal("turning sync off must forget the key")
	}
	if a.e.Folder() != "" {
		t.Fatal("sync should be off")
	}
}

// ---------------------------------------------------------------- unreliable folder

func blobFile(t *testing.T, folder string, d *device, content string) string {
	t.Helper()
	_, k, err := d.e.loadKey()
	if err != nil {
		t.Fatal(err)
	}
	name := k.name([]byte(content))
	var found string
	_ = filepath.WalkDir(filepath.Join(folder, rootName), func(p string, de fs.DirEntry, err error) error {
		if err == nil && de.Name() == name {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatalf("no blob for %q", content)
	}
	return found
}

func TestMissingTruncatedAndPlaceholderBlobsRetry(t *testing.T) {
	folder, a, b := pair(t)
	a.write("one.md", "first note\n")
	a.write("two.md", "second note\n")
	a.sync()

	// one blob has not arrived (iCloud left a placeholder), the other is half there
	p1 := blobFile(t, folder, a, "first note\n")
	held := p1 + ".held"
	os.Rename(p1, held)
	os.WriteFile(filepath.Join(filepath.Dir(p1), "."+filepath.Base(p1)+".icloud"), []byte("bplist"), 0o644)
	p2 := blobFile(t, folder, a, "second note\n")
	full, _ := os.ReadFile(p2)
	os.WriteFile(p2, full[:len(full)/2], 0o644)

	st := b.sync()
	if st.Waiting != 2 {
		t.Fatalf("both unavailable blobs should be waiting, got %+v", st)
	}
	b.mustLack("one.md")
	b.mustLack("two.md")

	// they arrive
	os.Rename(held, p1)
	os.Remove(filepath.Join(filepath.Dir(p1), "."+filepath.Base(p1)+".icloud"))
	os.WriteFile(p2, full, 0o644)
	b.sync()
	b.mustHave("one.md", "first note\n")
	b.mustHave("two.md", "second note\n")
}

func TestHalfDownloadedManifestIsNeverADeletion(t *testing.T) {
	folder, a, b := pair(t)
	a.write("keep.md", "keep me\n")
	syncAll(a, b)
	b.mustHave("keep.md", "keep me\n")

	m := filepath.Join(folder, rootName, devicesName, a.e.loadState().DeviceID, manifestName)
	full, _ := os.ReadFile(m)
	os.WriteFile(m, full[:len(full)/3], 0o644)
	b.sync()
	b.mustHave("keep.md", "keep me\n")

	os.Remove(m)
	os.WriteFile(filepath.Join(filepath.Dir(m), "."+manifestName+".icloud"), nil, 0o644)
	b.sync()
	b.mustHave("keep.md", "keep me\n")
	st := b.e.Status()
	var issue string
	for _, d := range st.Devices {
		if d.Name == "laptop" {
			issue = d.Issue
		}
	}
	if issue != CodeNotDownloaded {
		t.Fatalf("the peer should read as not downloaded, got %q", issue)
	}

	os.Remove(filepath.Join(filepath.Dir(m), "."+manifestName+".icloud"))
	os.WriteFile(m, full, 0o644)
	a.write("keep.md", "keep me, edited\n")
	syncAll(a, b)
	b.mustHave("keep.md", "keep me, edited\n")
}

func TestCloudConflictedCopiesAreIgnored(t *testing.T) {
	folder, a, b := pair(t)
	a.write("n.md", "real\n")
	a.sync()
	adir := filepath.Join(folder, rootName, devicesName, a.e.loadState().DeviceID)
	real, _ := os.ReadFile(filepath.Join(adir, manifestName))
	// what Dropbox, OneDrive and Google Drive name their copies
	os.WriteFile(filepath.Join(adir, "manifest (desktop's conflicted copy 2026-10-02).grs"), []byte("garbage"), 0o644)
	os.WriteFile(filepath.Join(adir, "manifest-DESKTOP-1.grs"), real, 0o644)
	os.WriteFile(filepath.Join(adir, "manifest (1).grs"), real[:10], 0o644)
	os.MkdirAll(filepath.Join(folder, rootName, devicesName, a.e.loadState().DeviceID+" (1)"), 0o755)
	os.WriteFile(filepath.Join(folder, rootName, "grimoire-sync (conflicted copy).json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(adir, blobsName, "zz"), []byte("stray"), 0o644)

	st := b.sync()
	if st.Devices != 2 {
		t.Fatalf("a duplicated device directory must not count as a device: %+v", st)
	}
	b.mustHave("n.md", "real\n")
	if len(b.files()) != 1 {
		t.Fatalf("conflicted copies must not become notes: %v", names(b.files()))
	}
	a.sync() // and the owner is not confused by them either
}

// ---------------------------------------------------------------- privacy

func TestNothingPlaintextInFolder(t *testing.T) {
	folder, a, b := pair(t)
	marker := "ZEBRA-CARDAMOM-7731"
	a.write("private/"+marker+" plans.md", "---\ntitle: "+marker+" title\n---\nThe body says "+marker+"\n")
	a.write("attachments/"+marker+".txt", marker)
	a.write(".grimoire/settings.json", `{"llm_api_key":"`+marker+`"}`)
	syncAll(a, b)

	_ = filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if strings.Contains(p, marker) || strings.Contains(p, "plans") || strings.Contains(p, "private") {
			t.Errorf("a path in the folder reveals a note name: %s", p)
		}
		if strings.Contains(p, "laptop") || strings.Contains(p, "desktop") {
			t.Errorf("a path in the folder reveals a device name: %s", p)
		}
		if !d.IsDir() {
			raw, _ := os.ReadFile(p)
			for _, needle := range []string{marker, "plans", "laptop", "desktop", "private/"} {
				if bytes.Contains(raw, []byte(needle)) {
					t.Errorf("%s contains plaintext %q", p, needle)
				}
			}
		}
		return nil
	})
	b.mustHave("private/"+marker+" plans.md", "---\ntitle: "+marker+" title\n---\nThe body says "+marker+"\n")
}

func TestGrimoireDirAndExcludedPathsNeverSync(t *testing.T) {
	_, a, b := pair(t)
	a.write(".grimoire/settings.json", `{"x":"y"}`)
	a.write(".grimoire/notes-in-here.md", "no\n")
	a.write("templates/meeting.md", "template\n")
	a.write(".obsidian/workspace.md", "no\n")
	a.write("plugins/evil/main.js", "alert(1)")
	a.write("notes/fine.md", "yes\n")
	a.write("notes/scratch.md.tmp", "partial")
	syncAll(a, b)

	if got := names(b.files()); len(got) != 1 || got[0] != "notes/fine.md" {
		t.Fatalf("only the note should sync, got %v", got)
	}
	for _, rel := range []string{".grimoire/notes-in-here.md", "templates/meeting.md",
		".obsidian/workspace.md", "plugins/evil/main.js", "notes/scratch.md.tmp"} {
		b.mustLack(rel)
	}
	raw, _ := os.ReadFile(filepath.Join(b.root, ".grimoire", "settings.json"))
	if strings.Contains(string(raw), `"x"`) {
		t.Fatal(".grimoire/settings.json synced")
	}
}

func TestIncomingPathsAreConfined(t *testing.T) {
	for _, bad := range []string{"../escape.md", ".grimoire/x.md", "a/../../b.md",
		"/etc/passwd.md", `win\path.md`, "templates/t.md", "plugins/x/main.js", "x.md.tmp", ""} {
		if Syncable(bad) {
			t.Errorf("Syncable(%q) = true", bad)
		}
	}
	for _, good := range []string{"a.md", "deep/er/note.md", "attachments/x.png", "b.canvas"} {
		if !Syncable(good) {
			t.Errorf("Syncable(%q) = false", good)
		}
	}
}

func TestEmptyVaultIsNotPublishedAsMassDeletion(t *testing.T) {
	_, a, b := pair(t)
	for i := 0; i < massDeleteFloor; i++ {
		a.write(fmt.Sprintf("n%d.md", i), "x\n")
	}
	a.write("one.md", "1\n")
	syncAll(a, b)
	for rel := range a.files() {
		os.Remove(filepath.Join(a.root, rel))
	}
	if _, err := a.e.SyncOnce(); err == nil || !strings.Contains(err.Error(), "looks empty") {
		t.Fatalf("want a refusal, got %v", err)
	}
	b.sync()
	b.mustHave("one.md", "1\n")
}

func TestCopiedInstallationBecomesNewDevice(t *testing.T) {
	folder, a, b := pair(t)
	a.write("n.md", "x\n")
	syncAll(a, b)
	// simulate a .grimoire/ copied to another machine: b takes a's identity
	ast := a.e.loadState()
	bst := b.e.loadState()
	bst.DeviceID = ast.DeviceID
	bst.Seq = ast.Seq + 5 // ahead, as a busy clone would be
	b.e.saveState(bst)
	b.sync()
	if b.e.loadState().DeviceID == ast.DeviceID {
		t.Fatal("a clone must move to a new device id")
	}
	syncAll(a, b)
	ids, _ := listDevices(newLayout(folder))
	if len(ids) != 3 {
		t.Fatalf("want the original two directories plus the clone's new one, got %d", len(ids))
	}
}

func TestSuggestionsFindCloudDrives(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{"Dropbox", "OneDrive - Personal", "Google Drive",
		"Library/Mobile Documents/com~apple~CloudDocs", "Documents"} {
		os.MkdirAll(filepath.Join(home, filepath.FromSlash(d)), 0o755)
	}
	got := map[string]string{}
	for _, s := range Suggestions(home) {
		got[s.Provider] = s.Path
	}
	for _, p := range []string{"dropbox", "onedrive", "gdrive", "icloud"} {
		if got[p] == "" {
			t.Errorf("no suggestion for %s: %v", p, got)
		}
	}
	if !strings.HasSuffix(got["dropbox"], filepath.Join("Dropbox", "Grimoire")) {
		t.Errorf("suggested path should be a Grimoire folder inside the drive: %s", got["dropbox"])
	}
	if !IsICloudPath(got["icloud"]) {
		t.Error("iCloud path not recognised")
	}
}

func TestPlacementGuards(t *testing.T) {
	a := newDevice(t, "a")
	if _, err := a.e.Setup(SetupOptions{Folder: filepath.Join(a.root, "sync"), Passphrase: pass, Create: true}); !IsCode(err, CodeFolderInVault) {
		t.Fatalf("want folder_in_vault, got %v", err)
	}
	if _, err := a.e.Setup(SetupOptions{Folder: filepath.Dir(a.root), Passphrase: pass, Create: true}); !IsCode(err, CodeVaultInFolder) {
		t.Fatalf("want vault_in_folder, got %v", err)
	}
}
