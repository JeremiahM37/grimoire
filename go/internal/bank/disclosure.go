package bank

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Progressive disclosure: a cheap index of what a bank holds, a timeline
// around one entry, and a fetch of full entries by id. An agent skims the
// index for a few hundred tokens, then asks for the handful it needs, instead
// of paying for fifty full facts to find three.
//
// Entries are cited by a short reference, "#" and the first eight characters
// of the entry's id. A reference resolves by unique prefix; the full id works
// everywhere a short one does.

const (
	refLen     = 8
	minRefLen  = 4
	titleChars = 110
)

// IndexEntry is one line of the index.
type IndexEntry struct {
	Ref   string `json:"ref"`
	ID    string `json:"id"`
	Type  string `json:"type"` // fact | observation
	Title string `json:"title"`
	Date  string `json:"date,omitempty"`
	Human bool   `json:"human,omitempty"`
	// Tokens is a rough cost of fetching the entry in full.
	Tokens int `json:"tokens"`
}

// IndexQuery filters the index.
type IndexQuery struct {
	// Query ranks by relevance (recall); empty lists newest first.
	Query string
	// Types limits to fact and/or observation.
	Types []string
	// Since keeps entries dated on or after this day (YYYY-MM-DD).
	Since  string
	Limit  int
	Offset int
}

// ShortRef is the citation form of an id.
func ShortRef(id string) string {
	if len(id) > refLen {
		id = id[:refLen]
	}
	return "#" + id
}

func titleOf(text string) string {
	if i := strings.Index(text, " | "); i > 0 {
		text = text[:i]
	}
	return clip(text, titleChars)
}

func roughTokens(text string) int { return (len([]rune(text)) + 3) / 4 }

type disclosed struct {
	entry IndexEntry
	text  string
}

// entries lists every fact and observation of a bank as index entries.
func (e *Engine) entries(bankID string) ([]disclosed, error) {
	facts, _, err := e.ListFacts(bankID, FactQuery{Limit: 1 << 30})
	if err != nil {
		return nil, err
	}
	out := make([]disclosed, 0, len(facts))
	for _, f := range facts {
		if f.Type == "observation" {
			continue
		}
		out = append(out, disclosed{entry: IndexEntry{Ref: ShortRef(f.ID), ID: f.ID, Type: "fact",
			Title: titleOf(f.Text), Date: isoDay(factWhen(f)), Human: f.Authority == "human",
			Tokens: roughTokens(f.Text)}, text: f.Text})
	}
	obs, _, _, err := e.ListObservations(bankID, ObservationQuery{Limit: 1 << 30})
	if err != nil {
		return nil, err
	}
	for _, o := range obs {
		d := o.UpdatedAt
		if d == "" {
			d = o.MentionedAt
		}
		out = append(out, disclosed{entry: IndexEntry{Ref: ShortRef(o.ID), ID: o.ID, Type: "observation",
			Title: titleOf(o.Text), Date: isoDay(d), Human: o.Authority == "human",
			Tokens: roughTokens(o.Text)}, text: o.Text})
	}
	return out, nil
}

func isoDay(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return ""
}

func typeAllowed(types []string, t string) bool {
	if len(types) == 0 {
		return true
	}
	for _, x := range types {
		if strings.EqualFold(strings.TrimSpace(x), t) {
			return true
		}
	}
	return false
}

// BankIndex returns a compact, citable index of a bank's entries.
func (e *Engine) BankIndex(ctx context.Context, bankID string, q IndexQuery) ([]IndexEntry, int, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, 0, err
	}
	all, err := e.entries(bankID)
	if err != nil {
		return nil, 0, err
	}
	byID := map[string]IndexEntry{}
	for _, d := range all {
		byID[d.entry.ID] = d.entry
	}
	var list []IndexEntry
	if strings.TrimSpace(q.Query) != "" {
		maxTokens := 1 << 20
		res, err := e.Recall(ctx, bankID, RecallRequest{Query: q.Query, Budget: "mid", MaxTokens: &maxTokens})
		if err != nil {
			return nil, 0, err
		}
		for _, r := range res.Results {
			if ent, ok := byID[r.ID]; ok {
				list = append(list, ent)
			}
		}
	} else {
		for _, d := range all {
			list = append(list, d.entry)
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].Date > list[j].Date })
	}
	filtered := list[:0]
	for _, ent := range list {
		if !typeAllowed(q.Types, ent.Type) || (q.Since != "" && ent.Date < q.Since) {
			continue
		}
		filtered = append(filtered, ent)
	}
	total := len(filtered)
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if q.Offset >= total {
		return []IndexEntry{}, total, nil
	}
	end := q.Offset + limit
	if end > total {
		end = total
	}
	return filtered[q.Offset:end], total, nil
}

