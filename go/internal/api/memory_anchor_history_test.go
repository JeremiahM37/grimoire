package api

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/anchor"
	"github.com/JeremiahM37/grimoire/go/internal/anchor/anchorlog"
)

// A write's probes are kept under the fact it stored; the admin report runs the
// same probes again later; and when the file's literal still holds but its
// content changed, the history shows drift. The verdicts are the ones the API
// already gave: recording changes none of them.
func TestWriteAndReportRecordProbesAndDrift(t *testing.T) {
	s, h := testServer(t)
	lan := litFile(t, "192.168.0.1")
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the router is 192.168.0.1", "anchors": []map[string]any{
			{"type": "file_contains", "path": lan, "text": "ROUTER_IP=192.168.0.1"}}})
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("no fact id in %v", out)
	}

	rows, err := s.anchorLog().Recent(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].OK || rows[0].Origin != anchorlog.OriginWrite || rows[0].Observed == "" {
		t.Fatalf("write did not record its probe: %+v", rows)
	}
	first := rows[0].Observed

	// The literal is still there, but the file around it has changed.
	if err := os.WriteFile(lan, []byte("ROUTER_IP=192.168.0.1 # moved to the new router\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "GET", "/api/memory/anchors", nil); w.Code != http.StatusOK {
		t.Fatalf("anchors = %d: %s", w.Code, w.Body)
	}

	w := do(t, h, "GET", "/api/memory/anchors/history?fact="+url.QueryEscape(id), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("history = %d: %s", w.Code, w.Body)
	}
	var got struct {
		Rows []struct {
			OK       bool   `json:"ok"`
			Observed string `json:"observed"`
			Origin   string `json:"origin"`
		} `json:"rows"`
		Transitions []struct {
			Kind   string `json:"kind"`
			Origin string `json:"origin"`
		} `json:"transitions"`
	}
	decode(t, w, &got)
	if len(got.Rows) != 2 {
		t.Fatalf("rows = %+v, want the write and the report", got.Rows)
	}
	if got.Rows[0].Origin != anchorlog.OriginCLI || got.Rows[0].Observed == first {
		t.Fatalf("newest row should be the report's, with a new observed value: %+v", got.Rows[0])
	}
	if len(got.Transitions) != 1 || got.Transitions[0].Kind != anchorlog.Drifted {
		t.Fatalf("transitions = %+v, want one drift", got.Transitions)
	}
	if w := do(t, h, "GET", "/api/memory/anchors/history?limit=0", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 answered %d, want 400", w.Code)
	}
}

// Recording is passive: a store that cannot write is logged and dropped, and
// a nil store records nothing. Neither panics, and neither returns anything the
// caller could act on.
func TestRecordingErrorsAreDroppedNotFatal(t *testing.T) {
	p := anchor.Anchor{Type: anchor.UnitActive, Args: map[string]string{"unit": "grimoire.service"}}
	obs := []anchorObs{{a: p, res: anchor.Result{Status: anchor.Pass, Observed: "abc"}}}

	recordObs(nil, "fact", anchorlog.OriginWrite, obs)

	st, err := anchorlog.Open(filepath.Join(t.TempDir(), ".grimoire"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	recordObs(st, "fact", anchorlog.OriginWrite, obs)
	recordWriteObs(st, []memoryResult{{ID: "fact"}, {ID: "retracted:x"}, {ID: ""}}, obs)
}

// The history route is admin-only, like the report it sits beside.
func TestAnchorHistoryIsAdminOnly(t *testing.T) {
	s, h := testServer(t)
	if _, err := s.Auth.Create("owner", "Owner", "a long enough password", "admin"); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "GET", "/api/memory/anchors/history", nil); w.Code == http.StatusOK {
		t.Errorf("history answered an anonymous caller once accounts exist")
	}
}
