package connectors

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Slack: one note per thread, not per message.
//
// A message is rarely a unit of knowledge — the answer is three replies down,
// and retrieving the reply without the question gives a reader a sentence with
// no referent. So a thread becomes a document, its parent message the title,
// and the replies the body in order. Channel messages with no replies are
// grouped into a daily note per channel, which is the smallest unit that still
// reads as a conversation.

func init() { Register(slack{}) }

type slack struct{}

func (slack) Kind() string { return "slack" }

func (slack) Describe() Kind {
	return Kind{
		Kind: "slack",
		Name: "Slack",
		Help: "Pulls conversations from the channels you list. The bot must be a " +
			"member of each one — Slack returns not_in_channel otherwise, which " +
			"is the most common reason a first sync comes back empty.",
		SecretHelp: "A bot token (xoxb-…) with channels:history, groups:history " +
			"and users:read. Create it at api.slack.com/apps → OAuth & Permissions.",
		Fields: []Field{
			{Name: "channels", Label: "Channel IDs", Required: true,
				Placeholder: "C01234567, C89ABCDEF",
				Help: "Comma-separated. Right-click a channel → View channel details; " +
					"the ID is at the bottom."},
			{Name: "include_threads", Label: "Include thread replies",
				Placeholder: "yes", Help: "yes (default) or no"},
			{Name: "route_by", Label: "Split by", Placeholder: "channel",
				Help: "Put each channel in its own folder, so a private channel can " +
					"sit in a Grimoire space with matching membership."},
			{Name: "route_map", Label: "Folder per value",
				Placeholder: "C0123=team/eng, C0456=hr"},
		},
		DefaultPrefix: "connectors/slack",
	}
}

func (s slack) Fetch(ctx context.Context, in Input) (Page, error) {
	if in.Secret == "" {
		return Page{}, missing("a bot token")
	}
	channels := splitList(in.Config.Get("channels"))
	if len(channels) == 0 {
		return Page{}, missing("at least one channel ID")
	}
	oldest := in.Cursor
	if oldest == "" {
		// A first sync takes the last 30 days rather than all history: the
		// point is to become useful immediately, and a channel with five years
		// of messages would otherwise spend the first sync on 2019.
		oldest = strconv.FormatInt(time.Now().AddDate(0, 0, -30).Unix(), 10) + ".000000"
	}

	var docs []Document
	newest := oldest
	for _, ch := range channels {
		msgs, err := s.history(ctx, in, ch, oldest)
		if err != nil {
			return Page{}, fmt.Errorf("channel %s: %w", ch, err)
		}
		for _, m := range msgs {
			if m.TS > newest {
				newest = m.TS
			}
		}
		docs = append(docs, s.assemble(ctx, in, ch, msgs)...)
		if len(docs) >= in.Limit && in.Limit > 0 {
			break
		}
	}
	return Page{Docs: docs, Cursor: newest}, nil
}

type slackMessage struct {
	TS       string `json:"ts"`
	Text     string `json:"text"`
	User     string `json:"user"`
	ThreadTS string `json:"thread_ts"`
	Subtype  string `json:"subtype"`
	ReplyCnt int    `json:"reply_count"`
}

type slackHistory struct {
	OK       bool           `json:"ok"`
	Error    string         `json:"error"`
	Messages []slackMessage `json:"messages"`
	HasMore  bool           `json:"has_more"`
}

func (s slack) history(ctx context.Context, in Input, channel, oldest string) ([]slackMessage, error) {
	q := url.Values{"channel": {channel}, "oldest": {oldest}, "limit": {"200"}}
	req, err := jsonRequest("https://slack.com/api/conversations.history", q,
		map[string]string{"Authorization": "Bearer " + in.Secret})
	if err != nil {
		return nil, err
	}
	var out slackHistory
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		// Slack answers 200 with ok:false, so a status check alone would read
		// every failure as success and sync nothing, forever, silently.
		return nil, slackError(out.Error)
	}
	return out.Messages, nil
}

func (s slack) replies(ctx context.Context, in Input, channel, ts string) ([]slackMessage, error) {
	q := url.Values{"channel": {channel}, "ts": {ts}, "limit": {"200"}}
	req, err := jsonRequest("https://slack.com/api/conversations.replies", q,
		map[string]string{"Authorization": "Bearer " + in.Secret})
	if err != nil {
		return nil, err
	}
	var out slackHistory
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, slackError(out.Error)
	}
	return out.Messages, nil
}

