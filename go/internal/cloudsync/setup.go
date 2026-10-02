package cloudsync

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// SetupOptions is everything the "Sync & backup" form submits.
type SetupOptions struct {
	Folder     string
	Passphrase string
	// Create allows starting a NEW backup when the folder has none. Without
	// it, a folder with no backup is an error: on a second device that almost
	// always means the cloud drive has not downloaded it yet, and starting a
	// fresh one there would split the devices into two backups.
	Create     bool
	DeviceName string
	Interval   int
}

// MinPassphrase is the shortest passphrase a new backup accepts.
const MinPassphrase = 8

// Setup connects this device to a folder: it creates a backup there or joins
// the one already there. A wrong passphrase writes nothing, anywhere.
func (e *Engine) Setup(o SetupOptions) (created bool, err error) {
	folder := strings.TrimSpace(ExpandHome(strings.TrimSpace(o.Folder)))
	if folder == "" {
		return false, errf(CodeFolderMissing, "no folder given")
	}
	if abs, err := filepath.Abs(folder); err == nil {
		folder = abs
	}
	if err := e.checkPlacement(folder); err != nil {
		return false, err
	}
	info, err := os.Stat(folder)
	if err != nil && errors.Is(err, fs.ErrNotExist) && o.Create {
		// "~/Dropbox/Grimoire" before it exists: make the leaf, never a
		// whole missing path (a missing ~/Dropbox means no Dropbox app).
		if utf8.RuneCountInString(o.Passphrase) < MinPassphrase {
			return false, errf(CodeBadPassphrase, "")
		}
		if _, err := CreateFolder(folder); err != nil {
			return false, err
		}
		info, err = os.Stat(folder)
	}
	if err != nil || !info.IsDir() {
		return false, errf(CodeFolderMissing, folder)
	}
	if err := e.checkPlacement(folder); err != nil {
		return false, err
	}

	l := newLayout(folder)
	h, err := readHeader(l)
	var k *keys
	switch {
	case err == nil:
		if k, err = deriveKeys(o.Passphrase, h.KDF); err != nil {
			return false, err
		}
		if !k.verify(h) {
			return false, errf(CodeWrongPass, "")
		}
	case errors.Is(err, fs.ErrNotExist):
		if ents, derr := os.ReadDir(l.devices()); derr == nil && len(ents) > 0 {
			return false, errf(CodeNotDownloaded, "")
		}
		if !o.Create {
			return false, errf(CodeNoBackup, folder)
		}
		if utf8.RuneCountInString(o.Passphrase) < MinPassphrase {
			return false, errf(CodeBadPassphrase, "")
		}
		if h, k, err = newHeader(o.Passphrase, e.now().UnixMilli()); err != nil {
			return false, err
		}
		raw, _ := json.MarshalIndent(h, "", "  ")
		if err := writeAtomic(l.header(), append(raw, '\n'), 0o644); err != nil {
			return false, errf(CodeUnwritable, err.Error())
		}
		_ = writeAtomic(filepath.Join(l.root, readmeName), []byte(readmeText), 0o644)
		if err := os.MkdirAll(l.devices(), 0o755); err != nil {
			return false, errf(CodeUnwritable, err.Error())
		}
		created = true
	default:
		return false, err
	}

	if err := e.saveKey(h.ID, k); err != nil {
		return created, err
	}
	patch := map[string]string{"sync_folder": folder}
	if n := strings.TrimSpace(o.DeviceName); n != "" {
		patch["device_name"] = n
	}
	if o.Interval > 0 {
		patch["sync_folder_interval"] = strconv.Itoa(o.Interval)
	}
	if e.Settings != nil {
		if err := e.Settings.UpdateInternal(patch); err != nil {
			return created, err
		}
	}
	e.Kick()
	return created, nil
}

// checkPlacement refuses a folder that contains the vault or sits inside it:
// the first would sync the notes twice over, the second would sync the
// backup into itself.
func (e *Engine) checkPlacement(folder string) error {
	resolved := folder
	if r, err := filepath.EvalSymlinks(folder); err == nil {
		resolved = r
	}
	vroot := e.Vault.Root
	in := func(parent, child string) bool {
		return child == parent || strings.HasPrefix(child, strings.TrimSuffix(parent, string(filepath.Separator))+string(filepath.Separator))
	}
	if in(resolved, vroot) {
		return errf(CodeVaultInFolder, "")
	}
	if in(vroot, resolved) {
		return errf(CodeFolderInVault, "")
	}
	return nil
}

