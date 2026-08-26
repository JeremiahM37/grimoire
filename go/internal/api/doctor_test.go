package api

import (
	"net/http"
	"testing"
)

// Each check must FIRE on the condition it exists for. A diagnostic that only
// ever reports ok is worse than none: it is a green light nobody earned.

func runDoctor(t *testing.T, h http.Handler) (string, map[string]Check) {
	t.Helper()
	w := do(t, h, "GET", "/api/doctor", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("doctor = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Status string  `json:"status"`
		Checks []Check `json:"checks"`
	}
	decode(t, w, &out)
	by := map[string]Check{}
	for _, c := range out.Checks {
		by[c.Name] = c
	}
	return out.Status, by
}

func TestDoctorPassesOnAHealthyInstance(t *testing.T) {
	_, h := testServer(t)
	status, checks := runDoctor(t, h)
	if len(checks) < 5 {
		t.Fatalf("only %d checks ran", len(checks))
	}
	for name, c := range checks {
		if c.Status == StatusFail {
			t.Errorf("%s failed on a healthy server: %s", name, c.Detail)
		}
		if c.Detail == "" {
			t.Errorf("%s reported no detail; an operator cannot act on a bare verdict", name)
		}
	}
	if status == string(StatusFail) {
		t.Errorf("overall status = %s on a healthy server", status)
	}
}

// The failure this command was written for: memory notes indexed, zero facts
// queryable, everything else green. Observed on a real deployment.
func TestDoctorCatchesUnqueryableMemory(t *testing.T) {
	s, h := testServer(t)
	if _, err := s.WriteNote("memory/2026-08-26.md",
		"# Memory\n\n- **2026-08-26 09:00 · agent** — the deploy host is prod.example <!--m id=abc123abc123-->\n",
		map[string]any{"memory": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
	if _, checks := runDoctor(t, h); checks["memory queryable"].Status != StatusOK {
		t.Fatalf("healthy memory reported as %s: %s",
			checks["memory queryable"].Status, checks["memory queryable"].Detail)
	}

	// Now reproduce the real fault: the facts table emptied while the notes
	// stay indexed. Nothing errors; recall simply returns nothing.
	if err := s.Index.DB.Exec("DELETE FROM memory_entries"); err != nil {
		t.Fatal(err)
	}
	status, checks := runDoctor(t, h)
	c := checks["memory queryable"]
	if c.Status != StatusFail {
		t.Fatalf("unqueryable memory reported as %s — this is the exact failure "+
			"that shipped with /api/health saying ok:true", c.Status)
	}
	if c.Fix == "" {
		t.Error("the check says what is wrong but not what to do about it")
	}
	if status != string(StatusFail) {
		t.Errorf("overall status = %s despite a failing check", status)
	}
}

// Files on disk that the index cannot see are invisible to search, and read to
// a user as "the agent doesn't know that" rather than as a fault.
func TestDoctorCatchesIndexDrift(t *testing.T) {
	s, h := testServer(t)
	if _, err := s.WriteNote("a.md", "# A\n\nbody\n", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
	if _, checks := runDoctor(t, h); checks["index matches vault"].Status != StatusOK {
		t.Fatalf("a consistent index reported as %s: %s",
			checks["index matches vault"].Status, checks["index matches vault"].Detail)
	}
	if err := s.Index.DB.Exec("DELETE FROM notes"); err != nil {
		t.Fatal(err)
	}
	_, checks := runDoctor(t, h)
	c := checks["index matches vault"]
	if c.Status != StatusFail {
		t.Fatalf("an empty index over a non-empty vault reported as %s: %s",
			c.Status, c.Detail)
	}
	if c.Fix == "" {
		t.Error("no remedy offered for an empty index")
	}
}

// Every check must name a remedy when it is not ok, or it has moved the work
// rather than done it.
func TestEveryNonOKCheckOffersARemedy(t *testing.T) {
	s, h := testServer(t)
	if err := s.Index.DB.Exec("DELETE FROM notes"); err != nil {
		t.Fatal(err)
	}
	_, checks := runDoctor(t, h)
	for name, c := range checks {
		if c.Status != StatusOK && c.Fix == "" {
			t.Errorf("%s is %s with no fix offered: %s", name, c.Status, c.Detail)
		}
	}
}