// resolveRef finds an entry by full id or unique prefix, with or without "#".
func resolveRef(all []disclosed, ref string) (*disclosed, error) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "#")
	if len(ref) < minRefLen {
		return nil, fmt.Errorf("%w: reference %q is too short", ErrInvalid, ref)
	}
	var hit *disclosed
	for i := range all {
		if all[i].entry.ID == ref {
			return &all[i], nil
		}
		if strings.HasPrefix(all[i].entry.ID, ref) {
			if hit != nil {
				return nil, fmt.Errorf("%w: reference #%s is ambiguous; use more characters", ErrInvalid, ref)
			}
			hit = &all[i]
		}
	}
	if hit == nil {
		return nil, fmt.Errorf("%w: no entry #%s", ErrNotFound, ref)
	}
	return hit, nil
}

// TimelineResult is the entries around an anchor, oldest first.
type TimelineResult struct {
	Anchor  string       `json:"anchor"`
	Entries []IndexEntry `json:"entries"`
	// AnchorRef marks which entry the window is centred on, if the anchor was an entry.
	AnchorRef string `json:"anchor_ref,omitempty"`
}

// Timeline returns the entries dated around an anchor: an entry reference, or
// a day (YYYY-MM-DD). before and after count entries on either side.
func (e *Engine) Timeline(bankID, anchor string, before, after int) (*TimelineResult, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, err
	}
	all, err := e.entries(bankID)
	if err != nil {
		return nil, err
	}
	var dated []IndexEntry
	for _, d := range all {
		if d.entry.Date != "" {
			dated = append(dated, d.entry)
		}
	}
	sort.SliceStable(dated, func(i, j int) bool {
		if dated[i].Date != dated[j].Date {
			return dated[i].Date < dated[j].Date
		}
		return dated[i].ID < dated[j].ID
	})
	if before < 0 || before > 50 {
		before = 5
	}
	if after < 0 || after > 50 {
		after = 5
	}
	res := &TimelineResult{Anchor: anchor, Entries: []IndexEntry{}}
	anchor = strings.TrimSpace(anchor)
	pos := -1
	if len(anchor) >= 10 && anchor[4] == '-' && anchor[7] == '-' {
		date := anchor[:10]
		pos = sort.Search(len(dated), func(i int) bool { return dated[i].Date >= date })
		// A day anchor centres on the first entry from that day on; before
		// counts the entries ahead of it, after the rest including it.
		start, end := pos-before, pos+after
		return e.window(res, dated, start, end), nil
	}
	hit, err := resolveRef(all, anchor)
	if err != nil {
		return nil, err
	}
	for i := range dated {
		if dated[i].ID == hit.entry.ID {
			pos = i
		}
	}
	if pos < 0 {
		return nil, fmt.Errorf("%w: entry #%s has no date to place on a timeline", ErrInvalid, hit.entry.ID[:min(refLen, len(hit.entry.ID))])
	}
	res.AnchorRef = hit.entry.Ref
	return e.window(res, dated, pos-before, pos+after+1), nil
}

func (e *Engine) window(res *TimelineResult, dated []IndexEntry, start, end int) *TimelineResult {
	if start < 0 {
		start = 0
	}
	if end > len(dated) {
		end = len(dated)
	}
	if start < end {
		res.Entries = dated[start:end]
	}
	return res
}

// DisclosedItem is an entry in full.
type DisclosedItem struct {
	IndexEntry
	Text string `json:"text"`
}

// GetByIDs returns entries in full by reference or id. References that match
// nothing are listed, not an error, so one bad citation does not lose the rest.
func (e *Engine) GetByIDs(bankID string, refs []string) ([]DisclosedItem, []string, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, nil, err
	}
	all, err := e.entries(bankID)
	if err != nil {
		return nil, nil, err
	}
	items := []DisclosedItem{}
	var missing []string
	seen := map[string]bool{}
	for _, ref := range refs {
		hit, err := resolveRef(all, ref)
		if err != nil {
			missing = append(missing, strings.TrimPrefix(strings.TrimSpace(ref), "#"))
			continue
		}
		if seen[hit.entry.ID] {
			continue
		}
		seen[hit.entry.ID] = true
		items = append(items, DisclosedItem{IndexEntry: hit.entry, Text: hit.text})
	}
	return items, missing, nil
}
