package connectors

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Google Calendar: one note per event.
//
// Calendar is where "what is Dana doing on the 14th" and "when did we agree to
// the review" live, and it changes constantly, so the cursor is the event's
// own `updated` stamp and a re-sync rewrites only what moved.

func init() { Register(gcal{}) }

type gcal struct{}

const gcalBase = "https://www.googleapis.com/calendar/v3"

func (gcal) Kind() string { return "gcal" }

func (gcal) Describe() Kind {
	return Kind{
		Kind: "gcal",
		Name: "Google Calendar",
		Help: "Pulls events (past and upcoming) from one calendar. Events other people " +
			"invited you to carry their text; only events you organise count as yours " +
			"under trust=own.",
		SecretHelp: "An OAuth token stored by `grimoire connect google` (calendar.readonly; " +
			"calendar.events only if you enable create_event).",
		Fields: []Field{
			{Name: "calendar_id", Label: "Calendar", Placeholder: "primary"},
			{Name: "days_back", Label: "Days back", Placeholder: "30"},
			{Name: "days_ahead", Label: "Days ahead", Placeholder: "90"},
		},
		DefaultPrefix: "connectors/calendar",
	}
}

type gcalEvent struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Location    string `json:"location"`
	HTMLLink    string `json:"htmlLink"`
	Updated     string `json:"updated"`
	HangoutLink string `json:"hangoutLink"`
	Start       struct {
		DateTime string `json:"dateTime"`
		Date     string `json:"date"`
	} `json:"start"`
	End struct {
		DateTime string `json:"dateTime"`
		Date     string `json:"date"`
	} `json:"end"`
	Organizer struct {
		Email, DisplayName string
		Self               bool
	} `json:"organizer"`
	Attendees []struct {
		Email          string `json:"email"`
		DisplayName    string `json:"displayName"`
		ResponseStatus string `json:"responseStatus"`
	} `json:"attendees"`
}

func (e gcalEvent) when() (string, string) {
	return firstNonEmpty(e.Start.DateTime, e.Start.Date), firstNonEmpty(e.End.DateTime, e.End.Date)
}

func (e gcalEvent) body() string {
	start, end := e.when()
	var b strings.Builder
	fmt.Fprintf(&b, "When: %s → %s\n", start, end)
	if e.Location != "" {
		fmt.Fprintf(&b, "Where: %s\n", e.Location)
	}
	if e.HangoutLink != "" {
		fmt.Fprintf(&b, "Meeting link: %s\n", e.HangoutLink)
	}
	if e.Organizer.Email != "" {
		fmt.Fprintf(&b, "Organizer: %s\n", firstNonEmpty(e.Organizer.DisplayName, e.Organizer.Email))
	}
	if len(e.Attendees) > 0 {
		b.WriteString("\nAttendees:\n")
		for _, a := range e.Attendees {
			fmt.Fprintf(&b, "- %s (%s)\n", firstNonEmpty(a.DisplayName, a.Email), a.ResponseStatus)
		}
	}
	if d := strings.TrimSpace(HTMLToMarkdown(e.Description)); d != "" {
		b.WriteString("\n" + d + "\n")
	}
	return strings.TrimSpace(b.String())
}

func (g gcal) calendar(in Input) string {
	return firstNonEmpty(in.Config.Get("calendar_id"), "primary")
}

func (g gcal) Fetch(ctx context.Context, in Input) (Page, error) {
	if in.Secret == "" {
		return Page{}, missing("an OAuth token (run `grimoire connect google`)")
	}
	back, ahead := atoiOr(in.Config.Get("days_back"), 30), atoiOr(in.Config.Get("days_ahead"), 90)
	now := timeNow()
	limit := in.Limit
	if limit <= 0 || limit > 250 {
		limit = 50
	}
	q := url.Values{
		"singleEvents": {"true"},
		"orderBy":      {"updated"},
		"maxResults":   {strconv.Itoa(limit)},
		"timeMin":      {rfc3339(now.AddDate(0, 0, -back))},
		"timeMax":      {rfc3339(now.AddDate(0, 0, ahead))},
	}
	if in.Cursor != "" {
		q.Set("updatedMin", in.Cursor)
	}
	req, err := jsonRequest(gcalBase+"/calendars/"+url.PathEscape(g.calendar(in))+"/events", q,
		map[string]string{"Authorization": "Bearer " + in.Secret})
	if err != nil {
		return Page{}, err
	}
	var out struct {
		Items         []gcalEvent `json:"items"`
		NextPageToken string      `json:"nextPageToken"`
	}
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return Page{}, err
	}
	page := Page{Cursor: in.Cursor, More: len(out.Items) >= limit}
	for _, e := range out.Items {
		if e.Updated > page.Cursor {
			page.Cursor = e.Updated
		}
		if e.Status == "cancelled" {
			continue
		}
		start, _ := e.when()
		title := firstNonEmpty(e.Summary, "(no title)")
		page.Docs = append(page.Docs, Document{
			ExternalID: e.ID, Title: title, Body: e.body(), URL: e.HTMLLink, Updated: e.Updated,
			Author: firstNonEmpty(e.Organizer.DisplayName, e.Organizer.Email),
			Meta:   map[string]string{"source": "google-calendar", "start": start},
			Own:    e.Organizer.Self,
		})
	}
	return page, nil
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 0 {
		return n
	}
	return def
}