// Disable turns folder sync off on this device and forgets the stored key.
// The folder and its backup are left alone.
func (e *Engine) Disable() error {
	if e.Settings != nil {
		if err := e.Settings.UpdateInternal(map[string]string{"sync_folder": "off"}); err != nil {
			return err
		}
	}
	if err := os.Remove(e.keyPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// UpdateOptions changes the device name or interval.
func (e *Engine) UpdateOptions(deviceName string, interval int) error {
	patch := map[string]string{}
	if deviceName = strings.TrimSpace(deviceName); deviceName != "" {
		patch["device_name"] = deviceName
	}
	if interval > 0 {
		patch["sync_folder_interval"] = strconv.Itoa(interval)
	}
	if len(patch) == 0 || e.Settings == nil {
		return nil
	}
	return e.Settings.UpdateInternal(patch)
}

// ---------------------------------------------------------------- status

// DeviceStatus is one device as this one last saw it.
type DeviceStatus struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	LastSeen int64  `json:"last_seen"` // unix ms of its last published manifest
	Self     bool   `json:"self"`
	Issue    string `json:"issue,omitempty"`
}

// Status is what the settings section and `grimoire sync status` show.
type Status struct {
	Enabled     bool           `json:"enabled"`
	Folder      string         `json:"folder"`
	DeviceID    string         `json:"device_id"`
	DeviceName  string         `json:"device_name"`
	Interval    int            `json:"interval"`
	Running     bool           `json:"running"`
	LastSync    int64          `json:"last_sync"`
	LastAttempt int64          `json:"last_attempt"`
	Error       *Error         `json:"error,omitempty"`
	Stats       Stats          `json:"stats"`
	Devices     []DeviceStatus `json:"devices"`
	ICloud      bool           `json:"icloud"`
}

// Status reports the last round without running one.
func (e *Engine) Status() Status {
	s := Status{Folder: e.Folder(), DeviceName: e.DeviceName(),
		Interval: int(e.Interval() / time.Second), Running: e.Running()}
	s.Enabled = s.Folder != ""
	s.ICloud = IsICloudPath(s.Folder)
	st := e.loadState()
	s.DeviceID = st.DeviceID
	s.LastSync, s.LastAttempt, s.Stats = st.LastSync, st.LastAttempt, st.LastStats
	s.Devices = []DeviceStatus{{ID: st.DeviceID, Name: s.DeviceName, LastSeen: st.LastManifest, Self: true}}
	ids := make([]string, 0, len(st.Peers))
	for id := range st.Peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := st.Peers[id]
		s.Devices = append(s.Devices, DeviceStatus{ID: id, Name: p.Name, LastSeen: p.Updated, Issue: p.Issue})
	}
	if !s.Enabled {
		return s
	}
	s.Error = st.LastError
	if s.Error == nil {
		if _, _, _, err := e.ready(); err != nil {
			s.Error = AsError(err)
		}
	}
	return s
}

// IsICloudPath reports whether a path is in iCloud Drive, which can evict
// files to placeholders unless the folder is set to stay downloaded.
func IsICloudPath(p string) bool {
	p = filepath.ToSlash(p)
	return strings.Contains(p, "Mobile Documents/com~apple~CloudDocs") ||
		strings.Contains(strings.ToLower(p), "/iclouddrive")
}

// ---------------------------------------------------------------- folder picking

// Suggestion is a cloud-drive folder found on this machine.
type Suggestion struct {
	Provider string `json:"provider"`
	Label    string `json:"label"`
	Root     string `json:"root"` // the drive's own folder
	Path     string `json:"path"` // the suggested sync folder inside it
}

