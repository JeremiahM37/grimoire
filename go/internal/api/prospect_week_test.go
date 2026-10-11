package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/prospect"
)

// The virtual-week fixture. Twelve intentions are planted, then sixty context
// loads are replayed at scripted times and situations. Each load states by hand
// which intentions must fire. Nothing here is computed by the code under test.
//
// Precision and recall are over (load, intention) pairs. Both must be 1.0.
// Acknowledgement steps sit between loads, so a once-intention is seen firing
// and unacknowledged, then acknowledged and gone.

type weekIntent struct {
	label, text, cue, life string
}

var weekIntents = []weekIntent{
	{"A1", "staging DB is shared, coordinate before migrating", "path:deploy/**", ""},
	{"A2", "docker compose down also stops the shared Jellyfin", "cmd:docker compose down", ""},
	{"A3", "release builds come from the GitHub tarball, not go build", "project:grimoire&term:release", ""},
	{"A4", "check whether the wildcard cert was renewed", "after:2026-11-01&term:cert", ""},
	{"A5", "schema freeze before the 20th, propose changes after", "before:2026-10-20&term:schema", ""},
	{"A6", "kestrel deployment needs copper certificates", "term:kestrel", ""},
	{"A7", "lectern env changed: re-check the owner gate", "path:/etc/lectern.env", "once"},
	{"A8", "restarting lectern can drop attached sessions", "cmd:systemctl restart", "once"},
	{"A9", "release checksum still pending for this tarball", "path:releases/**&project:grimoire", "until:2026-10-15"},
	{"A10", "the backup window moves when the DAS is scrubbing", "term:backup", "until:2026-10-14"},
	{"A11", "push needs a review of the release notes first", "cmd:git push&project:grimoire", "once"},
	{"A12", "static assets are cached: bump the build id after edits", "path:static/**", "until:2026-10-30"},
}

// weekLoad is one context request. act, when set, is the hook's action stage:
// the query is the tool and its target, and the path or command comes from it.
type weekLoad struct {
	at      string // YYYY-MM-DD, UTC 09:00
	q       string
	act     string
	project string
	want    []string
}

// weekStep is a load or an acknowledgement, in script order.
type weekStep struct {
	load *weekLoad
	ack  string // label to acknowledge
}

