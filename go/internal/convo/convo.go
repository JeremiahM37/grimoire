// Package convo turns a chat export into notes.
//
// An empty vault is worth nothing, and that is the whole cold start: the pitch
// is "point it at what you already have", and for most people the largest thing
// they already have is not a folder of markdown — it is two years of
// conversations with an assistant, sitting in a zip they downloaded once.
//
// The conversion is deliberately lossy in one direction only. Everything the
// export contains about WHO said WHAT and WHEN is kept, because that is what
// makes a transcript answerable later. Everything about rendering — model
// parameters, message ids, tool-call plumbing — is dropped, because a note full
// of machine bookkeeping is not a note anybody reads.
package convo

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Conversation is one imported chat, reduced to what a note needs.
type Conversation struct {
	Title    string
	Created  time.Time
	Messages []Message
	// Source names the export it came from, so a note's frontmatter can say
	// where it is from without the reader guessing.
	Source string
}

// Message is one turn.
type Message struct {
	Role string // "user" or "assistant"
	Text string
	At   time.Time
}

// ---------------------------------------------------------------- ChatGPT

// chatGPTMessage is one turn inside a mapping node.
type chatGPTMessage struct {
	Author struct {
		Role string `json:"role"`
	} `json:"author"`
	CreateTime float64 `json:"create_time"`
	Content    struct {
		ContentType string `json:"content_type"`
		Parts       []any  `json:"parts"`
	} `json:"content"`
}

// chatGPTNode is one node of the edit tree.
type chatGPTNode struct {
	ID       string          `json:"id"`
	Parent   string          `json:"parent"`
	Children []string        `json:"children"`
	Message  *chatGPTMessage `json:"message"`
}

// chatGPTConversation is one entry in ChatGPT's conversations.json.
//
// Its messages are a MAPPING of nodes with parent pointers, not a list — the
// export preserves the edit tree, so a conversation that was branched contains
// several alternative replies to the same prompt. Reading the map in map order
// produces a scrambled transcript; the tree has to be walked.
type chatGPTConversation struct {
	Title       string                 `json:"title"`
	CreateTime  float64                `json:"create_time"`
	Mapping     map[string]chatGPTNode `json:"mapping"`
	CurrentNode string                 `json:"current_node"`
}

// ParseChatGPT reads a ChatGPT conversations.json export.
func ParseChatGPT(raw []byte) ([]Conversation, error) {
	var items []chatGPTConversation
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("not a ChatGPT conversations.json: %w", err)
	}
	out := make([]Conversation, 0, len(items))
	for _, it := range items {
		c := Conversation{
			Title:   strings.TrimSpace(it.Title),
			Created: unixTime(it.CreateTime),
			Source:  "chatgpt",
		}
		for _, node := range chatGPTThread(it) {
			m := node.Message
			if m == nil {
				continue
			}
			role := m.Author.Role
			if role != "user" && role != "assistant" {
				continue // system prompts and tool plumbing are not the conversation
			}
			text := joinParts(m.Content.Parts)
			if strings.TrimSpace(text) == "" {
				continue
			}
			c.Messages = append(c.Messages, Message{
				Role: role, Text: text, At: unixTime(m.CreateTime),
			})
		}
		if len(c.Messages) == 0 {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// chatGPTThread walks from current_node back to the root and returns the nodes
// in order.
//
// Walking backwards is what picks ONE path through the edit tree — the branch
// that was actually left on screen — rather than every alternative reply the
// user regenerated past.
func chatGPTThread(c chatGPTConversation) []chatGPTNode {
	var chain []chatGPTNode
	seen := map[string]bool{}
	cur := c.CurrentNode
	for cur != "" && !seen[cur] {
		seen[cur] = true
		n, ok := c.Mapping[cur]
		if !ok {
			break
		}
		chain = append(chain, n)
		cur = n.Parent
	}
	// Reverse: the walk produced newest-first.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	if len(chain) > 0 {
		return chain
	}
	// No current_node — an older export. Fall back to every node carrying a
	// message, ordered by time. Scrambled for a branched chat, but better than
	// dropping the conversation.
	for _, n := range c.Mapping {
		if n.Message != nil {
			chain = append(chain, n)
		}
	}
	sort.SliceStable(chain, func(i, j int) bool {
		return chain[i].Message.CreateTime < chain[j].Message.CreateTime
	})
	return chain
}

// joinParts flattens ChatGPT's content parts, which are strings for ordinary
// text and objects for images and tool payloads.
func joinParts(parts []any) string {
	var b strings.Builder
	for _, p := range parts {
		switch t := p.(type) {
		case string:
			b.WriteString(t)
		case map[string]any:
			// An image or attachment. Named rather than dropped, so a reader is
			// not left wondering why a reply refers to something absent.
			if kind, _ := t["content_type"].(string); kind != "" {
				fmt.Fprintf(&b, "_(%s)_", kind)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// ---------------------------------------------------------------- Claude

// claudeConversation is one entry in Claude's conversations.json. Flat, unlike
// ChatGPT's: a list of messages in order.
type claudeConversation struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Messages  []struct {
		Sender    string `json:"sender"`
		Text      string `json:"text"`
		CreatedAt string `json:"created_at"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"chat_messages"`
}

// ParseClaude reads a Claude conversations.json export.
func ParseClaude(raw []byte) ([]Conversation, error) {
	var items []claudeConversation
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("not a Claude conversations.json: %w", err)
	}
	out := make([]Conversation, 0, len(items))
	for _, it := range items {
		c := Conversation{
			Title:   strings.TrimSpace(it.Name),
			Created: rfcTime(it.CreatedAt),
			Source:  "claude",
		}
		for _, m := range it.Messages {
			text := strings.TrimSpace(m.Text)
			if text == "" {
				// Newer exports carry the body in content blocks instead.
				var b strings.Builder
				for _, blk := range m.Content {
					if blk.Type == "text" {
						b.WriteString(blk.Text)
					}
				}
				text = strings.TrimSpace(b.String())
			}
			if text == "" {
				continue
			}
			role := "assistant"
			if strings.EqualFold(m.Sender, "human") || strings.EqualFold(m.Sender, "user") {
				role = "user"
			}
			c.Messages = append(c.Messages, Message{
				Role: role, Text: text, At: rfcTime(m.CreatedAt),
			})
		}
		if len(c.Messages) == 0 {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// ---------------------------------------------------------------- rendering

// Markdown renders one conversation as a note body.
func (c Conversation) Markdown() string {
	var b strings.Builder
	for _, m := range c.Messages {
		who := "**You**"
		if m.Role == "assistant" {
			who = "**Assistant**"
		}
		if !m.At.IsZero() {
			fmt.Fprintf(&b, "%s · %s\n\n", who, m.At.Format("2006-01-02 15:04"))
		} else {
			fmt.Fprintf(&b, "%s\n\n", who)
		}
		b.WriteString(strings.TrimSpace(m.Text))
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// Detect reports which export a file is, so the CLI does not have to ask.
//
// Guessing from the shape rather than the filename: both products call the file
// conversations.json, and a person who renamed it should not have to remember
// which flag to pass.
func Detect(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "[") {
		return ""
	}
	if strings.Contains(trimmed[:min(len(trimmed), 4000)], `"chat_messages"`) {
		return "claude"
	}
	if strings.Contains(trimmed[:min(len(trimmed), 4000)], `"mapping"`) {
		return "chatgpt"
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func unixTime(f float64) time.Time {
	if f <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(f), 0).UTC()
}

func rfcTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05.000000Z", "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