// Suggestions looks for the folders the common cloud drive apps create.
// home is passed in so tests can point it at a fake home directory.
func Suggestions(home string) []Suggestion {
	type cand struct{ provider, label, path string }
	var cands []cand
	glob := func(provider, label, pattern string) {
		matches, _ := filepath.Glob(pattern)
		sort.Strings(matches)
		for _, m := range matches {
			cands = append(cands, cand{provider, label, m})
		}
	}
	if home != "" {
		glob("dropbox", "Dropbox", filepath.Join(home, "Dropbox*"))
		glob("dropbox", "Dropbox", filepath.Join(home, "Library", "CloudStorage", "Dropbox*"))
		cands = append(cands, cand{"icloud", "iCloud Drive", filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs")})
		cands = append(cands, cand{"icloud", "iCloud Drive", filepath.Join(home, "iCloudDrive")})
		glob("onedrive", "OneDrive", filepath.Join(home, "OneDrive*"))
		glob("onedrive", "OneDrive", filepath.Join(home, "Library", "CloudStorage", "OneDrive*"))
		cands = append(cands, cand{"gdrive", "Google Drive", filepath.Join(home, "Google Drive")})
		cands = append(cands, cand{"gdrive", "Google Drive", filepath.Join(home, "My Drive")})
		glob("gdrive", "Google Drive", filepath.Join(home, "Library", "CloudStorage", "GoogleDrive*", "My Drive"))
	}
	cands = append(cands, cand{"gdrive", "Google Drive", "/Volumes/GoogleDrive/My Drive"})
	if runtime.GOOS == "windows" {
		cands = append(cands, cand{"gdrive", "Google Drive", `G:\My Drive`})
	}
	var out []Suggestion
	seen := map[string]bool{}
	for _, c := range cands {
		info, err := os.Stat(c.path)
		if err != nil || !info.IsDir() || seen[c.path] {
			continue
		}
		seen[c.path] = true
		out = append(out, Suggestion{Provider: c.provider, Label: c.label,
			Root: c.path, Path: filepath.Join(c.path, "Grimoire")})
	}
	return out
}

// Probe describes a candidate folder before the user commits to it, so the
// form can ask for a confirmation only when a new backup will be created.
type Probe struct {
	Path        string `json:"path"`
	Exists      bool   `json:"exists"`
	HasBackup   bool   `json:"has_backup"`
	Downloading bool   `json:"downloading"`
	ICloud      bool   `json:"icloud"`
	Problem     *Error `json:"problem,omitempty"`
}

// ProbeFolder inspects a folder without writing to it.
func (e *Engine) ProbeFolder(folder string) Probe {
	folder = ExpandHome(strings.TrimSpace(folder))
	if abs, err := filepath.Abs(folder); err == nil && folder != "" {
		folder = abs
	}
	p := Probe{Path: folder, ICloud: IsICloudPath(folder)}
	info, err := os.Stat(folder)
	if folder == "" || err != nil || !info.IsDir() {
		// A missing leaf under an existing folder is fine: Setup needs it to
		// exist, but the form can offer to create it.
		return p
	}
	p.Exists = true
	if err := e.checkPlacement(folder); err != nil {
		p.Problem = AsError(err)
		return p
	}
	l := newLayout(folder)
	switch _, err := readHeader(l); {
	case err == nil:
		p.HasBackup = true
	case IsCode(err, CodeNotDownloaded):
		p.Downloading = true
	case errors.Is(err, fs.ErrNotExist):
		if ents, derr := os.ReadDir(l.devices()); derr == nil && len(ents) > 0 {
			p.Downloading = true
		}
	default:
		p.Problem = AsError(err)
	}
	return p
}

// Listing is one directory in the server-side folder picker.
type Listing struct {
	Path   string   `json:"path"`
	Parent string   `json:"parent"`
	Dirs   []string `json:"dirs"`
}

// Browse lists the subdirectories of path (the home directory when empty),
// hiding dot-directories. It only lists names; it reads no file.
func Browse(path string) (Listing, error) {
	if strings.TrimSpace(path) == "" {
		path, _ = os.UserHomeDir()
	}
	path = ExpandHome(path)
	abs, err := filepath.Abs(path)
	if err != nil {
		return Listing{}, err
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		return Listing{}, errf(CodeFolderMissing, abs)
	}
	out := Listing{Path: abs, Parent: filepath.Dir(abs), Dirs: []string{}}
	if out.Parent == abs {
		out.Parent = ""
	}
	for _, en := range ents {
		if strings.HasPrefix(en.Name(), ".") {
			continue
		}
		isDir := en.IsDir()
		if !isDir && en.Type()&fs.ModeSymlink != 0 {
			if info, err := os.Stat(filepath.Join(abs, en.Name())); err == nil && info.IsDir() {
				isDir = true
			}
		}
		if isDir {
			out.Dirs = append(out.Dirs, en.Name())
		}
		if len(out.Dirs) >= 500 {
			break
		}
	}
	return out, nil
}

// CreateFolder makes the chosen sync folder if its parent exists, so picking
// "~/Dropbox/Grimoire" works before that leaf exists.
func CreateFolder(folder string) (string, error) {
	folder = ExpandHome(strings.TrimSpace(folder))
	abs, err := filepath.Abs(folder)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(filepath.Dir(abs)); err != nil || !info.IsDir() {
		return "", errf(CodeFolderMissing, filepath.Dir(abs))
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", errf(CodeUnwritable, err.Error())
	}
	return abs, nil
}