func (g gcal) Search(ctx context.Context, in Input, query string, limit int) ([]Hit, error) {
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	q := url.Values{"q": {query}, "singleEvents": {"true"}, "orderBy": {"startTime"},
		"maxResults": {strconv.Itoa(limit)}, "timeMin": {rfc3339(timeNow().AddDate(-1, 0, 0))}}
	req, err := jsonRequest(gcalBase+"/calendars/"+url.PathEscape(g.calendar(in))+"/events", q,
		map[string]string{"Authorization": "Bearer " + in.Secret})
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []gcalEvent `json:"items"`
	}
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return nil, err
	}
	var hits []Hit
	for _, e := range out.Items {
		start, _ := e.when()
		hits = append(hits, Hit{ID: e.ID, Title: e.Summary, Snippet: start + " " + e.Location,
			URL: e.HTMLLink, Updated: e.Updated, Author: e.Organizer.Email})
	}
	return hits, nil
}

func (g gcal) Read(ctx context.Context, in Input, id string) (Item, error) {
	req, err := jsonRequest(gcalBase+"/calendars/"+url.PathEscape(g.calendar(in))+"/events/"+url.PathEscape(id),
		nil, map[string]string{"Authorization": "Bearer " + in.Secret})
	if err != nil {
		return Item{}, err
	}
	var e gcalEvent
	if err := getJSON(ctx, in.Client, req, &e); err != nil {
		return Item{}, err
	}
	return Item{ID: e.ID, Title: e.Summary, Body: e.body(), URL: e.HTMLLink, Updated: e.Updated,
		Author: firstNonEmpty(e.Organizer.DisplayName, e.Organizer.Email)}, nil
}

func (gcal) Actions() []ActionSpec {
	return []ActionSpec{{
		Name:    "create_event",
		Summary: "Create a calendar event. Attendees are NOT emailed an invitation unless notify=yes.",
		Params: []Field{
			{Name: "summary", Label: "Title", Required: true},
			{Name: "start", Label: "Start (RFC3339, or YYYY-MM-DD for all-day)", Required: true},
			{Name: "end", Label: "End (same format)", Required: true},
			{Name: "attendees", Label: "Attendee emails, comma-separated"},
			{Name: "description", Label: "Description"},
			{Name: "location", Label: "Location"},
			{Name: "notify", Label: "yes to email invitations"},
		},
		Scopes: []string{"https://www.googleapis.com/auth/calendar.events"},
	}}
}

func (g gcal) Act(ctx context.Context, in Input, action string, p map[string]string) (ActionResult, error) {
	if action != "create_event" {
		return ActionResult{}, fmt.Errorf("gcal has no action %q", action)
	}
	when := func(v string) (map[string]string, error) {
		if _, err := time.Parse("2006-01-02", v); err == nil {
			return map[string]string{"date": v}, nil
		}
		if _, err := time.Parse(time.RFC3339, v); err != nil {
			return nil, fmt.Errorf("%q is not RFC3339 or YYYY-MM-DD", v)
		}
		return map[string]string{"dateTime": v}, nil
	}
	start, err := when(p["start"])
	if err != nil {
		return ActionResult{}, err
	}
	end, err := when(p["end"])
	if err != nil {
		return ActionResult{}, err
	}
	ev := map[string]any{"summary": p["summary"], "start": start, "end": end}
	if p["description"] != "" {
		ev["description"] = p["description"]
	}
	if p["location"] != "" {
		ev["location"] = p["location"]
	}
	var att []map[string]string
	for _, a := range splitList(p["attendees"]) {
		att = append(att, map[string]string{"email": a})
	}
	if len(att) > 0 {
		ev["attendees"] = att
	}
	send := "none"
	if strings.EqualFold(p["notify"], "yes") {
		send = "all"
	}
	var out struct {
		ID       string `json:"id"`
		HTMLLink string `json:"htmlLink"`
	}
	err = postJSON(ctx, in, gcalBase+"/calendars/"+url.PathEscape(g.calendar(in))+"/events?sendUpdates="+send, ev, &out)
	if err != nil {
		return ActionResult{}, err
	}
	return ActionResult{ID: out.ID, URL: out.HTMLLink, Message: "event created"}, nil
}
