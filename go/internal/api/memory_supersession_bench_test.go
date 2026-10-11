package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/modelcheck"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// The supersession replay. An ordered list of updates is written to one slot,
// each with a valid_from day, and after every update the test asks the store
// three questions: what is the top recall result now, what is the top result
// for valid_at on each day, and what does as_of answer just before and just
// after the update. It is the offline form of MemoryAgentBench's
// FactConsolidation (survey row 28): it measures which supersession path
// fails, and it is not a claim about any other system.
//
// Two arms. The chain arm is agent updates only; each must replace the last.
// The correction arm has a person's fact in the middle; agent updates after it
// must be recorded alongside, so the top result stays the person's value until
// a person changes it.

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// replayStats counts checks. A check is one comparison between the store and
// the expectation the arm defines; first keeps the first few failures.
type replayStats struct {
	checks, fails int
	first         []string
}

func (r *replayStats) check(ok bool, format string, args ...any) {
	r.checks++
	if !ok {
		r.fails++
		if len(r.first) < 5 {
			r.first = append(r.first, fmt.Sprintf(format, args...))
		}
	}
}

func billingText(port string) string { return "Billing Postgres runs on port " + port }

// queryBilling asks recall about the slot with extra query parameters and
// returns every hit's text in order.
func (o *oracle) queryBilling(query string) ([]string, string) {
	code, raw := o.call("GET", "/api/memory?limit=20&q=Billing+Postgres+runs+on+port"+query, "", nil)
	if code != http.StatusOK {
		return nil, fmt.Sprintf("recall answered %d: %s", code, raw)
	}
	var hits []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &hits); err != nil {
		return nil, err.Error()
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Text)
	}
	return out, ""
}

// runReplay plays ports into one slot, one update per step. humanAt is the
// step a person writes (-1 for none); after it, agent updates are challenges.
// chain is the arm name, used in messages.
func runReplay(t *testing.T, arm string, ports []string, humanAt int) replayStats {
	t.Helper()
	root, err := os.MkdirTemp("", "replay-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local)
	o := &oracle{root: root, m: &modelcheck.Model{}, base: base}
	var clock int
	prev := vault.Now
	vault.Now = func() time.Time { return base.Add(time.Duration(clock) * time.Minute) }
	defer func() { vault.Now = prev }()
	if err := o.open(); err != nil {
		t.Fatal(err)
	}
	defer o.closeDB()

	// Update k is written at minute 2k+1 and the instant "after update k" is
	// minute 2k+2, so as_of there sees exactly updates 0..k.
	afterUpdate := func(k int) string { return base.Add(time.Duration(2*k+2) * time.Minute).Format(time.RFC3339) }
	day := func(k int) string { return base.AddDate(0, 0, 2*k).Add(12 * time.Hour).Format(time.RFC3339) }

	var st replayStats
	// expected is the top result the store should give while the belief is
	// current. A person's value stands against every later agent update.
	expected := func(k int) string {
		if humanAt >= 0 && k >= humanAt {
			return ports[humanAt]
		}
		return ports[k]
	}
	for k, port := range ports {
		agent, human := "claude", k == humanAt
		if human {
			agent = "me"
		}
		clock = 2*k + 1
		body := map[string]any{
			"text": billingText(port), "topic": "ops", "agent": agent,
			"valid_from": base.AddDate(0, 0, 2*k).Format("2006-01-02"),
		}
		if human {
			body["human"] = true
		}
		if code, raw := o.call("POST", "/api/memory", "", body); code != http.StatusCreated {
			t.Fatalf("%s: write %d answered %d: %s", arm, k, code, raw)
		}

		clock = 2*k + 2
		want := expected(k)
		top, msg := o.queryBilling("")
		if msg != "" {
			t.Fatalf("%s: %s", arm, msg)
		}
		st.check(len(top) > 0 && top[0] == billingText(want),
			"%s step %d: top recall %v, want %q", arm, k, top, billingText(want))

		// The current belief about today's validity: the value believed now
		// and valid on the day it was written.
		cur, msg := o.queryBilling("&valid_at=" + day(k))
		if msg != "" {
			t.Fatalf("%s: %s", arm, msg)
		}
		st.check(len(cur) == 1 && cur[0] == billingText(want),
			"%s step %d: valid_at now %v, want %q", arm, k, cur, billingText(want))

		// The combined question: what was believed just after update j about
		// what was true on day j. Update j's value is the answer. Once a
		// person has written, nothing an agent writes closes the person's
		// validity or the validity of the challengers, so they all stay
		// answers for later days: the store reports the disagreement.
		for j := 0; j <= k; j++ {
			want := []string{billingText(ports[j])}
			if humanAt >= 0 && j > humanAt {
				// Every agent update since the person's write is still a
				// believed challenger valid on day j, and so is the person's.
				want = []string{billingText(ports[humanAt])}
				for i := humanAt + 1; i <= j; i++ {
					want = append(want, billingText(ports[i]))
				}
			}
			got, msg := o.queryBilling("&include_challenges=1&as_of=" + afterUpdate(j) + "&valid_at=" + day(j))
			if msg != "" {
				t.Fatalf("%s: %s", arm, msg)
			}
			st.check(sameStrings(got, want),
				"%s step %d: as_of after %d with valid_at day %d gives %v, want %v", arm, k, j, j, got, want)
		}
	}
	return st
}

// TestSupersessionReplayChain: agent updates only. Each one must replace the
// last, and every past belief must still be answerable as_of its instant.
func TestSupersessionReplayChain(t *testing.T) {
	ports := []string{"5432", "6432", "6543", "5432", "6432", "6543", "5432", "6432"}
	st := runReplay(t, "chain", ports, -1)
	t.Logf("chain arm: %d checks, %d failures; first: %v", st.checks, st.fails, st.first)
	if st.fails > 0 {
		t.Fatalf("chain arm: %d of %d checks failed; first: %v", st.fails, st.checks, st.first)
	}
}

// TestSupersessionReplayCorrection: a person writes the value at step 2, and
// the agent writes after it. The agent's values must be recorded alongside,
// so the top result stays the person's until a person changes it.
func TestSupersessionReplayCorrection(t *testing.T) {
	ports := []string{"5432", "6432", "6543", "5432", "6432"}
	st := runReplay(t, "correction", ports, 2)
	t.Logf("correction arm: %d checks, %d failures; first: %v", st.checks, st.fails, st.first)
	if st.fails > 0 {
		t.Fatalf("correction arm: %d of %d checks failed; first: %v", st.fails, st.checks, st.first)
	}
}
