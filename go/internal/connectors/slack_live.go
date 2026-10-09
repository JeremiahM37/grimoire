package connectors

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Live search, read and post for Slack. The sync side is slack.go.

func (s slack) Search(ctx context.Context, in Input, query string, limit int) ([]Hit, error) {
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	req, err := jsonRequest("https://slack.com/api/search.messages",
		url.Values{"query": {query}, "count": {strconv.Itoa(limit)}, "sort": {"timestamp"}},
		map[string]string{"Authorization": "Bearer " + in.Secret})
	if err != nil {
		return nil, err
	}
	var out struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Messages struct {
			Matches []struct {
				Text      string `json:"text"`
				TS        string `json:"ts"`
				Username  string `json:"username"`
				Permalink string `json:"permalink"`
				Channel   struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"channel"`
			} `json:"matches"`
		} `json:"messages"`
	}
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		if out.Error == "not_allowed_token_type" || out.Error == "missing_scope" {
			return nil, fmt.Errorf("slack search needs a USER token with the search:read scope " +
				"(bot tokens cannot search); reconnect with `grimoire connect slack`")
		}
		return nil, slackError(out.Error)
	}
	var hits []Hit
	for _, m := range out.Messages.Matches {
		hits = append(hits, Hit{
			ID: m.Channel.ID + ":" + m.TS, Title: "#" + m.Channel.Name + " " + firstLine(m.Text),
			Snippet: m.Text, URL: m.Permalink, Updated: slackTime(m.TS), Author: m.Username})
	}
	return hits, nil
}

func (s slack) Read(ctx context.Context, in Input, id string) (Item, error) {
	channel, ts, ok := strings.Cut(id, ":")
	if !ok || channel == "" || ts == "" {
		return Item{}, fmt.Errorf("slack ids look like CHANNEL:TS (from source_search)")
	}
	msgs, err := s.replies(ctx, in, channel, ts)
	if err != nil {
		return Item{}, err
	}
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "**%s** · %s\n\n%s\n\n", displayUser(m.User), slackTime(m.TS), m.Text)
	}
	title := channel
	if len(msgs) > 0 {
		title = firstLine(msgs[0].Text)
	}
	return Item{ID: id, Title: title, Body: strings.TrimSpace(b.String())}, nil
}

func (slack) Actions() []ActionSpec {
	return []ActionSpec{{
		Name:    "post_message",
		Summary: "Post a message to a Slack channel (or thread). Visible to everyone in the channel; cannot be unsent from here.",
		Params: []Field{
			{Name: "channel", Label: "Channel ID", Required: true},
			{Name: "text", Label: "Text", Required: true},
			{Name: "thread_ts", Label: "Thread timestamp, to reply in a thread"},
		},
		Scopes:       []string{"chat:write"},
		Irreversible: true,
	}}
}

func (s slack) Act(ctx context.Context, in Input, action string, p map[string]string) (ActionResult, error) {
	if action != "post_message" {
		return ActionResult{}, fmt.Errorf("slack has no action %q", action)
	}
	// post_channels, when set, is an allowlist: the operator decides which
	// channels an agent may ever speak in, whatever the token could reach.
	if allow := splitList(in.Config.Get("post_channels")); len(allow) > 0 {
		ok := false
		for _, c := range allow {
			if c == p["channel"] {
				ok = true
			}
		}
		if !ok {
			return ActionResult{}, fmt.Errorf("channel %q is not in this connector's post_channels", p["channel"])
		}
	}
	body := map[string]any{"channel": p["channel"], "text": p["text"]}
	if p["thread_ts"] != "" {
		body["thread_ts"] = p["thread_ts"]
	}
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		TS    string `json:"ts"`
	}
	if err := postJSON(ctx, in, "https://slack.com/api/chat.postMessage", body, &out); err != nil {
		return ActionResult{}, err
	}
	if !out.OK {
		return ActionResult{}, slackError(out.Error)
	}
	return ActionResult{ID: p["channel"] + ":" + out.TS, Message: "message posted"}, nil
}
