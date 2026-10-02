package cloudsync

import (
	"errors"
	"io/fs"
	"os"
	"sort"
)

// Restoring a deleted note from the folder.
//
// A deletion travels as a tombstone that keeps the last content hash, and a
// blob stays in the folder while any manifest names it, so for as long as
// tombstones are kept (90 days) a deleted note is still in the backup and
// restoring it costs one blob read.

// DeletedNote is a file deleted on some device that the backup still holds.
type DeletedNote struct {
	Path      string `json:"path"`
	DeletedAt int64  `json:"deleted_at"` // unix ms
	Device    string `json:"device"`
	Size      int64  `json:"size"`
}

// Deleted lists deleted files that are not on this device now, newest first.
func (e *Engine) Deleted() ([]DeletedNote, error) {
	l, k, _, err := e.ready()
	if err != nil {
		return nil, err
	}
	devices, err := listDevices(l)
	if err != nil {
		return nil, err
	}
	best := map[string]DeletedNote{}
	live := map[string]bool{}
	for _, id := range devices {
		m, err := k.readManifest(l, id)
		if err != nil {
			continue
		}
		for rel, en := range m.Entries {
			if !en.D {
				live[rel] = true
				continue
			}
			if cur, ok := best[rel]; !ok || en.M > cur.DeletedAt {
				best[rel] = DeletedNote{Path: rel, DeletedAt: en.M, Device: m.DeviceName, Size: en.S}
			}
		}
	}
	out := []DeletedNote{}
	for rel, d := range best {
		if live[rel] {
			continue // deleted on one device, but another still has it
		}
		if p, err := e.Vault.SafeRawPath(rel); err == nil {
			if _, err := os.Stat(p); err == nil {
				continue
			}
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeletedAt > out[j].DeletedAt })
	return out, nil
}

// Restore writes a deleted file's last version back into the vault. The next
// round publishes it, and because it is newer than the deletion, every other
// device brings it back too.
func (e *Engine) Restore(rel string) error {
	if !Syncable(rel) {
		return errf(CodeNotFound, rel)
	}
	l, k, _, err := e.ready()
	if err != nil {
		return err
	}
	p, err := e.Vault.SafeRawPath(rel)
	if err != nil {
		return errf(CodeNotFound, rel)
	}
	if _, err := os.Stat(p); err == nil {
		return errf(CodeAlreadyPresent, rel)
	}
	devices, err := listDevices(l)
	if err != nil {
		return err
	}
	var hash string
	var at int64
	for _, id := range devices {
		m, err := k.readManifest(l, id)
		if err != nil {
			continue
		}
		if en, ok := m.Entries[rel]; ok && en.D && en.M >= at {
			hash, at = en.H, en.M
		}
	}
	if hash == "" {
		return errf(CodeNotFound, rel)
	}
	content, err := k.readBlob(l, buildBlobIndex(l, devices), devices, hash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errf(CodeNotFound, rel)
		}
		return errf(CodeNotDownloaded, "")
	}
	if err := writeAtomic(p, content, 0o644); err != nil {
		return err
	}
	if isNote(rel) && e.Index != nil {
		_, _ = e.Index.Upsert(rel)
	}
	e.Kick()
	return nil
}