var weekScript = []weekStep{
	{load: &weekLoad{"2026-10-11", "what is the kestrel plan", "", "", []string{"A6"}}},
	{load: &weekLoad{"2026-10-11", "tell me about the release tarball", "", "other", nil}},
	{load: &weekLoad{"2026-10-11", "release tarball checksum", "", "grimoire", []string{"A3"}}},
	{load: &weekLoad{"2026-10-11", "", "Edit /home/me/repo/deploy/app.yml", "grimoire", []string{"A1"}}},
	{load: &weekLoad{"2026-10-11", "", "Bash docker compose down", "", []string{"A2"}}},
	{load: &weekLoad{"2026-10-11", "", "Bash docker compose ps", "", nil}},
	{load: &weekLoad{"2026-10-11", "", "Bash docker compose down --volumes", "", []string{"A2"}}},
	{load: &weekLoad{"2026-10-11", "", "Edit /home/me/repo/notdeploy/x.yml", "", nil}},
	{load: &weekLoad{"2026-10-11", "the backup job ran", "", "", []string{"A10"}}},
	{load: &weekLoad{"2026-10-11", "kestrel", "", "grimoire", []string{"A6"}}},
	{load: &weekLoad{"2026-10-12", "backup", "", "", []string{"A10"}}},
	{load: &weekLoad{"2026-10-12", "", "Edit /etc/lectern.env", "", []string{"A7"}}},
	{load: &weekLoad{"2026-10-12", "", "Edit /etc/lectern.env", "", []string{"A7"}}},
	{ack: "A7"},
	{load: &weekLoad{"2026-10-12", "", "Edit /etc/lectern.env", "", nil}},
	{load: &weekLoad{"2026-10-12", "", "Bash systemctl restart lectern", "", []string{"A8"}}},
	{load: &weekLoad{"2026-10-13", "", "Bash systemctl restart lectern", "", []string{"A8"}}},
	{ack: "A8"},
	{load: &weekLoad{"2026-10-13", "", "Bash systemctl restart lectern", "", nil}},
	{load: &weekLoad{"2026-10-13", "", "Bash git push origin main", "grimoire", []string{"A11"}}},
	{load: &weekLoad{"2026-10-13", "", "Bash git push origin main", "other", nil}},
	{load: &weekLoad{"2026-10-13", "", "Bash git push", "grimoire", []string{"A11"}}},
	{ack: "A11"},
	{load: &weekLoad{"2026-10-13", "", "Bash git push", "grimoire", nil}},
	{load: &weekLoad{"2026-10-14", "backup window", "", "", []string{"A10"}}},
	{load: &weekLoad{"2026-10-14", "", "Edit /home/me/repo/releases/v1.tar.gz", "grimoire", []string{"A9"}}},
	{load: &weekLoad{"2026-10-15", "", "Edit /home/me/repo/releases/v1.tar.gz", "grimoire", []string{"A9"}}},
	{load: &weekLoad{"2026-10-15", "backup", "", "", nil}},
	{load: &weekLoad{"2026-10-16", "", "Edit /home/me/repo/releases/v1.tar.gz", "grimoire", nil}},
	{load: &weekLoad{"2026-10-16", "", "Edit /home/me/repo/static/app.css", "", []string{"A12"}}},
	{load: &weekLoad{"2026-10-16", "kestrel", "", "", []string{"A6"}}},
	{load: &weekLoad{"2026-10-16", "cert", "", "", nil}},
	{load: &weekLoad{"2026-10-17", "schema change proposal", "", "", []string{"A5"}}},
	{load: &weekLoad{"2026-10-17", "kestrel", "", "other", []string{"A6"}}},
	{load: &weekLoad{"2026-10-19", "schema", "", "", []string{"A5"}}},
	{load: &weekLoad{"2026-10-20", "schema", "", "", nil}},
	{load: &weekLoad{"2026-10-20", "", "Edit /home/me/repo/static/app.css", "", []string{"A12"}}},
	{load: &weekLoad{"2026-10-25", "", "Edit /home/me/repo/deploy/prod.yml", "", []string{"A1"}}},
	{load: &weekLoad{"2026-10-25", "release", "", "grimoire", []string{"A3"}}},
	{load: &weekLoad{"2026-10-30", "", "Edit /home/me/repo/static/app.css", "", []string{"A12"}}},
	{load: &weekLoad{"2026-10-31", "", "Edit /home/me/repo/static/app.css", "", nil}},
	{load: &weekLoad{"2026-10-31", "cert", "", "", nil}},
	{load: &weekLoad{"2026-10-31", "kestrel", "", "", []string{"A6"}}},
	{load: &weekLoad{"2026-11-01", "check cert", "", "", []string{"A4"}}},
	{load: &weekLoad{"2026-11-01", "kestrel", "", "", []string{"A6"}}},
	{load: &weekLoad{"2026-11-02", "cert renewal", "", "", []string{"A4"}}},
	{load: &weekLoad{"2026-11-02", "", "Edit /home/me/repo/deploy/x.yml", "", []string{"A1"}}},
	{load: &weekLoad{"2026-11-02", "", "Bash docker compose down", "", []string{"A2"}}},
	{load: &weekLoad{"2026-11-02", "release notes", "", "grimoire", []string{"A3"}}},
	{load: &weekLoad{"2026-11-02", "", "Edit /etc/lectern.env", "", nil}},
	{load: &weekLoad{"2026-11-02", "kestrel and backup and cert", "", "grimoire", []string{"A4", "A6"}}},
	{load: &weekLoad{"2026-11-02", "kestrel cert", "", "other", []string{"A4", "A6"}}},
	{load: &weekLoad{"2026-10-11", "kestrel", "", "other", []string{"A6"}}},
	{load: &weekLoad{"2026-10-11", "", "Bash docker compose down", "grimoire", []string{"A2"}}},
	{load: &weekLoad{"2026-10-11", "", "Edit /home/me/repo/deploy/app.yml", "grimoire", []string{"A1"}}},
	{load: &weekLoad{"2026-10-13", "restart lectern", "", "", nil}},
	{load: &weekLoad{"2026-10-11", "", "", "", nil}},
	{load: &weekLoad{"2026-10-12", "", "", "grimoire", nil}},
	{load: &weekLoad{"2026-10-17", "", "Edit /tmp/notes.txt", "", nil}},
	{load: &weekLoad{"2026-10-11", "kestrel", "", "grimoire", []string{"A6"}}},
	{load: &weekLoad{"2026-10-11", "we should backup the database", "", "", []string{"A10"}}},
	{load: &weekLoad{"2026-10-15", "", "Edit /home/me/repo/releases/v2.tar.gz", "grimoire", []string{"A9"}}},
	{load: &weekLoad{"2026-10-15", "schema", "", "", []string{"A5"}}},
	{load: &weekLoad{"2026-11-02", "", "Bash git push", "grimoire", nil}},
}

