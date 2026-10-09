package connectors

import (
	"fmt"
	"sort"
	"strings"
)

// Shared by every mail source (Gmail, IMAP, Outlook).
//
// A mail thread is the unit of knowledge, for the same reason a Slack thread
// is: the answer is in message three, and message three without message one
// has no referent. So each source groups messages into threads and hands them
// to ThreadDocument, which renders ONE markdown document per thread.
//
// Attachments are listed, never ingested. An attachment is arbitrary binary
// from a stranger; indexing it would turn a mailbox into a way to push a PDF
// of instructions into recall. The listing tells a reader that something was
// attached and what it was called.

// MailMessage is one message, normalised across providers.
type MailMessage struct {
	ID          string
	From        string // display form, "Name <addr>"
	FromAddr    string // bare lowercase address, for ownership checks
	To          []string
	Cc          []string
	Date        string // RFC3339 when known
	Subject     string
	Body        string // plain text (HTML already reduced)
	Attachments []string
	Sent        bool // true when the account owner sent it
}

// maxMailBody bounds one message's text. Quoted reply chains repeat the whole
// thread in every message; capping keeps a long thread from being N^2.
const maxMailBody = 20000

// ThreadDocument renders messages (oldest first) as one document.
// The caller sets ExternalID, URL and any source-specific Meta afterwards.
func ThreadDocument(msgs []MailMessage) Document {
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].Date < msgs[j].Date })
	doc := Document{Meta: map[string]string{}}
	if len(msgs) == 0 {
		return doc
	}
	doc.Title = strings.TrimSpace(msgs[0].Subject)
	if doc.Title == "" {
		doc.Title = "(no subject)"
	}
	people := map[string]bool{}
	var order []string
	attachments := 0
	allSent := true
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "### %s — %s\n\n", firstNonEmpty(m.From, "unknown sender"), m.Date)
		if len(m.To) > 0 {
			fmt.Fprintf(&b, "To: %s\n", strings.Join(m.To, ", "))
		}
		if len(m.Cc) > 0 {
			fmt.Fprintf(&b, "Cc: %s\n", strings.Join(m.Cc, ", "))
		}
		if len(m.To)+len(m.Cc) > 0 {
			b.WriteString("\n")
		}
		b.WriteString(clipMail(strings.TrimSpace(m.Body)))
		b.WriteString("\n\n")
		for _, a := range m.Attachments {
			fmt.Fprintf(&b, "- attachment (not ingested): %s\n", a)
			attachments++
		}
		if len(m.Attachments) > 0 {
			b.WriteString("\n")
		}
		for _, p := range append([]string{m.From}, append(append([]string{}, m.To...), m.Cc...)...) {
			if p = strings.TrimSpace(p); p != "" && !people[p] {
				people[p] = true
				order = append(order, p)
			}
		}
		if !m.Sent {
			allSent = false
		}
		doc.Updated = m.Date
	}
	doc.Body = strings.TrimSpace(b.String())
	doc.Author = msgs[0].From
	doc.Meta["participants"] = strings.Join(order, "; ")
	doc.Meta["message_count"] = fmt.Sprint(len(msgs))
	if attachments > 0 {
		doc.Meta["attachments"] = fmt.Sprint(attachments)
	}
	// Own only when EVERY message was sent by the owner: a thread holding one
	// reply from a stranger is a stranger's text.
	doc.Own = allSent
	return doc
}

func clipMail(s string) string {
	if len(s) <= maxMailBody {
		return s
	}
	return s[:maxMailBody] + "\n\n[… truncated]"
}

// NormalizeSubject strips Re:/Fwd: prefixes, for subject-based threading.
func NormalizeSubject(s string) string {
	s = strings.TrimSpace(s)
	for {
		l := strings.ToLower(s)
		switch {
		case strings.HasPrefix(l, "re:"), strings.HasPrefix(l, "fw:"):
			s = strings.TrimSpace(s[3:])
		case strings.HasPrefix(l, "fwd:"):
			s = strings.TrimSpace(s[4:])
		default:
			return s
		}
	}
}

// mailText reduces a body to plain text: HTML is converted, plain passes through.
func mailText(contentType, body string) string {
	if strings.Contains(strings.ToLower(contentType), "html") {
		return strings.TrimSpace(HTMLToMarkdown(body))
	}
	return strings.TrimSpace(body)
}

// sinceCutoff turns the "since" setting (YYYY-MM-DD) or a default number of
// days back into a time. Used by every mail/calendar source for a first sync.
func sinceCutoff(cfg Config, defaultDays int) (string, error) {
	if v := cfg.Get("since"); v != "" {
		t, err := parseDay(v)
		if err != nil {
			return "", fmt.Errorf("%w: since must be YYYY-MM-DD", ErrConfig)
		}
		return t, nil
	}
	return nowMinusDays(defaultDays), nil
}

func parseDay(v string) (string, error) {
	t, err := timeParseDay(v)
	if err != nil {
		return "", err
	}
	return rfc3339(t), nil
}

func nowMinusDays(n int) string { return rfc3339(timeNow().AddDate(0, 0, -n)) }
