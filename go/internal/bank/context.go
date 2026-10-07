package bank

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// DefaultContextChars is how much a session-start injection may render.
// Claude Code replaces hook output longer than 10,000 characters with a short
// stub, so the default leaves headroom under that line.
const DefaultContextChars = 9000

// Context item kinds, in the order they are shown.
const (
	KindDigest    = "digest"
	KindDirective = "directive"
	KindModel     = "model"
	KindObserve   = "observation"
	KindFact      = "fact"
)

var kindTitles = map[string]string{
	KindDigest:    "Where we left off",
	KindDirective: "Standing rules",
	KindModel:     "What the bank knows",
	KindObserve:   "Patterns",
	KindFact:      "Recent facts",
}

// ContextItem is one candidate for the injection. Items are given in
// descending value; the fitter drops from the end.
type ContextItem struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Text string `json:"text"`
}

// ContextOptions shape a session-start injection.
type ContextOptions struct {
	// MaxChars is the rendered limit; zero means DefaultContextChars.
	MaxChars int
	// Source is how the session began: startup, resume, clear or compact.
	Source string
}

// SessionContext is the rendered injection and what it cost.
type SessionContext struct {
	Context  string   `json:"context"`
	Chars    int      `json:"chars"`
	Limit    int      `json:"limit"`
	Included int      `json:"included"`
	Dropped  int      `json:"dropped"`
	Kinds    []string `json:"kinds"`
}

func clampChars(n int) int {
	switch {
	case n <= 0:
		return DefaultContextChars
	case n < 300:
		return 300
	case n > 100000:
		return 100000
	}
	return n
}

// renderContext lays items out under their section titles.
func renderContext(bankID string, items []ContextItem, dropped int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<grimoire_bank_context bank=%q>\n", bankID)
	b.WriteString("Memory from this repository's bank: a record of the past that may or may not bear " +
		"on the task. Verify it against the code.\n")
	last := ""
	for _, it := range items {
		if it.Kind != last {
			fmt.Fprintf(&b, "\n## %s\n", kindTitles[it.Kind])
			last = it.Kind
		}
		b.WriteString(it.Text)
		if !strings.HasSuffix(it.Text, "\n") {
			b.WriteString("\n")
		}
	}
	if dropped > 0 {
		fmt.Fprintf(&b, "\n(%d lower-value items left out; ask with bank_recall for more.)\n", dropped)
	}
	b.WriteString("</grimoire_bank_context>")
	return b.String()
}

// FitContext renders items under limit characters. It measures the rendered
// text, drops the lowest-value (last) item and measures again until it fits;
// if the single most valuable item alone is too long it is cut at a line
// boundary. It returns the text and how many items were kept and dropped.
func FitContext(bankID string, items []ContextItem, limit int) (string, int, int) {
	limit = clampChars(limit)
	if len(items) == 0 {
		return "", 0, 0
	}
	kept := append([]ContextItem(nil), items...)
	for len(kept) > 1 {
		dropped := len(items) - len(kept)
		if utf8.RuneCountInString(renderContext(bankID, kept, dropped)) <= limit {
			break
		}
		kept = kept[:len(kept)-1]
	}
	dropped := len(items) - len(kept)
	out := renderContext(bankID, kept, dropped)
	if utf8.RuneCountInString(out) > limit {
		// One item remains and it is too long: trim its text to what is left.
		empty := utf8.RuneCountInString(renderContext(bankID, []ContextItem{{Kind: kept[0].Kind}}, dropped))
		room := limit - empty - 2
		if room < 0 {
			room = 0
		}
		kept[0].Text = cutAtLine(kept[0].Text, room)
		out = renderContext(bankID, kept, dropped)
	}
	return out, len(kept), dropped
}

func cutAtLine(s string, room int) string {
	r := []rune(s)
	if len(r) <= room {
		return s
	}
	if room <= 1 {
		return ""
	}
	cut := string(r[:room-1])
	if i := strings.LastIndex(cut, "\n"); i > room/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " \n") + "…"
}

func oneLineText(s string) string { return strings.Join(strings.Fields(s), " ") }

// contextItems gathers candidates in descending value: the last session's
// digest, directives, mental models (a person's first), observations, then
// facts (a person's first, newest first).
func (e *Engine) contextItems(bankID string, o ContextOptions) ([]ContextItem, error) {
	var items []ContextItem
	if d := e.latestDigestItem(bankID); d != nil {
		items = append(items, *d)
	}
	dirs, err := e.ListDirectives(bankID, nil, true)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(dirs, func(i, j int) bool { return dirs[i].Priority > dirs[j].Priority })
	for _, d := range dirs {
		items = append(items, ContextItem{Kind: KindDirective, ID: d.ID, Text: "- " + oneLineText(d.Text)})
	}
	models, err := e.ListModels(bankID, ModelQuery{Detail: true})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(models, func(i, j int) bool {
		return models[i].Authority == "human" && models[j].Authority != "human"
	})
	for _, m := range models {
		if strings.TrimSpace(m.Body) == "" {
			continue
		}
		items = append(items, ContextItem{Kind: KindModel, ID: m.ID,
			Text: "### " + m.Name + "\n" + strings.TrimSpace(m.Body)})
	}
	obs, _, _, err := e.ListObservations(bankID, ObservationQuery{Limit: 500})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(obs, func(i, j int) bool {
		hi, hj := obs[i].Authority == "human", obs[j].Authority == "human"
		if hi != hj {
			return hi
		}
		return obs[i].ProofCount > obs[j].ProofCount
	})
	for _, ob := range obs {
		items = append(items, ContextItem{Kind: KindObserve, ID: ob.ID, Text: "- " + oneLineText(ob.Text)})
	}
	facts, _, err := e.ListFacts(bankID, FactQuery{Type: "", Limit: 500})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(facts, func(i, j int) bool {
		hi, hj := facts[i].Authority == "human", facts[j].Authority == "human"
		if hi != hj {
			return hi
		}
		return factWhen(facts[i]) > factWhen(facts[j])
	})
	for _, f := range facts {
		if f.Type == "observation" {
			continue
		}
		line := "- " + oneLineText(f.Text)
		if f.Authority == "human" {
			line += " [written by a person]"
		}
		if w := factWhen(f); len(w) >= 10 {
			line += " (" + w[:10] + ")"
		}
		items = append(items, ContextItem{Kind: KindFact, ID: f.ID, Text: line})
	}
	return items, nil
}

func factWhen(f RecallFact) string {
	if f.MentionedAt != "" {
		return f.MentionedAt
	}
	return f.OccurredStart
}

// SessionContext renders what a coding agent is shown when a session starts,
// within the character limit. It needs no model.
func (e *Engine) SessionContext(bankID string, o ContextOptions) (*SessionContext, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, err
	}
	items, err := e.contextItems(bankID, o)
	if err != nil {
		return nil, err
	}
	limit := clampChars(o.MaxChars)
	text, kept, dropped := FitContext(bankID, items, limit)
	res := &SessionContext{Context: text, Limit: limit, Included: kept, Dropped: dropped, Kinds: []string{}}
	res.Chars = utf8.RuneCountInString(text)
	seen := map[string]bool{}
	for _, it := range items[:kept] {
		if !seen[it.Kind] {
			seen[it.Kind] = true
			res.Kinds = append(res.Kinds, it.Kind)
		}
	}
	return res, nil
}
