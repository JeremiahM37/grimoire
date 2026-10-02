// Package cloudsync keeps a vault in sync between devices through a folder
// that a cloud drive (Dropbox, iCloud Drive, OneDrive, Google Drive) already
// syncs, with no server and nothing new to sign up for.
//
// The folder is treated as a dumb, slow, non-atomic shared disk:
//
//   - one writer per file: each device writes only its own devices/<id>/
//     directory and reads everyone else's, so a cloud drive never has two
//     edits of one file to reconcile;
//   - everything is encrypted (XChaCha20-Poly1305 under an Argon2id-derived
//     key) and content-addressed by a keyed hash, so no file name reveals a
//     note's path, title or content;
//   - anything missing, half-downloaded or not yet arrived is skipped and
//     retried next round, never read as a deletion.
//
// Merging reuses the peer sync's machinery: per-note CRDT documents
// (internal/crdtstore) for concurrent edits to the same note, conflict copies
// (internal/sync's naming) where text cannot be merged, and tombstones for
// deletions. For each peer a device keeps the last entry it merged from that
// peer as the base, so only what changed since is looked at.
package cloudsync

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/crdtstore"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	gsync "github.com/JeremiahM37/grimoire/go/internal/sync"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Settings is the slice of settings.Store the engine uses.
type Settings interface {
	Get(key string) string
	UpdateInternal(patch map[string]string) error
}

// Engine runs sync rounds for one vault.
type Engine struct {
	Vault    *vault.Vault
	Index    *index.Index // optional; notes written by sync are re-indexed through it
	CRDT     *crdtstore.Store
	Settings Settings
	Dir      string // .grimoire/cloudsync

	// Trash moves a note deleted on another device into this device's trash,
	// so a synced deletion is undoable locally. Nil removes the file.
	Trash func(rel string) error
	// Snapshot records a note's previous body in version history before sync
	// overwrites it. Optional.
	Snapshot func(rel, body string)

	Now func() time.Time

	mu      sync.Mutex
	running bool
	kick    chan struct{}
}