func TestVirtualWeekFiresExactlyWhenTheCueHolds(t *testing.T) {
	restore := exp.Set("prospect")
	defer restore()
	pinClock(t, time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC))
	_, h := testServer(t)

	type planted struct{ path, id string }
	ids := map[string]planted{}
	for _, in := range weekIntents {
		body := map[string]any{"text": fmt.Sprintf("[%s] %s", in.label, in.text), "topic": "ops",
			"cue": in.cue}
		if in.life != "" {
			body["life"] = in.life
		}
		got := remember(t, h, body)
		ids[in.label] = planted{path: got["path"].(string), id: got["id"].(string)}
	}

	var loads, tp, fp, fn, pairs, positives int
	for _, step := range weekScript {
		if step.ack != "" {
			p := ids[step.ack]
			w := do(t, h, "POST", "/api/memory/intention/ack", map[string]any{"path": p.path, "id": p.id})
			if w.Code != http.StatusOK {
				t.Fatalf("ack %s = %d %s", step.ack, w.Code, w.Body)
			}
			continue
		}
		ld := step.load
		loads++
		at, err := time.Parse("2006-01-02", ld.at)
		if err != nil {
			t.Fatal(err)
		}
		pinClock(t, at.Add(9*time.Hour))
		query := url.Values{"q": {ld.q}}
		if ld.act != "" {
			query.Set("q", ld.act)
			query.Set("stage", "action")
		}
		if ld.project != "" {
			query.Set("intent_project", ld.project)
		}
		context := contextOf(t, h, query)
		fired := map[string]bool{}
		for _, line := range reminders(context) {
			for _, in := range weekIntents {
				if strings.Contains(line, "["+in.label+"]") {
					fired[in.label] = true
				}
			}
		}
		if n := len(reminders(context)); n > prospect.MaxFiresPerReq {
			t.Fatalf("load %d delivered %d reminders", loads, n)
		}
		// Anything carrying a label outside a reminder line is an ordinary item
		// leaking a cued fact: the gate failed, even if it fired by chance.
		for _, in := range weekIntents {
			if strings.Contains(context, "["+in.label+"]") && !fired[in.label] {
				t.Fatalf("load %d (%s %q): %s came back as ordinary recall", loads, ld.at, ld.q+ld.act, in.label)
			}
		}
		want := map[string]bool{}
		for _, l := range ld.want {
			want[l] = true
			positives++
		}
		for _, in := range weekIntents {
			pairs++
			switch {
			case fired[in.label] && want[in.label]:
				tp++
			case fired[in.label] && !want[in.label]:
				fp++
				t.Errorf("load %d (%s %q %q): %s fired, cue should not hold", loads, ld.at, ld.q, ld.act, in.label)
			case !fired[in.label] && want[in.label]:
				fn++
				t.Errorf("load %d (%s %q %q): %s did not fire, cue holds", loads, ld.at, ld.q, ld.act, in.label)
			}
		}
	}
	precision := float64(tp) / float64(max(tp+fp, 1))
	recall := float64(tp) / float64(max(tp+fn, 1))
	t.Logf("virtual week: %d loads, %d intentions, %d (load,intention) pairs, %d expected firings",
		loads, len(weekIntents), pairs, positives)
	t.Logf("tp=%d fp=%d fn=%d precision=%.3f recall=%.3f", tp, fp, fn, precision, recall)
	if fp != 0 || fn != 0 || tp != positives {
		t.Fatalf("precision %.3f recall %.3f", precision, recall)
	}
	if loads < 60 {
		t.Fatalf("the fixture has %d loads, want about 60", loads)
	}
}
