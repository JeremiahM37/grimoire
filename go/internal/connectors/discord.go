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

// Discord: channel messages as documents.
//
// The same argument as Slack, for the communities that never used Slack.
// Decisions, incident post-mortems and the answer to "why is it like this" are
// in a channel, and nobody is going to retype them into a wiki.
//
// Messages are grouped into one document per day per channel rather than one
// per message. A single chat line is not a document: it has no title, it
// retrieves badly, and a thousand of them bury a vault. A day of a channel is
// the smallest unit that still reads like something.

func init() { Register(discord{}) }

type discord struct{}

func (discord) Kind() string { return "discord" }

func (discord) Describe() Kind {
	return Kind{
		Kind: "discord",
		Name: "Discord",
		Help: "Pulls messages from one channel, grouped into a document per day. " +
			"Enable Developer Mode in Discord (Settings → Advanced), then right-click " +
			"the channel → Copy Channel ID.",
		SecretHelp: "A bot token from the Discord Developer Portal. The bot needs the " +
			"Read Message History permission and must be a member of the server. " +
			"Prefix with 'Bot ' is added automatically.",
		Fields: []Field{
			{Name: "channel", Label: "Channel ID", Required: true, Placeholder: "123456789012345678"},
			{Name: "channel_name", Label: "Channel name", Placeholder: "engineering",
				Help: "Used in titles and frontmatter. The API does not always expose it."},
		},
		DefaultPrefix: "connectors/discord",
	}
}

// discordMessage is the subset of the message object that matters here.
type discordMessage struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Author    struct {
		Username string `json:"username"`
		Bot      bool   `json:"bot"`
	} `json:"author"`
}

func (d discord) Fetch(ctx context.Context, in Input) (Page, error) {
	channel := in.Config.Get("channel")
	if channel == "" {
		return Page{}, fmt.Errorf("%w: channel id is required", ErrConfig)
	}
	// Discord ids are snowflakes: an integer whose high bits are a timestamp.
	// That is what makes `after=<id>` a cursor — it means "newer than this
	// message" without the API needing to understand dates.
	if _, err := strconv.ParseUint(channel, 10, 64); err != nil {
		return Page{}, fmt.Errorf("%w: channel id must be numeric — enable Developer "+
			"Mode and use Copy Channel ID, not the channel name", ErrConfig)
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 100 // the API's own maximum for this endpoint
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if in.Cursor != "" {
		q.Set("after", in.Cursor)
	}
	req, err := jsonRequest("https://discord.com/api/v10/channels/"+channel+"/messages", q,
		map[string]string{"Authorization": "Bot " + in.Secret})
	if err != nil {
		return Page{}, err
	}
	var msgs []discordMessage
	if err := getJSON(ctx, in.Client, req, &msgs); err != nil {
		return Page{}, err
	}
	if len(msgs) == 0 {
		return Page{Cursor: in.Cursor}, nil
	}

	// The endpoint returns newest-first. Sorting ascending makes the last id
	// the cursor and keeps a day's transcript in reading order.
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })

	name := in.Config.Get("channel_name")
	if name == "" {
		name = channel
	}
	byDay := map[string][]discordMessage{}
	var days []string
	for _, m := range msgs {
		day := m.Timestamp
		if len(day) >= 10 {
			day = day[:10]
		}
		if _, seen := byDay[day]; !seen {
			days = append(days, day)
		}
		byDay[day] = append(byDay[day], m)
	}
	sort.Strings(days)

	docs := make([]Document, 0, len(days))
	for _, day := range days {
		var b strings.Builder
		var updated string
		for _, m := range byDay[day] {
			if strings.TrimSpace(m.Content) == "" {
				continue // an embed, attachment or reaction with no text
			}
			hhmm := m.Timestamp
			if len(hhmm) >= 16 {
				hhmm = hhmm[11:16]
			}
			author := m.Author.Username
			if m.Author.Bot {
				author += " (bot)"
			}
			fmt.Fprintf(&b, "**%s · %s** — %s\n\n", hhmm, author, strings.TrimSpace(m.Content))
			updated = m.Timestamp
		}
		body := strings.TrimSpace(b.String())
		if body == "" {
			continue
		}
		docs = append(docs, Document{
			// Keyed on channel+day so re-syncing a day that gained messages
			// updates the document instead of adding a second copy of it.
			ExternalID: channel + ":" + day,
			Title:      fmt.Sprintf("#%s — %s", name, day),
			Body:       body,
			URL:        "https://discord.com/channels/@me/" + channel,
			Updated:    updated,
			Meta:       map[string]string{"channel": name, "channel_id": channel, "date": day},
		})
	}
	cursor := msgs[len(msgs)-1].ID
	return Page{Docs: docs, Cursor: cursor, More: len(msgs) == limit}, nil
}

// discordTime parses the API's ISO-8601 timestamps. Kept separate so a change
// in their format is one edit.
func discordTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}