// New builds an engine for a vault whose private directory is grimoireDir.
func New(v *vault.Vault, ix *index.Index, c *crdtstore.Store, s Settings, grimoireDir string) *Engine {
	return &Engine{Vault: v, Index: ix, CRDT: c, Settings: s,
		Dir: filepath.Join(grimoireDir, "cloudsync")}
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Stats summarises one round.
type Stats struct {
	Uploaded   int `json:"uploaded"`
	Downloaded int `json:"downloaded"`
	Deleted    int `json:"deleted"`
	Merged     int `json:"merged"`
	Conflicts  int `json:"conflicts"`
	Waiting    int `json:"waiting"` // files not available yet; retried next round
	Failed     int `json:"failed"`  // files that could not be written here
	Devices    int `json:"devices"`
}

const (
	tombstoneTTL   = 90 * 24 * time.Hour
	blobGrace      = 24 * time.Hour
	gcEvery        = time.Hour
	heartbeatEvery = time.Hour
	maxFileBytes   = 64 << 20
	lockStale      = 10 * time.Minute
	// massDeleteFloor: a vault that held at least this many files and now
	// holds none is treated as missing, not emptied. Below it, deleting your
	// last few notes is an ordinary thing to do.
	massDeleteFloor = 5
)

// Folder returns the configured folder, or "" when sync is off.
func (e *Engine) Folder() string {
	if e.Settings == nil {
		return ""
	}
	f := strings.TrimSpace(e.Settings.Get("sync_folder"))
	if f == "" || strings.EqualFold(f, "off") {
		return ""
	}
	return ExpandHome(f)
}

// DeviceName is the name other devices see; the hostname unless set.
func (e *Engine) DeviceName() string {
	if e.Settings != nil {
		if n := strings.TrimSpace(e.Settings.Get("device_name")); n != "" {
			return n
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "this device"
}

// Interval is the background sync period.
func (e *Engine) Interval() time.Duration {
	n := 60
	if e.Settings != nil {
		if v, err := strconv.Atoi(strings.TrimSpace(e.Settings.Get("sync_folder_interval"))); err == nil {
			n = v
		}
	}
	if n < 10 {
		n = 10
	}
	return time.Duration(n) * time.Second
}

// ExpandHome turns a leading ~ into the home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// Syncable reports whether a vault-relative path is content that syncs:
// notes, canvases, and everything under attachments/. Never .grimoire/ (the
// index, settings, credential vault and this device's sync state), never the
// other reserved directories the peer sync leaves out, never hidden files or
// directories (.obsidian, .trash, .git, .DS_Store), and never temp files.
// Incoming paths are held to the same rule, so a manifest cannot place a file
// anywhere this device would not have published one from.
func Syncable(rel string) bool {
	if rel == "" || strings.Contains(rel, `\`) || strings.HasPrefix(rel, "/") {
		return false
	}
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.HasPrefix(p, ".") {
			return false
		}
		for _, r := range vault.ReservedDirs {
			if p == r {
				return false
			}
		}
	}
	base := parts[len(parts)-1]
	if strings.HasSuffix(base, ".tmp") {
		return false
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".md", ".canvas":
		return true
	}
	return len(parts) > 1 && parts[0] == "attachments"
}

func isNote(rel string) bool { return strings.EqualFold(filepath.Ext(rel), ".md") }

// splitRaw separates a note's frontmatter block from its body, as the vault
// parses it: the CRDT tracks the body, the frontmatter travels whole.
func splitRaw(raw string) (prefix, body string) {
	_, body = markdown.ParseFrontmatter(raw)
	if !strings.HasSuffix(raw, body) {
		return "", raw
	}
	return raw[:len(raw)-len(body)], body
}

// ---------------------------------------------------------------- locking

// lock serialises rounds within this process and between the server and a
// CLI command on the same vault.
func (e *Engine) lock() (func(), error) {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return nil, errf(CodeBusy, "")
	}
	e.running = true
	e.mu.Unlock()
	release := func() {
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
	}
	if err := os.MkdirAll(e.Dir, 0o700); err != nil {
		release()
		return nil, err
	}
	p := filepath.Join(e.Dir, "lock")
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { _ = os.Remove(p); release() }, nil
		}
		if info, statErr := os.Stat(p); statErr == nil && time.Since(info.ModTime()) > lockStale {
			_ = os.Remove(p) // a crashed round; nothing holds it
			continue
		}
		break
	}
	release()
	return nil, errf(CodeBusy, "")
}

// Running reports whether a round is in progress in this process.
func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// ---------------------------------------------------------------- one round

type localFile struct {
	H string
	S int64
	M int64
}

type round struct {
	e    *Engine
	k    *keys
	l    layout
	st   *state
	self string
	name string
	now  time.Time

	devices []string
	idx     blobIndex

	cur     map[string]localFile
	present map[string]bool // syncable files seen, readable or not
	own     map[string]Entry
	changed map[string]bool
	adopted map[string]bool // entries whose CRDT blob was taken from a peer

	manifests map[string]*Manifest
	peersOK   bool
	stats     Stats
}

// SyncOnce runs one full round: scan, merge every peer, publish.
func (e *Engine) SyncOnce() (Stats, error) {
	release, err := e.lock()
	if err != nil {
		return Stats{}, err
	}
	defer release()

	st := e.loadState()
	st.LastAttempt = e.now().UnixMilli()
	stats, err := e.syncLocked(st)
	if err != nil {
		st.LastError = AsError(err)
	} else {
		st.LastError = nil
		st.LastSync = st.LastAttempt
		st.LastStats = stats
	}
	if saveErr := e.saveState(st); saveErr != nil && err == nil {
		err = saveErr
	}
	return stats, err
}

// ready resolves the folder, the header and the stored key, or says plainly
// which of them is the problem.
func (e *Engine) ready() (layout, *keys, *Header, error) {
	folder := e.Folder()
	if folder == "" {
		return layout{}, nil, nil, errf(CodeNotConfigured, "")
	}
	if info, err := os.Stat(folder); err != nil || !info.IsDir() {
		return layout{}, nil, nil, errf(CodeFolderMissing, folder)
	}
	l := newLayout(folder)
	h, err := readHeader(l)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if _, derr := os.Stat(l.devices()); derr == nil {
				return l, nil, nil, errf(CodeNotDownloaded, "")
			}
			return l, nil, nil, errf(CodeNoBackup, folder)
		}
		return l, nil, nil, err
	}
	kf, k, err := e.loadKey()
	if err != nil {
		return l, nil, nil, err
	}
	if kf.BackupID != h.ID {
		// The folder now holds a different backup than the one this
		// device's key was made for.
		return l, nil, nil, errf(CodeKeyMissing, "the folder holds a different backup")
	}
	if !k.verify(h) {
		return l, nil, nil, errf(CodeWrongPass, "")
	}
	return l, k, h, nil
}

func (e *Engine) syncLocked(st *state) (Stats, error) {
	l, k, h, err := e.ready()
	if err != nil {
		return Stats{}, err
	}
	if st.BackupID != h.ID {
		// First round against this backup: start clean, under a new id.
		*st = state{BackupID: h.ID, Cache: st.Cache,
			Published: map[string]Entry{}, Peers: map[string]*peerState{}}
		st.newIdentity()
	}
	if info, err := os.Stat(e.Vault.Root); err != nil || !info.IsDir() {
		return Stats{}, fmt.Errorf("the notes folder %s is not available", e.Vault.Root)
	}

	r := &round{e: e, k: k, l: l, st: st, name: e.DeviceName(), now: e.now(),
		cur: map[string]localFile{}, present: map[string]bool{},
		changed: map[string]bool{}, adopted: map[string]bool{},
		manifests: map[string]*Manifest{}, peersOK: true}

	r.checkIdentity()
	r.self = st.DeviceID
	if err := os.MkdirAll(l.device(r.self), 0o755); err != nil {
		return Stats{}, errf(CodeUnwritable, err.Error())
	}

	if err := r.scan(); err != nil {
		return Stats{}, err
	}
	r.own = make(map[string]Entry, len(st.Published))
	for p, en := range st.Published {
		r.own[p] = en
	}
	if err := r.recordLocalChanges(); err != nil {
		return Stats{}, err
	}

	if r.devices, err = listDevices(l); err != nil {
		return Stats{}, errf(CodeNotDownloaded, err.Error())
	}
	r.idx = buildBlobIndex(l, r.devices)

	seen := map[string]bool{}
	for _, id := range r.devices {
		if id == r.self {
			continue
		}
		seen[id] = true
		r.mergePeer(id)
	}
	for id := range st.Peers {
		if !seen[id] {
			delete(st.Peers, id) // the device's directory is gone
		}
	}

	if err := r.publish(); err != nil {
		return r.stats, err
	}
	r.gc()
	r.stats.Devices = len(r.devices)
	if r.stats.Devices == 0 {
		r.stats.Devices = 1
	}
	return r.stats, nil
}

// checkIdentity notices when another installation writes this device's
// directory: a copied .grimoire/ folder, or a backup restored on a second
// machine. Two writers is the one thing the layout exists to prevent, so the
// device that notices moves to a new id.
func (r *round) checkIdentity() {
	m, err := r.k.readManifest(r.l, r.st.DeviceID)
	if err != nil {
		return
	}
	if m.Instance != r.st.Instance {
		log.Printf("cloud sync: another installation is writing device %s; this one becomes a new device", r.st.DeviceID)
		r.st.newIdentity()
		r.st.Peers = map[string]*peerState{}
		return
	}
	if m.Seq > r.st.Seq {
		r.st.Seq = m.Seq
	}
}

// scan hashes the vault's syncable files, re-reading only what changed since
// the last round.
func (r *round) scan() error {
	root := r.e.Vault.Root
	cache := map[string]fileCache{}
	err := vault.WalkTree(root, r.e.Vault.Follow, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		if p == root {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			for _, reserved := range vault.ReservedDirs {
				if d.Name() == reserved {
					return filepath.SkipDir
				}
			}
			return nil
		}
		relp, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel := filepath.ToSlash(relp)
		if !Syncable(rel) {
			return nil
		}
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
			return nil
		}
		r.present[rel] = true
		c, ok := r.st.Cache[rel]
		mt := info.ModTime().UnixNano()
		h := c.H
		if !ok || c.MtimeNs != mt || c.Size != info.Size() {
			data, err := os.ReadFile(p)
			if err != nil {
				return nil // unreadable now: left alone, never read as deleted
			}
			h = r.k.name(data)
		}
		cache[rel] = fileCache{MtimeNs: mt, Size: info.Size(), H: h}
		r.cur[rel] = localFile{H: h, S: info.Size(), M: info.ModTime().UnixMilli()}
		return nil
	})
	r.st.Cache = cache
	return err
}

// recordLocalChanges folds this device's own edits since the last round into
// its pending manifest.
func (r *round) recordLocalChanges() error {
	live := 0
	for _, en := range r.own {
		if !en.D {
			live++
		}
	}
	if len(r.present) == 0 && live >= massDeleteFloor {
		// An empty vault is far more often an unmounted disk or a moved
		// folder than a person deleting every note, and publishing it would
		// delete every note on every other device.
		return fmt.Errorf("the notes folder %s looks empty, so nothing was synced (to avoid deleting your notes on your other devices)", r.e.Vault.Root)
	}
	for rel, f := range r.cur {
		en, ok := r.own[rel]
		if ok && !en.D && en.H == f.H {
			continue
		}
		next := Entry{H: f.H, S: f.S, M: f.M}
		if ok {
			next.P = withHistory(en.P, en.H)
		}
		r.own[rel] = next
		r.changed[rel] = true
	}
	nowMs := r.now.UnixMilli()
	for rel, en := range r.own {
		if en.D || r.present[rel] {
			continue
		}
		r.own[rel] = Entry{H: en.H, S: en.S, M: nowMs, D: true, P: en.P}
		r.changed[rel] = true
	}
	return nil
}

// mergePeer brings in what one peer changed since this device last merged it.
func (r *round) mergePeer(id string) {
	ps := r.st.Peers[id]
	if ps == nil {
		ps = &peerState{Base: map[string]Entry{}}
		r.st.Peers[id] = ps
	}
	if ps.Base == nil {
		ps.Base = map[string]Entry{}
	}
	m, err := r.k.readManifest(r.l, id)
	if err != nil {
		r.peersOK = false
		switch {
		case errors.Is(err, fs.ErrNotExist) && placeholder(r.l.manifest(id)):
			ps.Issue = CodeNotDownloaded
		case errors.Is(err, fs.ErrNotExist):
			ps.Issue = "starting" // a device mid-way through its first publish
		default:
			ps.Issue = CodeNotDownloaded // half-downloaded, or damaged
		}
		return
	}
	if m.Seq < ps.Seq {
		// The cloud drive handed back an older copy than one already merged.
		r.peersOK = false
		ps.Issue = CodeNotDownloaded
		return
	}
	ps.Issue = ""
	ps.Seq, ps.Name, ps.Updated = m.Seq, m.DeviceName, m.Updated
	r.manifests[id] = m

	paths := make([]string, 0, len(m.Entries)+len(ps.Base))
	for p := range m.Entries {
		paths = append(paths, p)
	}
	for p := range ps.Base {
		if _, ok := m.Entries[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, rel := range paths {
		pe, havePeer := m.Entries[rel]
		be, haveBase := ps.Base[rel]
		if !havePeer {
			delete(ps.Base, rel) // the peer forgot an old tombstone
			continue
		}
		if haveBase && sameEntry(pe, be) {
			continue
		}
		if !Syncable(rel) {
			continue // not something this device accepts from anyone
		}
		done, err := r.mergePath(id, rel, pe)
		if err != nil {
			log.Printf("cloud sync: %s from %s: %v", rel, m.DeviceName, err)
		}
		if done {
			ps.Base[rel] = pe
		}
	}
}

var errChanged = errors.New("changed on this device during the round")

// mergePath reconciles one path against a peer's entry. done=false means
// "not settled, try again next round" (a blob not here yet, a file edited
// mid-round), which is never confused with a deletion.
func (r *round) mergePath(peer, rel string, pe Entry) (bool, error) {
	l, haveLocal := r.cur[rel]
	o, haveOwn := r.own[rel]

	if pe.D {
		if !haveLocal {
			return true, nil
		}
		if !(l.H == pe.H || pe.descends(l.H)) {
			return true, nil // edited here after the peer last saw it: the edit wins
		}
		if err := r.removeLocal(rel, l.H); err != nil {
			return false, err
		}
		r.own[rel] = Entry{H: l.H, S: l.S, M: pe.M, D: true, P: withHistory(pe.P, pe.H)}
		r.changed[rel] = true
		r.stats.Deleted++
		return true, nil
	}

	if haveLocal && l.H == pe.H {
		r.shareHistory(peer, rel, pe)
		return true, nil
	}

	if !haveLocal {
		if haveOwn && o.D && o.descends(pe.H) && pe.M <= o.M {
			return true, nil // the peer has not seen this device's deletion yet
		}
		return r.takePeer(rel, pe, "")
	}

	if pe.descends(l.H) {
		return r.takePeer(rel, pe, l.H) // a later version of what is here
	}
	if haveOwn && o.descends(pe.H) {
		return true, nil // the peer is behind; it will take this version
	}
	return r.concurrent(peer, rel, l, o, pe)
}

// takePeer writes the peer's version here. expect is the hash the local file
// must still have ("" = must not exist), so an edit made during the round is
// never overwritten.
func (r *round) takePeer(rel string, pe Entry, expect string) (bool, error) {
	content, err := r.fetch(pe.H)
	if err != nil {
		r.stats.Waiting++
		return false, nil
	}
	oldBody := ""
	if expect != "" && isNote(rel) {
		if old, err := r.readLocal(rel); err == nil {
			_, oldBody = splitRaw(string(old))
		}
	}
	if err := r.writeLocal(rel, content, pe.M, expect); err != nil {
		if errors.Is(err, errChanged) {
			return false, nil
		}
		r.stats.Failed++
		return false, err
	}
	next := Entry{H: pe.H, S: pe.S, M: pe.M, P: pe.P}
	if expect != "" {
		next.P = withHistory(pe.P, expect)
	}
	if isNote(rel) && pe.C != "" {
		if doc, err := r.fetch(pe.C); err == nil {
			_, body := splitRaw(string(content))
			if r.e.CRDT.Adopt(rel, oldBody, string(doc), body) == nil {
				next.C = pe.C
				r.adopted[rel] = true
			}
		}
	}
	r.own[rel] = next
	r.changed[rel] = true
	r.stats.Downloaded++
	return true, nil
}

// shareHistory runs when both sides already hold identical text (two devices
// that started from copies of one vault, say). Their CRDT documents were built
// separately and share no atom ids, so a later concurrent edit could only
// produce a conflict copy. The device with the higher id adopts the lower
// one's document, so the next concurrent edit merges.
func (r *round) shareHistory(peer, rel string, pe Entry) {
	if !isNote(rel) || pe.C == "" || peer > r.self || r.e.CRDT == nil {
		return
	}
	doc, err := r.fetch(pe.C)
	if err != nil {
		return
	}
	raw, err := r.readLocal(rel)
	if err != nil {
		return
	}
	_, body := splitRaw(string(raw))
	if !crdtstore.Mergeable(rel, body) || r.e.CRDT.SharesHistory(rel, body, string(doc)) {
		return
	}
	if r.e.CRDT.Adopt(rel, body, string(doc), body) == nil {
		en := r.own[rel]
		en.C = pe.C
		r.own[rel] = en
		r.adopted[rel] = true
		r.changed[rel] = true
	}
}

// concurrent handles real concurrent edits: both sides changed the file since
// they last agreed. Notes whose CRDT documents share history are merged;
// everything else keeps both versions, the newer at the path and the older as
// a conflict copy, and nothing is lost either way.
func (r *round) concurrent(peer, rel string, l localFile, o Entry, pe Entry) (bool, error) {
	peerContent, err := r.fetch(pe.H)
	if err != nil {
		r.stats.Waiting++
		return false, nil
	}
	localContent, err := r.readLocal(rel)
	if err != nil || r.k.name(localContent) != l.H {
		return false, nil
	}

	if isNote(rel) && pe.C != "" && r.e.CRDT != nil {
		lPrefix, lBody := splitRaw(string(localContent))
		pPrefix, pBody := splitRaw(string(peerContent))
		if crdtstore.Mergeable(rel, lBody) && crdtstore.Mergeable(rel, pBody) {
			doc, derr := r.fetch(pe.C)
			if derr != nil {
				r.stats.Waiting++
				return false, nil
			}
			if r.e.CRDT.SharesHistory(rel, lBody, string(doc)) {
				merged, err := r.e.CRDT.Merge(rel, lBody, string(doc))
				if err == nil {
					// Frontmatter is not merged character by character; the
					// newer side's block wins, decided the same way on both
					// devices so they converge.
					prefix := lPrefix
					if peerNewer(pe.M, l.M, peer, r.self) {
						prefix = pPrefix
					}
					out := []byte(prefix + merged)
					mt := max(l.M, pe.M)
					if err := r.writeLocal(rel, out, mt, l.H); err != nil {
						if errors.Is(err, errChanged) {
							return false, nil
						}
						r.stats.Failed++
						return false, err
					}
					hist := withHistory(append(append([]string{}, o.P...), pe.P...), l.H, pe.H)
					r.own[rel] = Entry{H: r.k.name(out), S: int64(len(out)), M: mt, P: hist}
					r.changed[rel] = true
					r.stats.Merged++
					return true, nil
				}
			}
		}
	}

	if !peerNewer(pe.M, l.M, peer, r.self) {
		// This device's version is the newer one. The peer reaches the same
		// verdict and keeps its own version as the conflict copy, so exactly
		// one copy is made.
		return true, nil
	}
	copyRel := gsync.ConflictNameUnless(rel, func(c string) bool {
		if r.present[c] {
			return true
		}
		if _, ok := r.own[c]; ok {
			return true
		}
		p, err := r.e.Vault.SafeRawPath(c)
		if err != nil {
			return true
		}
		_, statErr := os.Stat(p)
		return statErr == nil
	})
	if err := r.writeLocal(copyRel, localContent, l.M, ""); err != nil {
		r.stats.Failed++
		return false, err
	}
	r.own[copyRel] = Entry{H: l.H, S: l.S, M: l.M}
	r.changed[copyRel] = true
	if err := r.writeLocal(rel, peerContent, pe.M, l.H); err != nil {
		if errors.Is(err, errChanged) {
			return false, nil
		}
		r.stats.Failed++
		return false, err
	}
	next := Entry{H: pe.H, S: pe.S, M: pe.M, P: withHistory(pe.P, l.H)}
	if isNote(rel) && pe.C != "" && r.e.CRDT != nil {
		// Take the peer's document with its text, so the next concurrent
		// edit between the two can merge instead of copying again.
		if doc, err := r.fetch(pe.C); err == nil {
			_, lBody := splitRaw(string(localContent))
			_, pBody := splitRaw(string(peerContent))
			if r.e.CRDT.Adopt(rel, lBody, string(doc), pBody) == nil {
				next.C = pe.C
				r.adopted[rel] = true
			}
		}
	}
	r.own[rel] = next
	r.changed[rel] = true
	r.stats.Conflicts++
	return true, nil
}

// peerNewer is the one tie-break every device applies identically: later
// mtime wins, then the larger device id.
func peerNewer(peerM, localM int64, peer, self string) bool {
	if peerM != localM {
		return peerM > localM
	}
	return peer > self
}

func (r *round) fetch(name string) ([]byte, error) {
	return r.k.readBlob(r.l, r.idx, r.devices, name)
}

func (r *round) readLocal(rel string) ([]byte, error) {
	p, err := r.e.Vault.SafeRawPath(rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// writeLocal places content at rel, provided the file still holds expect
// (or, with expect "", still does not exist).
func (r *round) writeLocal(rel string, content []byte, mtimeMs int64, expect string) error {
	if !Syncable(rel) {
		return fmt.Errorf("refusing to write %q", rel)
	}
	p, err := r.e.Vault.SafeRawPath(rel)
	if err != nil {
		return err
	}
	old, readErr := os.ReadFile(p)
	switch {
	case expect == "" && readErr == nil:
		return errChanged
	case expect != "" && (readErr != nil || r.k.name(old) != expect):
		return errChanged
	}
	if readErr == nil && isNote(rel) && r.e.Snapshot != nil {
		_, body := splitRaw(string(old))
		r.e.Snapshot(rel, body)
	}
	if err := writeAtomic(p, content, 0o644); err != nil {
		return err
	}
	if mtimeMs > 0 {
		t := time.UnixMilli(mtimeMs)
		_ = os.Chtimes(p, t, t)
	}
	if info, err := os.Stat(p); err == nil {
		h := r.k.name(content)
		r.st.Cache[rel] = fileCache{MtimeNs: info.ModTime().UnixNano(), Size: info.Size(), H: h}
		r.cur[rel] = localFile{H: h, S: info.Size(), M: info.ModTime().UnixMilli()}
		r.present[rel] = true
	}
	if isNote(rel) && r.e.Index != nil {
		if _, err := r.e.Index.Upsert(rel); err != nil {
			log.Printf("cloud sync: indexing %s: %v", rel, err)
		}
	}
	return nil
}

func (r *round) removeLocal(rel, expect string) error {
	p, err := r.e.Vault.SafeRawPath(rel)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(p)
	if err != nil || r.k.name(old) != expect {
		return errChanged
	}
	if isNote(rel) && r.e.Trash != nil {
		err = r.e.Trash(rel)
	} else {
		err = os.Remove(p)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if isNote(rel) {
		if r.e.Index != nil {
			_ = r.e.Index.Remove(rel)
		}
		if r.e.CRDT != nil {
			_ = r.e.CRDT.DeleteDoc(rel)
		}
	}
	delete(r.cur, rel)
	delete(r.present, rel)
	delete(r.st.Cache, rel)
	return nil
}

// publish uploads what this device has that the folder does not, then writes
// its manifest. Blobs first: a manifest must never name a blob that is not
// there yet.
func (r *round) publish() error {
	self := r.self
	upload := func(content []byte) (string, error) {
		name := r.k.name(content)
		if len(r.idx[name]) > 0 {
			return name, nil // already in the folder, under any device
		}
		if _, err := r.k.writeBlob(r.l, self, content); err != nil {
			return "", errf(CodeUnwritable, err.Error())
		}
		r.idx[name] = append(r.idx[name], self)
		r.stats.Uploaded++
		return name, nil
	}

	rels := make([]string, 0, len(r.own))
	for rel := range r.own {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		en := r.own[rel]
		if en.D {
			continue
		}
		needDoc := isNote(rel) && r.changed[rel] && !r.adopted[rel]
		missing := len(r.idx[en.H]) == 0 || (en.C != "" && len(r.idx[en.C]) == 0)
		if !r.changed[rel] && !missing {
			continue
		}
		content, err := r.readLocal(rel)
		if err != nil || r.k.name(content) != en.H {
			// Edited since the scan: publish it next round instead.
			if prev, ok := r.st.Published[rel]; ok {
				r.own[rel] = prev
			} else {
				delete(r.own, rel)
			}
			continue
		}
		if _, err := upload(content); err != nil {
			return err
		}
		if isNote(rel) && r.e.CRDT != nil && (needDoc || (en.C != "" && len(r.idx[en.C]) == 0)) {
			en.C = ""
			_, body := splitRaw(string(content))
			if crdtstore.Mergeable(rel, body) {
				if doc, err := r.e.CRDT.BodyDocJSON(rel, body); err == nil {
					if name, err := upload([]byte(doc)); err == nil {
						en.C = name
					} else {
						return err
					}
				}
			}
		}
		r.own[rel] = en
	}

	// Tombstones are kept long enough for every device to see them and for a
	// deleted note to stay restorable, then dropped.
	cutoff := r.now.Add(-tombstoneTTL).UnixMilli()
	for rel, en := range r.own {
		if en.D && en.M < cutoff {
			delete(r.own, rel)
		}
	}

	changed := len(r.own) != len(r.st.Published)
	if !changed {
		for rel, en := range r.own {
			if prev, ok := r.st.Published[rel]; !ok || prev.H != en.H || prev.D != en.D || prev.C != en.C || prev.M != en.M {
				changed = true
				break
			}
		}
	}
	heartbeat := r.now.Sub(time.UnixMilli(r.st.LastManifest)) > heartbeatEvery
	if changed || heartbeat || r.st.Seq == 0 {
		r.st.Seq++
		m := &Manifest{Version: Version, DeviceID: self, DeviceName: r.name,
			Instance: r.st.Instance, Seq: r.st.Seq, Updated: r.now.UnixMilli(), Entries: r.own}
		if err := r.k.writeManifest(r.l, m); err != nil {
			r.st.Seq--
			return errf(CodeUnwritable, err.Error())
		}
		r.st.LastManifest = r.now.UnixMilli()
		r.manifests[self] = m
	}
	r.st.Published = r.own
	return nil
}

// gc removes this device's own blobs that no manifest references any more.
// Only when every peer's manifest was read this round: a reference this
// device cannot see is a reference it must assume exists.
func (r *round) gc() {
	if !r.peersOK || r.now.Sub(time.UnixMilli(r.st.LastGC)) < gcEvery {
		return
	}
	r.st.LastGC = r.now.UnixMilli()
	cleanTemp(r.l.device(r.self), time.Hour)
	refs := map[string]bool{}
	add := func(es map[string]Entry) {
		for _, en := range es {
			refs[en.H] = true
			if en.C != "" {
				refs[en.C] = true
			}
		}
	}
	add(r.own)
	for _, m := range r.manifests {
		add(m.Entries)
	}
	root := filepath.Join(r.l.device(r.self), blobsName)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !blobNameRE.MatchString(d.Name()) || refs[d.Name()] {
			return nil
		}
		if info, err := d.Info(); err == nil && r.now.Sub(info.ModTime()) > blobGrace {
			_ = os.Remove(p)
		}
		return nil
	})
}
