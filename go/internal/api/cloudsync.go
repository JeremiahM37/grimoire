package api

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/JeremiahM37/grimoire/go/internal/cloudsync"
)

// Sync through a folder a cloud drive syncs (Settings, Sync & backup).
//
// Every route here is adminOnly: setting it up hands the whole vault to a
// folder, the picker lists directories on this machine, and restore writes
// notes. The passphrase arrives once, in the setup body, is stretched into a
// key, and is never stored or echoed.

type cloudStatusOut struct {
	cloudsync.Status
	Suggestions []cloudsync.Suggestion `json:"suggestions"`
	Peer        *string                `json:"peer"`
	Home        string                 `json:"home"`
}

func (s *Server) cloudStatus() cloudStatusOut {
	home, _ := os.UserHomeDir()
	out := cloudStatusOut{Suggestions: cloudsync.Suggestions(home), Home: home}
	if out.Suggestions == nil {
		out.Suggestions = []cloudsync.Suggestion{}
	}
	if s.Cloud != nil {
		out.Status = s.Cloud.Status()
	}
	if out.Devices == nil {
		out.Devices = []cloudsync.DeviceStatus{}
	}
	if s.SyncPeer != "" {
		peer := s.SyncPeer
		out.Peer = &peer
	}
	return out
}

// cloudErr answers with the error's code and its plain message, so the
// console can show the message as written and key behaviour off the code.
func cloudErr(w http.ResponseWriter, err error) {
	e := cloudsync.AsError(err)
	status := http.StatusBadRequest
	switch e.Code {
	case cloudsync.CodeBusy:
		status = http.StatusConflict
	case cloudsync.CodeWrongPass:
		status = http.StatusUnauthorized
	case cloudsync.CodeNotFound:
		status = http.StatusNotFound
	case "error":
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, map[string]any{"detail": e.Error(), "code": e.Code, "message": e.Msg})
}

func (s *Server) cloudReady(w http.ResponseWriter) bool {
	if s.Cloud == nil {
		writeErr(w, http.StatusServiceUnavailable, "folder sync is not available on this server")
		return false
	}
	return true
}

func (s *Server) getCloudSync(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.cloudStatus())
}

func (s *Server) setupCloudSync(w http.ResponseWriter, r *http.Request) {
	if !s.cloudReady(w) {
		return
	}
	var in struct {
		Folder     string `json:"folder"`
		Passphrase string `json:"passphrase"`
		Create     bool   `json:"create"`
		DeviceName string `json:"device_name"`
		Interval   int    `json:"interval"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	created, err := s.Cloud.Setup(cloudsync.SetupOptions{Folder: in.Folder,
		Passphrase: in.Passphrase, Create: in.Create, DeviceName: in.DeviceName, Interval: in.Interval})
	if err != nil {
		cloudErr(w, err)
		return
	}
	// The first round runs now so the form can show a result; a large vault
	// keeps going in the background if it takes longer than the request.
	_, _ = s.Cloud.SyncOnce()
	out := s.cloudStatus()
	writeJSON(w, http.StatusOK, map[string]any{"created": created, "status": out})
}

func (s *Server) disableCloudSync(w http.ResponseWriter, _ *http.Request) {
	if !s.cloudReady(w) {
		return
	}
	if err := s.Cloud.Disable(); err != nil {
		cloudErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.cloudStatus())
}

func (s *Server) cloudSyncNow(w http.ResponseWriter, _ *http.Request) {
	if !s.cloudReady(w) {
		return
	}
	stats, err := s.Cloud.SyncOnce()
	if err != nil {
		cloudErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": stats, "status": s.cloudStatus()})
}

func (s *Server) cloudSyncOptions(w http.ResponseWriter, r *http.Request) {
	if !s.cloudReady(w) {
		return
	}
	var in struct {
		DeviceName string `json:"device_name"`
		Interval   int    `json:"interval"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := s.Cloud.UpdateOptions(in.DeviceName, in.Interval); err != nil {
		cloudErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.cloudStatus())
}

func (s *Server) probeCloudFolder(w http.ResponseWriter, r *http.Request) {
	if !s.cloudReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.Cloud.ProbeFolder(r.URL.Query().Get("path")))
}

func (s *Server) browseCloudFolder(w http.ResponseWriter, r *http.Request) {
	l, err := cloudsync.Browse(r.URL.Query().Get("path"))
	if err != nil {
		cloudErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) cloudDeleted(w http.ResponseWriter, _ *http.Request) {
	if !s.cloudReady(w) {
		return
	}
	del, err := s.Cloud.Deleted()
	if err != nil {
		cloudErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": del})
}

func (s *Server) cloudRestore(w http.ResponseWriter, r *http.Request) {
	if !s.cloudReady(w) {
		return
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := s.Cloud.Restore(in.Path); err != nil {
		cloudErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored": in.Path})
}
