package api

// One timeline for everything an agent did.
//
// Three records already existed and none of them were ever joined: the read
// audit (which restricted documents were opened, and which were refused), the
// memory store (which facts an agent wrote, with the agent and task that wrote
// them), and the credential audit (which grants were minted and which calls
// were brokered with them). Each answers a different third of the same
// question, and answering it meant opening three screens and comparing
// timestamps by eye.
//
// This is assembly, not mechanism: no new collector, no new table, nothing
// sampled that was not already being recorded. It is the join that makes the
// existing rows legible as a sequence -- an agent read this, concluded that,
// and then spent a credential -- which is the shape the question is actually
// asked in.

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/readlog"
)

// Event is one thing that happened, from whichever record noticed it.
type Event struct {
	At    string `json:"at"`             // RFC3339 where the source gives it
	Kind  string `json:"kind"`           // read | memory | credential
	Actor string `json:"actor"`          // account, agent name, or grantee
	What  string `json:"what"`           // one line, already human-readable
	Path  string `json:"path,omitempty"` // note or URL, when there is one
	// Denied marks the events worth looking at first: a refused read, a
	// refused brokered call. Surfacing them as a flag rather than a separate
	// endpoint keeps them in sequence with whatever led up to them.
	Denied bool `json:"denied,omitempty"`
}

// timeline merges the three records, newest first.
//
// The credential leg is included only when the vault is unlocked, because
// those rows name secrets and brokered URLs. That is a soft omission on
// purpose: a locked vault should narrow what the page shows, not fail it --
// the read and memory legs are still the answer to most questions.
func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 1000)
	}
	kinds := map[string]bool{}
	for _, k := range strings.Split(r.URL.Query().Get("kind"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			kinds[k] = true
		}
	}
	want := func(k string) bool { return len(kinds) == 0 || kinds[k] }

	out := []Event{}
	if want("read") {
		out = append(out, s.readEvents(limit)...)
	}
	if want("memory") {
		out = append(out, s.memoryEvents(r, limit)...)
	}
	locked := false
	if want("credential") {
		ev, ok := s.credentialEvents(limit)
		out, locked = append(out, ev...), !ok
	}

	// Sort by the timestamp string. Every source writes RFC3339 or the
	// "2006-01-02 15:04" memory stamp, both of which sort lexically in time
	// order within their own format; parsing normalises across the two.
	sort.SliceStable(out, func(i, j int) bool {
		return eventTime(out[i].At).After(eventTime(out[j].At))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events": out,
		// Say when a leg is missing rather than showing a short list as if it
		// were the whole story. A timeline silently missing its credential
		// third is worse than one that says so.
		"credentials_hidden": locked,
	})
}

func eventTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (s *Server) readEvents(limit int) []Event {
	rows, err := s.Reads.Recent(readlog.Query{Limit: limit})
	if err != nil {
		return nil
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		actor := row.Name
		if actor == "" {
			actor = row.User
		}
		what := "opened " + row.Path
		if !row.Allowed {
			what = "was refused " + row.Path
		}
		out = append(out, Event{
			At: row.At, Kind: "read", Actor: actor,
			What: what, Path: row.Path, Denied: !row.Allowed})
	}
	return out
}

func (s *Server) memoryEvents(r *http.Request, limit int) []Event {
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter: filterFor(r, false), Limit: limit})
	if err != nil {
		return nil
	}
	out := make([]Event, 0, len(hits))
	for _, h := range hits {
		actor := h.Agent
		if actor == "" {
			actor = "an agent"
		}
		what := "remembered: " + h.Text
		if h.SupersededBy != "" {
			what = "corrected: " + h.Text
		}
		if h.Task != "" {
			what += "  (" + h.Task + ")"
		}
		out = append(out, Event{
			At: h.Stamp, Kind: "memory", Actor: actor, What: what, Path: h.Note})
	}
	return out
}

// credentialEvents returns the broker's audit rows. The bool reports whether
// they could be read at all -- false means the vault is locked, which is not
// an error and must not read as "nothing happened".
func (s *Server) credentialEvents(limit int) ([]Event, bool) {
	if s.Broker == nil {
		return nil, true
	}
	rows, err := s.Broker.Audit(limit)
	if err != nil {
		return nil, false
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		action, _ := row["action"].(string)
		secret, _ := row["secret"].(string)
		detail, _ := row["detail"].(string)
		ts, _ := row["ts"].(string)
		out = append(out, Event{
			At: ts, Kind: "credential", Actor: secret,
			What:   strings.TrimSpace(action + " " + detail),
			Denied: action == "denied",
		})
	}
	return out, true
}
