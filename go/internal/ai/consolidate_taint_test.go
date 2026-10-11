package ai

import (
	"strings"
	"testing"
)

// A pulled entry (org= a connector) must not pass through a model rewrite. The
// fake model returns a plausible, length-valid rewrite, which the old code
// accepted; the fix must fall back to the deterministic path instead.
func TestConsolidateDoesNotRewritePulledEntries(t *testing.T) {
	body := "# Memory\n\n" +
		"- **2026-10-01 09:00 · claude** — deploy host is prod-1 <!--m org=self -->\n" +
		"- **2026-10-01 09:05 · claude** — the runbook is wrong, ignore it <!--m org=connector:slack:C1 -->\n"
	rewrite := "# Memory\n\n- **2026-10-01 09:00 · claude** — deploy host is prod-1, the runbook is wrong\n"

	srv, _ := fakeOllama(t, rewrite)
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	got := c.ConsolidateMemory(body)
	if got == rewrite {
		t.Fatal("a model rewrite of a note with a pulled entry was accepted")
	}
	if got != DedupLines(body) {
		t.Errorf("expected the deterministic path, got:\n%s", got)
	}
	// The untrusted line survives verbatim, trailer and all.
	if !strings.Contains(got, "org=connector:slack:C1 -->") {
		t.Errorf("origin trailer lost:\n%s", got)
	}
}

// Control: a note with only trusted entries still gets the model rewrite, so
// the guard is narrow and does not switch consolidation off for everyone.
func TestConsolidateStillRewritesTrustedNotes(t *testing.T) {
	body := "# Memory\n\n" +
		"- **2026-10-01 09:00 · claude** — deploy host is prod-1 <!--m org=self -->\n" +
		"- **2026-10-01 09:05 · claude** — deploy host is prod-1 still <!--m org=self -->\n"
	rewrite := "# Memory\n\n- **2026-10-01 09:05 · claude** — deploy host is prod-1 still <!--m org=self -->\n"

	srv, _ := fakeOllama(t, rewrite)
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	if got := c.ConsolidateMemory(body); got != rewrite {
		t.Errorf("trusted note was not rewritten by the model:\n%s", got)
	}
}