// assemble turns raw messages into documents: one per thread, plus a daily
// note per channel for everything that was never replied to.
func (s slack) assemble(ctx context.Context, in Input, channel string, msgs []slackMessage) []Document {
	threads := map[string][]slackMessage{}
	var loose []slackMessage
	for _, m := range msgs {
		if m.Subtype == "channel_join" || m.Subtype == "channel_leave" || strings.TrimSpace(m.Text) == "" {
			continue
		}
		switch {
		case m.ReplyCnt > 0:
			threads[m.TS] = nil // fetched below
		case m.ThreadTS != "" && m.ThreadTS != m.TS:
			threads[m.ThreadTS] = nil
		default:
			loose = append(loose, m)
		}
	}

	var docs []Document
	roots := make([]string, 0, len(threads))
	for ts := range threads {
		roots = append(roots, ts)
	}
	sort.Strings(roots)
	includeThreads := !strings.EqualFold(in.Config.Get("include_threads"), "no")
	if includeThreads {
		for _, root := range roots {
			replies, err := s.replies(ctx, in, channel, root)
			if err != nil || len(replies) == 0 {
				continue
			}
			var b strings.Builder
			for _, m := range replies {
				fmt.Fprintf(&b, "**%s** · %s\n\n%s\n\n", displayUser(m.User), slackTime(m.TS), m.Text)
			}
			title := firstLine(replies[0].Text)
			docs = append(docs, Document{
				ExternalID: channel + "/" + root,
				Title:      title,
				Body:       b.String(),
				Updated:    slackTime(replies[len(replies)-1].TS),
				Author:     displayUser(replies[0].User),
				Meta:       map[string]string{"channel": channel, "thread": root},
			})
		}
	}

	byDay := map[string][]slackMessage{}
	for _, m := range loose {
		byDay[slackDay(m.TS)] = append(byDay[slackDay(m.TS)], m)
	}
	days := make([]string, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Strings(days)
	for _, day := range days {
		group := byDay[day]
		sort.Slice(group, func(i, j int) bool { return group[i].TS < group[j].TS })
		var b strings.Builder
		for _, m := range group {
			fmt.Fprintf(&b, "**%s** · %s\n\n%s\n\n", displayUser(m.User), slackTime(m.TS), m.Text)
		}
		docs = append(docs, Document{
			ExternalID: channel + "/day/" + day,
			Title:      "#" + channel + " " + day,
			Body:       b.String(),
			Updated:    slackTime(group[len(group)-1].TS),
			Meta:       map[string]string{"channel": channel, "day": day},
		})
	}
	return docs
}

func slackError(code string) error {
	hint := map[string]string{
		"not_in_channel":    "the bot is not a member of that channel — invite it",
		"channel_not_found": "no such channel ID, or the token cannot see it",
		"invalid_auth":      "the bot token was rejected",
		"missing_scope":     "the token lacks channels:history or groups:history",
		"ratelimited":       "rate limited; the next sync resumes from the cursor",
		"token_expired":     "the bot token has expired",
		"account_inactive":  "the token's account is deactivated",
		"not_authed":        "no token was sent",
	}[code]
	if hint == "" {
		return fmt.Errorf("slack: %s", code)
	}
	return fmt.Errorf("slack: %s (%s)", code, hint)
}

// slackTime renders a Slack timestamp ("1712345678.000200") as RFC3339.
func slackTime(ts string) string {
	sec, _, _ := strings.Cut(ts, ".")
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return ""
	}
	return rfc3339(time.Unix(n, 0))
}

func slackDay(ts string) string {
	if t := slackTime(ts); len(t) >= 10 {
		return t[:10]
	}
	return "unknown"
}

// displayUser keeps the raw user id when nothing better is known. Resolving
// ids to names needs users:read and a second call per user; the id is at least
// stable and unambiguous.
func displayUser(id string) string {
	if id == "" {
		return "unknown"
	}
	return id
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '\n' || r == ' '
	}) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstLine(s string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(s), "\n", 2)[0])
	if len([]rune(line)) > 90 {
		line = string([]rune(line)[:90]) + "…"
	}
	if line == "" {
		return "(untitled thread)"
	}
	return line
}
