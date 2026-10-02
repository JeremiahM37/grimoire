package cloudsync

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Device-local state, all under .grimoire/cloudsync/, which never syncs.
//
//	key.json    the derived master key (0600). Stored so sync can run
//	            unattended; see SECURITY.md for what that trades.
//	state.json  this device's identity, what it last published, and per peer
//	            the last state it merged from that peer (the merge base).

type fileCache struct {
	MtimeNs int64  `json:"t"`
	Size    int64  `json:"s"`
	H       string `json:"h"`
}

type peerState struct {
	Seq     int64            `json:"seq"`
	Name    string           `json:"name"`
	Updated int64            `json:"updated"`
	Issue   string           `json:"issue,omitempty"`
	Base    map[string]Entry `json:"base"`
}

type state struct {
	DeviceID     string                `json:"device_id"`
	Instance     string                `json:"instance"`
	BackupID     string                `json:"backup_id"`
	Seq          int64                 `json:"seq"`
	LastManifest int64                 `json:"last_manifest"`
	Published    map[string]Entry      `json:"published"`
	Peers        map[string]*peerState `json:"peers"`
	Cache        map[string]fileCache  `json:"cache"`
	LastSync     int64                 `json:"last_sync"`
	LastAttempt  int64                 `json:"last_attempt"`
	LastError    *Error                `json:"last_error,omitempty"`
	LastStats    Stats                 `json:"last_stats"`
	LastGC       int64                 `json:"last_gc"`
}

type keyFile struct {
	BackupID string `json:"backup_id"`
	Key      string `json:"key"`
}

func (e *Engine) statePath() string { return filepath.Join(e.Dir, "state.json") }
func (e *Engine) keyPath() string   { return filepath.Join(e.Dir, "key.json") }

func (e *Engine) loadState() *state {
	st := &state{}
	if raw, err := os.ReadFile(e.statePath()); err == nil {
		_ = json.Unmarshal(raw, st) // a corrupt state file costs a full rescan, nothing more
	}
	if st.Published == nil {
		st.Published = map[string]Entry{}
	}
	if st.Peers == nil {
		st.Peers = map[string]*peerState{}
	}
	if st.Cache == nil {
		st.Cache = map[string]fileCache{}
	}
	if st.DeviceID == "" {
		st.newIdentity()
	}
	return st
}

// newIdentity gives this device a fresh id. Its old directory in the folder
// stays readable to everyone, so nothing is lost by changing it.
func (st *state) newIdentity() {
	st.DeviceID = randHex(16)
	st.Instance = randHex(8)
	st.Seq = 0
	st.LastManifest = 0
}

func (e *Engine) saveState(st *state) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(e.Dir, 0o700); err != nil {
		return err
	}
	return writeAtomic(e.statePath(), raw, 0o600)
}

func (e *Engine) loadKey() (*keyFile, *keys, error) {
	raw, err := os.ReadFile(e.keyPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, errf(CodeKeyMissing, "")
		}
		return nil, nil, err
	}
	var kf keyFile
	if json.Unmarshal(raw, &kf) != nil {
		return nil, nil, errf(CodeKeyMissing, "the stored key is unreadable")
	}
	master, err := base64.StdEncoding.DecodeString(kf.Key)
	if err != nil {
		return nil, nil, errf(CodeKeyMissing, "the stored key is unreadable")
	}
	k, err := keysFromMaster(master)
	if err != nil {
		return nil, nil, errf(CodeKeyMissing, err.Error())
	}
	return &kf, k, nil
}

// saveKey stores the master key with owner-only permissions, in a directory
// only the owner can enter.
func (e *Engine) saveKey(backupID string, k *keys) error {
	if err := os.MkdirAll(e.Dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(e.Dir, 0o700)
	raw, _ := json.Marshal(keyFile{BackupID: backupID,
		Key: base64.StdEncoding.EncodeToString(k.master)})
	if err := writeAtomic(e.keyPath(), raw, 0o600); err != nil {
		return err
	}
	return os.Chmod(e.keyPath(), 0o600)
}
