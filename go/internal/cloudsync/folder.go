package cloudsync

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// On-disk layout inside the folder the user picked:
//
//	GrimoireSync/
//	  grimoire-sync.json          header: format, KDF params + salt, key check (plaintext)
//	  README.txt                  what this folder is (plaintext, no user data)
//	  devices/<device-id>/
//	    manifest.grs              encrypted: device name + {path, hash, mtime, deleted, history}
//	    blobs/<ab>/<name>         encrypted note/attachment/canvas contents and CRDT state,
//	                              named by a keyed hash of the content
//
// Every file under devices/<id>/ has exactly one writer: the device with that
// id. A cloud drive makes a "conflicted copy" when two machines change one
// file, so a layout with a shared file would eventually lose writes; this one
// cannot produce a conflict to begin with. The header is written once, by the
// device that creates the backup, and never rewritten.

const (
	rootName     = "GrimoireSync"
	headerName   = "grimoire-sync.json"
	readmeName   = "README.txt"
	devicesName  = "devices"
	manifestName = "manifest.grs"
	blobsName    = "blobs"

	// maxObjectBytes bounds any one decrypted object, so a hostile or corrupt
	// file cannot make a device allocate without limit.
	maxObjectBytes = 256 << 20
)

var (
	deviceIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	blobNameRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

const readmeText = `This folder holds an encrypted Grimoire backup.

Grimoire on each of your devices reads and writes here to keep your notes in
sync. Everything except grimoire-sync.json is encrypted with your sync
passphrase; file names do not reveal note names or contents.

Do not edit, rename or move files in here by hand. To set up another device,
install Grimoire, choose this same folder in Settings, Sync & backup, and enter
the same passphrase.
`

type layout struct{ root string }

func newLayout(folder string) layout { return layout{root: filepath.Join(folder, rootName)} }

func (l layout) header() string            { return filepath.Join(l.root, headerName) }
func (l layout) devices() string           { return filepath.Join(l.root, devicesName) }
func (l layout) device(id string) string   { return filepath.Join(l.devices(), id) }
func (l layout) manifest(id string) string { return filepath.Join(l.device(id), manifestName) }
func (l layout) blob(id, name string) string {
	return filepath.Join(l.device(id), blobsName, name[:2], name)
}

// placeholder reports whether iCloud has left a stand-in for a file it has
// not downloaded: "x" becomes ".x.icloud" until the bytes arrive.
func placeholder(path string) bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".icloud"))
	return err == nil
}

// writeAtomic writes through a temp file in the same directory and renames
// it into place, so a cloud client never uploads, and a reader never sees, a
// half-written file under the real name. The temp name is not one any reader
// opens.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+randHex(4)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// readHeader loads the header. A missing header is reported as such so the
// caller can tell "no backup here" from "the cloud drive is still busy".
func readHeader(l layout) (*Header, error) {
	raw, err := os.ReadFile(l.header())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if placeholder(l.header()) {
				return nil, errf(CodeNotDownloaded, "")
			}
			return nil, err
		}
		return nil, err
	}
	var h Header
	if json.Unmarshal(raw, &h) != nil || h.Format != Format {
		// A half-downloaded header is the common cause, and the fix for that
		// is to wait, not to start over.
		return nil, errf(CodeNotDownloaded, "")
	}
	if h.Version > Version {
		return nil, errf(CodeNewerFormat, "")
	}
	if h.Cipher != Cipher {
		return nil, errf(CodeNewerFormat, "")
	}
	return &h, nil
}

// Manifest is one device's view of the vault, as it last published it.
type Manifest struct {
	Version    int              `json:"v"`
	DeviceID   string           `json:"device_id"`
	DeviceName string           `json:"device_name"`
	Instance   string           `json:"instance"`
	Seq        int64            `json:"seq"`
	Updated    int64            `json:"updated"` // unix ms
	Entries    map[string]Entry `json:"entries"`
}

// Entry is one file in a manifest.
//
// H is the keyed content hash, which is also the blob name. P is the recent
// ancestry of this version (truncated hashes, newest first): a device whose
// current content appears there knows the entry descends from what it has and
// can fast-forward, rather than treating a plain later edit as a conflict.
type Entry struct {
	H string   `json:"h"`
	S int64    `json:"s,omitempty"`
	M int64    `json:"m"` // unix ms
	D bool     `json:"d,omitempty"`
	C string   `json:"c,omitempty"` // blob holding the note body's CRDT document
	P []string `json:"p,omitempty"`
}

const (
	histLen    = 10
	histPrefix = 16
)

func short(h string) string {
	if len(h) > histPrefix {
		return h[:histPrefix]
	}
	return h
}

