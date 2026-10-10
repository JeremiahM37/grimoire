package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// GET /api/memory/stream — what agents learn and change, as it happens.
//
// The changes digest answers "what moved since Monday" on request. This is the
// same question asked continuously: a Server-Sent Events feed of the same
// classification (beliefChanges in memory_changes.go), so a stream and a digest
// cannot disagree about what happened. There is deliberately no second log.
//
// Event kinds:
//
//	memory.added       a new fact, replacing nothing
//	memory.superseded  a fact replaced an earlier one (carries both texts)
//	memory.forgotten   a fact was retracted, or ran out of its time-to-live
//	memory.challenged  an agent's claim was refused supersession and is in dispute
//	memory.disputed    a fact an agent is disputing, seen from the fact's side
//
// Two rules are load-bearing:
//
//   - Access is decided per event, at emit time. The query runs through the
//     caller's filter, and every event is checked again with canRead before it
//     is written. An event the caller may not read is dropped whole: not its
//     text, not its id, not its path, since a path alone reveals a note exists.
//   - The event id is "<stamp>|<event>|<entry id>". Stamps have minute
//     resolution, so Last-Event-ID resumes from the start of the minute it names:
//     nothing is skipped, and a client may receive some events of that minute
//     again. It should dedupe by id, which is what the id is for. Within a
//     connection the stream remembers what it sent in the current minute, so a
//     late event with an earlier key in the same minute is still delivered.
//   - Known limit: the stream reads state, not a log, so a fact that is added and
//     superseded between two polls appears only as superseded. Polls are one
//     second apart. A correction can show as an add followed by a supersession
//     when a poll lands between the two index writes. Events older than the
//     changes scan limit (2000 facts) are not replayed.

// Event names, as they appear on the wire.
const (
	evAdded      = "memory.added"
	evSuperseded = "memory.superseded"
	evForgotten  = "memory.forgotten"
	evChallenged = "memory.challenged"
	evDisputed   = "memory.disputed"
)

// Poll and heartbeat intervals are variables so a test can run a stream in
// milliseconds. Nothing else changes them.
var (
	streamPoll      = time.Second
	streamHeartbeat = 25 * time.Second
)

