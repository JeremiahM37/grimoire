package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/cloudsync"
)

func cloudServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	cloudsync.DefaultKDF = cloudsync.KDFParams{Name: "argon2id", Time: 1, MemoryKiB: 64, Threads: 1}
	s, h := testServer(t)
	s.Cloud = cloudsync.New(s.Vault, s.Index, s.CRDT, s.Settings, filepath.Join(s.Vault.Root, ".grimoire"))
	return s, h
}

func TestFolderSyncSetupStatusAndErrors(t *testing.T) {
	s, h := cloudServer(t)
	folder := filepath.Join(t.TempDir(), "Dropbox", "Grimoire")
	os.MkdirAll(filepath.Dir(folder), 0o755)
	os.WriteFile(filepath.Join(s.Vault.Root, "hello.md"), []byte("hi\n"), 0o644)

	var st cloudStatusOut
	decode(t, do(t, h, "GET", "/api/sync/folder", nil), &st)
	if st.Enabled {
		t.Fatal("folder sync should start off")
	}

	var probe cloudsync.Probe
	decode(t, do(t, h, "GET", "/api/sync/folder/probe?path="+folder, nil), &probe)
	if probe.HasBackup || probe.Exists {
		t.Fatalf("probe of a new leaf = %+v", probe)
	}

	// joining a folder that holds no backup must not quietly start one
	w := do(t, h, "POST", "/api/sync/folder", map[string]any{"folder": folder, "passphrase": "long enough pass"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"folder_missing"`) {
		t.Fatalf("join missing = %d %s", w.Code, w.Body)
	}
	empty := t.TempDir()
	w = do(t, h, "POST", "/api/sync/folder", map[string]any{"folder": empty, "passphrase": "long enough pass"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"no_backup"`) {
		t.Fatalf("join empty = %d %s", w.Code, w.Body)
	}

	w = do(t, h, "POST", "/api/sync/folder", map[string]any{"folder": folder,
		"passphrase": "long enough pass", "create": true, "device_name": "Work laptop"})
	if w.Code != http.StatusOK {
		t.Fatalf("setup = %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "long enough pass") {
		t.Fatal("the passphrase must never be echoed")
	}
	decode(t, do(t, h, "GET", "/api/sync/folder", nil), &st)
	if !st.Enabled || st.LastSync == 0 || st.DeviceName != "Work laptop" || st.Error != nil {
		t.Fatalf("status after setup = %+v", st.Status)
	}
	if raw, _ := os.ReadFile(filepath.Join(s.Vault.Root, ".grimoire", "settings.json")); strings.Contains(string(raw), "long enough") {
		t.Fatal("the passphrase must not be written to settings.json")
	}

	if w := do(t, h, "POST", "/api/sync/folder/now", nil); w.Code != http.StatusOK {
		t.Fatalf("now = %d %s", w.Code, w.Body)
	}

	// a second server joining with the wrong passphrase
	_, h2 := cloudServer(t)
	w = do(t, h2, "POST", "/api/sync/folder", map[string]any{"folder": folder, "passphrase": "not the right one"})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "does not match") {
		t.Fatalf("wrong passphrase = %d %s", w.Code, w.Body)
	}

	if w := do(t, h, "DELETE", "/api/sync/folder", nil); w.Code != http.StatusOK {
		t.Fatalf("off = %d %s", w.Code, w.Body)
	}
	decode(t, do(t, h, "GET", "/api/sync/folder", nil), &st)
	if st.Enabled {
		t.Fatal("still on after DELETE")
	}
}

func TestFolderBrowseListsDirectoriesOnly(t *testing.T) {
	_, h := cloudServer(t)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Dropbox"), 0o755)
	os.MkdirAll(filepath.Join(root, ".hidden"), 0o755)
	os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644)
	var l cloudsync.Listing
	decode(t, do(t, h, "GET", "/api/sync/folder/browse?path="+root, nil), &l)
	if len(l.Dirs) != 1 || l.Dirs[0] != "Dropbox" {
		t.Fatalf("browse = %+v", l)
	}
}