// descends reports whether hash h is in e's ancestry (or is e itself).
func (e Entry) descends(h string) bool {
	if h == "" {
		return false
	}
	if e.H == h {
		return true
	}
	s := short(h)
	for _, p := range e.P {
		if p == s {
			return true
		}
	}
	return false
}

// withHistory returns hist with the given hashes prepended, deduplicated and
// capped.
func withHistory(hist []string, front ...string) []string {
	out := make([]string, 0, histLen)
	seen := map[string]bool{}
	add := func(h string) {
		h = short(h)
		if h == "" || seen[h] || len(out) >= histLen {
			return
		}
		seen[h] = true
		out = append(out, h)
	}
	for _, h := range front {
		add(h)
	}
	for _, h := range hist {
		add(h)
	}
	return out
}

func sameEntry(a, b Entry) bool { return a.H == b.H && a.D == b.D }

func (k *keys) writeManifest(l layout, m *Manifest) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	sealed, err := k.seal("manifest:"+m.DeviceID, raw)
	if err != nil {
		return err
	}
	return writeAtomic(l.manifest(m.DeviceID), sealed, 0o644)
}

// readManifest returns a device's manifest. errIncomplete covers every way a
// file can be present but not usable yet.
func (k *keys) readManifest(l layout, id string) (*Manifest, error) {
	sealed, err := os.ReadFile(l.manifest(id))
	if err != nil {
		return nil, err
	}
	plain, err := k.open("manifest:"+id, sealed)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if json.Unmarshal(plain, &m) != nil || m.DeviceID != id {
		return nil, errIncomplete
	}
	if m.Entries == nil {
		m.Entries = map[string]Entry{}
	}
	return &m, nil
}

// listDevices returns the device directories in the folder. Anything that is
// not exactly a device id is ignored: a cloud drive's "abc (1)" duplicate, an
// iCloud placeholder, a stray file somebody dropped in.
func listDevices(l layout) ([]string, error) {
	ents, err := os.ReadDir(l.devices())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && deviceIDRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// blobIndex lists which device directories hold which blobs, read once per
// round instead of a stat per file per device. A blob iCloud has not yet
// downloaded counts as present: it exists, it just is not here yet.
type blobIndex map[string][]string // name -> device ids

func buildBlobIndex(l layout, devices []string) blobIndex {
	idx := blobIndex{}
	for _, id := range devices {
		root := filepath.Join(l.device(id), blobsName)
		shards, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, sh := range shards {
			if !sh.IsDir() || len(sh.Name()) != 2 {
				continue
			}
			files, err := os.ReadDir(filepath.Join(root, sh.Name()))
			if err != nil {
				continue
			}
			for _, f := range files {
				name := f.Name()
				if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".icloud") {
					name = strings.TrimSuffix(strings.TrimPrefix(name, "."), ".icloud")
				}
				if blobNameRE.MatchString(name) {
					idx[name] = append(idx[name], id)
				}
			}
		}
	}
	return idx
}

func (k *keys) writeBlob(l layout, self string, content []byte) (string, error) {
	name := k.name(content)
	sealed, err := k.seal("blob:"+name, content)
	if err != nil {
		return "", err
	}
	return name, writeAtomic(l.blob(self, name), sealed, 0o644)
}

// readBlob finds a blob in any device's directory, preferring the ones the
// index says hold it. The content is checked against its name after
// decryption as well as authenticated, so a blob cannot be served under the
// wrong name.
func (k *keys) readBlob(l layout, idx blobIndex, devices []string, name string) ([]byte, error) {
	if !blobNameRE.MatchString(name) {
		return nil, errIncomplete
	}
	tried := map[string]bool{}
	try := func(id string) ([]byte, error) {
		tried[id] = true
		sealed, err := os.ReadFile(l.blob(id, name))
		if err != nil {
			return nil, err
		}
		plain, err := k.open("blob:"+name, sealed)
		if err != nil {
			return nil, err
		}
		if k.name(plain) != name {
			return nil, errIncomplete
		}
		return plain, nil
	}
	var lastErr error = fs.ErrNotExist
	for _, id := range append(append([]string{}, idx[name]...), devices...) {
		if tried[id] {
			continue
		}
		plain, err := try(id)
		if err == nil {
			return plain, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			lastErr = err
		}
	}
	return nil, lastErr
}

// cleanTemp removes this device's own abandoned temp files. Only its own
// directory: another device's temp file may be a write in progress.
func cleanTemp(dir string, olderThan time.Duration) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		n := d.Name()
		if strings.HasPrefix(n, ".") && strings.HasSuffix(n, ".tmp") {
			if info, err := d.Info(); err == nil && time.Since(info.ModTime()) > olderThan {
				_ = os.Remove(p)
			}
		}
		return nil
	})
}