// memEvent is one change, ready to put on the wire.
type memEvent struct {
	Key   string `json:"-"` // the SSE id: at|event|entry id
	At    string `json:"at"`
	Event string `json:"event"`

	ID      string `json:"id"`
	Path    string `json:"path,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
	Text    string `json:"text,omitempty"`

	// Context for the kinds that relate two facts. Each is set only when the
	// fact it names is readable by the caller.
	ReplacesID   string `json:"replaces_id,omitempty"`
	ReplacedText string `json:"replaced_text,omitempty"`
	ChallengerID string `json:"challenger_id,omitempty"`
	ContestedID  string `json:"contested_id,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

func eventKey(at, event, id string) string { return at + "|" + event + "|" + id }

// streamEvents is the changes classification, shaped as stream events, sorted by
// key. Everything it returns has passed canRead.
func (s *Server) streamEvents(r *http.Request, since, now time.Time) ([]memEvent, error) {
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter:            filterFor(r, true),
		Agent:             strings.TrimSpace(r.URL.Query().Get("agent")),
		Session:           strings.TrimSpace(r.URL.Query().Get("session")),
		IncludeSuperseded: true,
		IncludeExpired:    true,
		Now:               now,
		Limit:             changeScanLimit,
	})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]index.MemoryHit, len(hits))
	for _, h := range hits {
		byID[h.ID] = h
	}

	var out []memEvent
	add := func(ev memEvent) {
		if ev.Path != "" && !s.canRead(r, ev.Path) {
			return
		}
		ev.Key = eventKey(ev.At, ev.Event, ev.ID)
		out = append(out, ev)
	}

	for _, c := range beliefChanges(hits, since, now) {
		switch c.Kind {
		case changeLearned:
			add(memEvent{At: c.At, Event: evAdded, ID: c.ID, Path: c.Path, Agent: c.Agent,
				Session: byID[c.ID].Session, Text: c.Text})
		case changeChanged:
			// Keyed on the fact that now stands, as in the digest: one correction
			// is one event, and it carries both texts.
			add(memEvent{At: c.At, Event: evSuperseded, ID: c.ID, Path: c.Path, Agent: c.Agent,
				Session: byID[c.ID].Session, Text: c.Text,
				ReplacesID: c.ReplacedID, ReplacedText: c.ReplacedText})
		case changeRetracted:
			add(memEvent{At: c.At, Event: evForgotten, ID: c.ID, Path: c.Path, Agent: c.Agent,
				Session: byID[c.ID].Session, Text: c.Text, Reason: "retracted"})
		case changeExpired:
			add(memEvent{At: c.At, Event: evForgotten, ID: c.ID, Path: c.Path, Agent: c.Agent,
				Session: byID[c.ID].Session, Text: c.Text, Reason: "expired"})
		}
	}

	// Disagreements. A challenge is the agent's claim (the challenger) being
	// refused supersession of a fact (the contested one). Both sides get an
	// event: the claim is challenged, the fact is disputed. Either side is
	// reported only while it is still live, since a settled or superseded
	// dispute is history, not news.
	for _, h := range hits {
		if h.Challenges == "" || h.Superseded() || h.ExpiredAt(now) {
			continue
		}
		at, ok := stampTime(h.Stamp)
		if !ok || at.Before(since) {
			continue
		}
		ch := memEvent{At: h.Stamp, Event: evChallenged, ID: h.ID, Path: h.Note,
			Agent: h.Agent, Session: h.Session, Text: h.Text}
		contested, known := byID[h.Challenges]
		if known {
			ch.ContestedID = contested.ID
		}
		add(ch)
		if known && !contested.Superseded() && !contested.ExpiredAt(now) {
			add(memEvent{At: h.Stamp, Event: evDisputed, ID: contested.ID, Path: contested.Note,
				Agent: contested.Agent, Session: contested.Session, Text: contested.Text,
				ChallengerID: h.ID})
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// streamPos is how far a connection has been sent: the stamp it is inside, and
// the keys already written with that stamp. A same-minute event that arrives
// after a later one is still delivered, because the stream tracks what it sent
// rather than comparing keys.
type streamPos struct {
	minute string
	sent   map[string]bool
}

// advance returns the events in evs that pos has not sent yet, and the position
// after them. evs must be sorted by key.
func advance(evs []memEvent, pos streamPos) ([]memEvent, streamPos) {
	var out []memEvent
	for _, ev := range evs {
		if ev.At < pos.minute || (ev.At == pos.minute && pos.sent[ev.Key]) {
			continue
		}
		out = append(out, ev)
	}
	newest := pos.minute
	for _, ev := range evs {
		if ev.At > newest {
			newest = ev.At
		}
	}
	next := streamPos{minute: newest, sent: map[string]bool{}}
	if newest == pos.minute {
		for k := range pos.sent {
			next.sent[k] = true
		}
	}
	for _, ev := range out {
		if ev.At == newest {
			next.sent[ev.Key] = true
		}
	}
	return out, next
}

func sinceOf(stamp string) time.Time {
	if t, ok := stampTime(stamp); ok {
		return t
	}
	return time.Time{}
}

func (s *Server) memoryStream(w http.ResponseWriter, r *http.Request) {
	// Position is settled before the first byte is written, so an event that
	// happens right after the client sees the headers is delivered, not lost to
	// a baseline taken a moment later.
	now := vault.Now()
	var pos streamPos
	var replay []memEvent
	if at, ok := parseEventID(r.Header.Get("Last-Event-ID")); ok {
		// Resume from the minute the id names. Every event in that minute is
		// replayed, so a client may see some twice; it should dedupe by id. What
		// this never does is skip one, which is the failure that matters.
		evs, err := s.streamEvents(r, sinceOf(at), now)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		replay, pos = advance(evs, streamPos{minute: at, sent: map[string]bool{}})
	} else {
		// Fresh connection: start from now. What already happened is not replayed,
		// so the baseline is the current state, consumed without being sent.
		stamp := now.In(time.Local).Format(memory.StampFormat)
		evs, err := s.streamEvents(r, sinceOf(stamp), now)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		_, pos = advance(evs, streamPos{minute: stamp, sent: map[string]bool{}})
	}

	rc := http.NewResponseController(w)
	// The server's WriteTimeout is five minutes, right for every other route and
	// wrong for a stream that is open for hours. Clearing it is per-response.
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	for _, ev := range replay {
		if writeFrame(w, ev) != nil {
			return
		}
	}
	if rc.Flush() != nil {
		return
	}

	poll := time.NewTicker(streamPoll)
	defer poll.Stop()
	beat := time.NewTicker(streamHeartbeat)
	defer beat.Stop()
	stop := s.stopChan()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-stop:
			return
		case <-beat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		case <-poll.C:
			evs, err := s.streamEvents(r, sinceOf(pos.minute), vault.Now())
			if err != nil {
				// A failed poll is not a dead stream. Say so and try again.
				if _, werr := fmt.Fprintf(w, ": poll failed: %s\n\n", strings.Join(strings.Fields(err.Error()), " ")); werr != nil {
					return
				}
				if rc.Flush() != nil {
					return
				}
				continue
			}
			var out []memEvent
			out, pos = advance(evs, pos)
			for _, ev := range out {
				if writeFrame(w, ev) != nil {
					return
				}
			}
			if len(out) > 0 && rc.Flush() != nil {
				return
			}
		}
	}
}

// parseEventID returns the minute a Last-Event-ID names. A malformed id is
// treated as no id, so a stale or hand-edited one starts a fresh stream rather
// than failing the connection.
func parseEventID(id string) (string, bool) {
	parts := strings.SplitN(id, "|", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return "", false
	}
	if _, ok := stampTime(parts[0]); !ok {
		return "", false
	}
	return parts[0], true
}

func writeFrame(w io.Writer, ev memEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.Key, ev.Event, b)
	return err
}

// stopChan closes when the server is shutting down, so open streams end instead
// of holding http.Server.Shutdown until its deadline.
func (s *Server) stopChan() <-chan struct{} {
	s.stopOnce.Do(func() { s.stop = make(chan struct{}) })
	return s.stop
}

// Close ends every open memory stream. The command calls it before shutting
// the HTTP server down. It is safe to call more than once.
func (s *Server) Close() {
	s.stopChan()
	s.closeOnce.Do(func() { close(s.stop) })
}

// streamState is embedded in Server: the two fields Close needs.
type streamState struct {
	stopOnce  sync.Once
	closeOnce sync.Once
	stop      chan struct{}
}
