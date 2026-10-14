package api

import (
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/JeremiahM37/grimoire/go/internal/anchor"
	"github.com/JeremiahM37/grimoire/go/internal/anchor/anchorlog"
)

// Result history. Every anchor probe a caller runs is also written to the
// anchor history (internal/anchor/anchorlog): the verdict, a hash of what the
// probe saw, and the caller. Recording is passive. It is called after the probe
// has answered, it never changes a verdict or a response, and a failure to
// write is logged and dropped.

// anchorObs is one probe run and its result.
type anchorObs struct {
	a   anchor.Anchor
	res anchor.Result
}

// reportObs collects the probe runs behind a set of reports.
func reportObs(reports []anchorReport) []anchorObs {
	var out []anchorObs
	for _, rep := range reports {
		if rep.probe != nil {
			out = append(out, *rep.probe)
		}
	}
	return out
}

// recordObs writes the probe runs of one fact. A nil store (no vault) records
// nothing; a write error is logged and otherwise ignored.
func recordObs(st *anchorlog.Store, factID, origin string, obs []anchorObs) {
	if factID == "" {
		return
	}
	for _, o := range obs {
		if err := st.Record(factID, origin, o.a, o.res); err != nil {
			log.Printf("anchor history: %v", err)
		}
	}
}

// recordWriteObs records a write's probes under every fact it stored. A write
// runs its probes before the facts exist, so this is called once they do; the
// retracted marker is not a fact and is skipped.
func recordWriteObs(st *anchorlog.Store, results []memoryResult, obs []anchorObs) {
	for _, r := range results {
		if r.ID == "" || strings.HasPrefix(r.ID, "retracted:") {
			continue
		}
		recordObs(st, r.ID, anchorlog.OriginWrite, obs)
	}
}

// anchorLogState caches the store per vault, as the rules store does, so the
// database is opened once and not per probe.
var anchorLogState struct {
	sync.Mutex
	store *anchorlog.Store
	root  string
}

// anchorLog is the result history for this vault, or nil when there is no vault
// or the database cannot be opened. nil is safe to record into.
func (s *Server) anchorLog() *anchorlog.Store {
	if s.Vault == nil {
		return nil
	}
	anchorLogState.Lock()
	defer anchorLogState.Unlock()
	if anchorLogState.store != nil && anchorLogState.root == s.Vault.Root {
		return anchorLogState.store
	}
	st, err := anchorlog.Open(filepath.Join(s.Vault.Root, ".grimoire"))
	if err != nil {
		log.Printf("anchor history: %v", err)
		return nil
	}
	anchorLogState.store, anchorLogState.root = st, s.Vault.Root
	return st
}

// memoryAnchorHistory lists recent probe results and the changes between them:
// pass to fail, fail to pass, and drift (a pass whose observed value changed).
// GET /api/memory/anchors/history?fact=ID&limit=N. Admin only.
func (s *Server) memoryAnchorHistory(w http.ResponseWriter, r *http.Request) {
	st := s.anchorLog()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "no anchor history: no memory store")
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 5000 {
			writeErr(w, http.StatusBadRequest, "limit must be 1 to 5000")
			return
		}
		limit = n
	}
	rows, err := st.Recent(r.URL.Query().Get("fact"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":        rows,
		"transitions": anchorlog.Transitions(rows),
	})
}
