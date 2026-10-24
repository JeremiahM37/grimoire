package api

import (
	"fmt"

	"net/http"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
)

// The owner's inbox: everything Grimoire is waiting on a person for, in one
// list.
//
// Each of these used to live on its own surface that nobody looked at: the
// dream's questions in a report note, connector failures in the journal,
// approvals behind an admin-only route the console polled and failed 1,474
// times while its badge said zero. The inbox gathers them where the owner and
// their agents already look — the top of get_briefing, the console badge —
// and it is meant to be short. The dream fixes what it can and settles what
// follows from the store, so what reaches here is what genuinely needs a
// person.
//
// It holds counts and descriptions, never a credential or a note's text, so
// it is readable without the admin credential; acting on an item still goes
// through the gated route that owns it.

// InboxItem is one thing waiting for the owner.
type InboxItem struct {
	Source string    `json:"source"` // dream, connector, approvals, credentials
	Title  string    `json:"title"`
	Detail string    `json:"detail,omitempty"`
	Action string    `json:"action"`
	Path   string    `json:"path,omitempty"`
	Since  time.Time `json:"since,omitempty"`
}

// ownerView reports whether the caller is the owner the inbox is for: anyone
// on an instance without accounts, an administrator on one with them. Other
// account holders get an empty inbox; it is about the owner's memory and
// connectors, not theirs.
func (s *Server) ownerView(r *http.Request) bool {
	if s.Auth == nil || !s.Auth.Enabled() {
		return true
	}
	return principal(r).IsAdmin()
}

// Inbox lists what needs the owner, most actionable first.
func (s *Server) Inbox() []InboxItem {
	var out []InboxItem
	if n := s.pendingGrantRequests(); n > 0 {
		out = append(out, InboxItem{Source: "credentials",
			Title:  fmt.Sprintf("%d credential request%s waiting", n, plural(n)),
			Action: "Approve or deny in the console's Grants panel, or `grimoire grants requests`."})
	}
	if n := s.pendingSourceActions(); n > 0 {
		out = append(out, InboxItem{Source: "approvals",
			Title:  fmt.Sprintf("%d agent action%s waiting for approval", n, plural(n)),
			Action: "Approve or deny in the console's Approvals panel (opened with the admin credential)."})
	}
	for _, p := range s.ConnectorProblems() {
		out = append(out, InboxItem{Source: "connector", Title: p.Title, Detail: p.Detail,
			Action: p.Action, Since: p.Since})
	}
	for _, a := range s.dreamAsks() {
		out = append(out, InboxItem{Source: "dream", Title: a.Message, Action: a.Action, Path: a.Path})
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (s *Server) pendingGrantRequests() int {
	if s.Broker == nil {
		return 0
	}
	n, err := s.Broker.PendingCount()
	if err != nil {
		return 0
	}
	return n
}

func (s *Server) pendingSourceActions() int {
	svc := s.Sources()
	if svc == nil {
		return 0
	}
	list, err := svc.Actions("pending", 200)
	if err != nil {
		return 0
	}
	return len(list)
}

// dreamAsks are the last dream's questions. They survive a restart through
// the dream state, so the inbox does not empty itself when the service
// restarts and the next dream is a day away.
func (s *Server) dreamAsks() []dream.Ask {
	return s.loadDreamState().Asks
}

// inboxLine is the inbox as one paragraph for the top of a briefing: an agent
// reads it first, and can tell the person in its own words.
func inboxLine(items []InboxItem) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Needs the owner (%d): ", len(items))
	for i, it := range items {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(strings.TrimSuffix(it.Title, "."))
		if it.Action != "" {
			b.WriteString(" — " + it.Action)
		}
	}
	return b.String()
}

func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	items := []InboxItem{}
	approvals, credentials := 0, 0
	if s.ownerView(r) {
		if all := s.Inbox(); all != nil {
			items = all
		}
		approvals, credentials = s.pendingSourceActions(), s.pendingGrantRequests()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"counts": map[string]int{
			"approvals":   approvals,
			"credentials": credentials,
			"total":       len(items),
		},
	})
}
