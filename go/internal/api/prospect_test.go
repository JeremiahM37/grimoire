package api

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/prospect"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Intention tests set the clock and the experiment flag, so they stay serial.

func pinClock(t *testing.T, at time.Time) {
	t.Helper()
	old := vault.Now
	vault.Now = func() time.Time { return at }
	t.Cleanup(func() { vault.Now = old })
}

func contextOf(t *testing.T, h http.Handler, query url.Values) string {
	t.Helper()
	w := do(t, h, "GET", "/api/memory/context?"+query.Encode(), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("context = %d: %s", w.Code, w.Body)
	}
	var out map[string]any
	decode(t, w, &out)
	s, _ := out["context"].(string)
	return s
}

// reminders returns the lines of a context that are reminders, so a test can
// tell a delivered intention from an ordinary fact that happens to share text.
func reminders(context string) []string {
	var out []string
	for _, line := range strings.Split(context, "\n") {
		if strings.Contains(line, "Reminder you set") {
			out = append(out, line)
		}
	}
	return out
}

func TestIntentionsAreRefusedWhenTheExperimentIsOff(t *testing.T) {
	_, h := testServer(t)
	w := do(t, h, "POST", "/api/memory", map[string]any{"text": "Freeze before the 20th", "topic": "ops", "cue": "term:freeze"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "GRIMOIRE_EXP=prospect") {
		t.Fatalf("flag off accepted a cue: %d %s", w.Code, w.Body)
	}
	w = do(t, h, "GET", "/api/memory/intentions", nil)
	var out map[string]any
	decode(t, w, &out)
	if out["enabled"] != false {
		t.Fatalf("listing claims enabled with the flag off: %v", out)
	}
}

func TestIntentionWritesAreValidated(t *testing.T) {
	restore := exp.Set("prospect")
	defer restore()
	_, h := testServer(t)
	for name, body := range map[string]map[string]any{
		"malformed cue":      {"text": "x", "topic": "ops", "cue": "file:/etc/passwd"},
		"life without a cue": {"text": "x", "topic": "ops", "life": "once"},
		"bad life":           {"text": "x", "topic": "ops", "cue": "term:x", "life": "forever"},
		"untrusted origin":   {"text": "x", "topic": "ops", "cue": "term:x", "origin": "connector:slack:C1"},
		"private intention":  {"text": "x", "topic": "ops", "cue": "term:x", "visibility": "private"},
	} {
		if w := do(t, h, "POST", "/api/memory", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
}

func TestIntentionIsHeldFromOrdinaryRecallAndFiresOnlyWhenItsCueHolds(t *testing.T) {
	restore := exp.Set("prospect")
	defer restore()
	pinClock(t, time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC))
	_, h := testServer(t)
	remember(t, h, map[string]any{"text": "PROSPECT_MARK kestrel build needs a fresh cache", "topic": "ops", "cue": "term:cronjob"})

	if got := contextOf(t, h, url.Values{"q": {"kestrel build"}}); strings.Contains(got, "PROSPECT_MARK") {
		t.Fatalf("a cued fact came back as ordinary recall: %s", got)
	}
	got := contextOf(t, h, url.Values{"q": {"run the cronjob now"}})
	if lines := reminders(got); len(lines) != 1 || !strings.Contains(lines[0], "PROSPECT_MARK") {
		t.Fatalf("the cue held but no reminder came back: %s", got)
	}
}

func TestActionStageFiresOnCommandAndPathWithoutHookChanges(t *testing.T) {
	restore := exp.Set("prospect")
	defer restore()
	pinClock(t, time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC))
	_, h := testServer(t)
	remember(t, h, map[string]any{"text": "CMD_MARK compose down stops Jellyfin", "topic": "ops", "cue": "cmd:docker compose down"})
	remember(t, h, map[string]any{"text": "PATH_MARK staging DB is shared", "topic": "ops", "cue": "path:deploy/**"})

	cmd := contextOf(t, h, url.Values{"q": {"Bash docker compose down --volumes"}, "stage": {"action"}})
	if !strings.Contains(cmd, "CMD_MARK") || strings.Contains(cmd, "PATH_MARK") {
		t.Fatalf("command action: %s", cmd)
	}
	path := contextOf(t, h, url.Values{"q": {"Edit /home/me/repo/deploy/app.yml"}, "stage": {"action"}})
	if !strings.Contains(path, "PATH_MARK") || strings.Contains(path, "CMD_MARK") {
		t.Fatalf("path action: %s", path)
	}
	none := contextOf(t, h, url.Values{"q": {"Bash docker compose ps"}, "stage": {"action"}})
	if strings.Contains(none, "CMD_MARK") {
		t.Fatalf("a different command fired: %s", none)
	}
}

func TestOnceIntentionRetiresOnlyAfterAnAcknowledgedFiring(t *testing.T) {
	restore := exp.Set("prospect")
	defer restore()
	pinClock(t, time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC))
	server, h := testServer(t)
	first := remember(t, h, map[string]any{"text": "ONCE_MARK re-check the owner gate", "topic": "ops",
		"cue": "term:lectern", "life": "once"})
	ack := map[string]any{"path": first["path"], "id": first["id"]}

	if w := do(t, h, "POST", "/api/memory/intention/ack", ack); w.Code != http.StatusConflict {
		t.Fatalf("acknowledged before anything fired: %d %s", w.Code, w.Body)
	}
	query := url.Values{"q": {"restart lectern"}}
	if got := contextOf(t, h, query); len(reminders(got)) != 1 {
		t.Fatalf("once-intention did not fire: %s", got)
	}
	if got := contextOf(t, h, query); len(reminders(got)) != 1 {
		t.Fatalf("an unacknowledged once-intention stopped firing: %s", got)
	}
	if w := do(t, h, "POST", "/api/memory/intention/ack", ack); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"retired":true`) {
		t.Fatalf("ack after a firing: %d %s", w.Code, w.Body)
	}
	if got := contextOf(t, h, query); len(reminders(got)) != 0 {
		t.Fatalf("a retired once-intention fired again: %s", got)
	}
	var list map[string]any
	decode(t, do(t, h, "GET", "/api/memory/intentions", nil), &list)
	items := list["intentions"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["status"] != "retired" {
		t.Fatalf("listing: %v", list)
	}
	logged, err := os.ReadFile(server.prospectLogPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{`"event":"fired"`, `"event":"retired"`} {
		if !strings.Contains(string(logged), event) {
			t.Fatalf("log has no %s line: %s", event, logged)
		}
	}
}

func TestIntentionFiringIsLoggedAndCapped(t *testing.T) {
	restore := exp.Set("prospect")
	defer restore()
	pinClock(t, time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC))
	server, h := testServer(t)
	for _, mark := range []string{"CAP_A", "CAP_B", "CAP_C", "CAP_D"} {
		remember(t, h, map[string]any{"text": mark + " kestrel reminder", "topic": "ops", "cue": "term:kestrel"})
	}
	got := contextOf(t, h, url.Values{"q": {"kestrel"}})
	if n := len(reminders(got)); n != prospect.MaxFiresPerReq {
		t.Fatalf("%d reminders delivered, want %d: %s", n, prospect.MaxFiresPerReq, got)
	}
	data, err := os.ReadFile(server.prospectLogPath())
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), `"event":"fired"`); n != prospect.MaxFiresPerReq {
		t.Fatalf("%d firings logged, want %d", n, prospect.MaxFiresPerReq)
	}
}

func TestRecallHidesACuedFactUnlessItsCueHolds(t *testing.T) {
	restore := exp.Set("prospect")
	defer restore()
	pinClock(t, time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC))
	_, h := testServer(t)
	remember(t, h, map[string]any{"text": "RECALL_MARK kestrel note for the cron host", "topic": "ops", "cue": "term:cronjob"})
	recallText := func(q string) string {
		w := do(t, h, "GET", "/api/memory?q="+url.QueryEscape(q), nil)
		if w.Code != http.StatusOK {
			t.Fatalf("recall = %d: %s", w.Code, w.Body)
		}
		return w.Body.String()
	}
	if got := recallText("kestrel"); strings.Contains(got, "RECALL_MARK") {
		t.Fatalf("ordinary recall returned a cued fact: %s", got)
	}
	if got := recallText("cronjob kestrel"); !strings.Contains(got, "RECALL_MARK") {
		t.Fatalf("the cue held but recall did not return the fact: %s", got)
	}
}

func TestFlagOnWithoutIntentionsAnswersLikeFlagOff(t *testing.T) {
	// The flag must change nothing for a store with no intentions in it: every
	// ordinary context and recall answer is the same byte for byte.
	pinClock(t, time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC))
	_, h := testServer(t)
	remember(t, h, map[string]any{"text": "Kestrel build uses copper certificates", "topic": "kestrel"})
	remember(t, h, map[string]any{"text": "The kitchen cupboards are purple", "topic": "kitchen"})
	remember(t, h, map[string]any{"text": "Backups run nightly at three", "topic": "ops"})
	queries := []string{"kestrel build", "kitchen", "backups nightly", "continue", ""}
	answer := func() []string {
		var out []string
		for _, q := range queries {
			out = append(out, contextOf(t, h, url.Values{"q": {q}}))
			w := do(t, h, "GET", "/api/memory?q="+url.QueryEscape(q), nil)
			out = append(out, w.Body.String())
		}
		return out
	}
	off := answer()
	restore := exp.Set("prospect")
	on := answer()
	restore()
	for i := range off {
		if off[i] != on[i] {
			t.Fatalf("answer %d changed with the flag on and no intentions:\noff: %s\n on: %s", i, off[i], on[i])
		}
	}
}
