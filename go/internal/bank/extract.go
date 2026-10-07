package bank

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/usage"
)

// Extraction modes.
const (
	ModeConcise  = "concise"  // a model picks out the facts worth keeping
	ModeVerbatim = "verbatim" // one fact per chunk, the chunk itself, annotated by a model
	ModeChunks   = "chunks"   // one fact per chunk, no model at all
)

// extractInput is one chunk on its way to the extractor.
type extractInput struct {
	Chunk     string
	Index     int
	Total     int
	EventDate time.Time // zero when the source has no time
	Context   string
	Metadata  map[string]string
	Mission   string
	Mode      string
	Causal    bool
}

// extracted is one fact as the extractor returned it, before ids, entity
// resolution and storage.
type extracted struct {
	Text     string
	Type     string
	Kind     string
	OccStart time.Time
	OccEnd   time.Time
	Entities []string
	// Causes are indices into the same chunk's surviving facts.
	Causes []int
}

// The extraction instructions. The wording is our own; what it asks for — a
// selective set of durable facts, each with when/who/why, an event-or-state
// kind, resolved dates and entity names — is what recall is built to use.
const extractSystem = `You read one piece of a conversation or document and write down the facts in it that are worth remembering for months.

KEEP: facts about people (who they are, relationships, jobs, where they live), preferences and opinions, plans and goals, decisions, milestones and events (things that happened, were bought, visited, finished, started, changed), skills and expertise, problems and constraints, and concrete sensory or emotional details that characterise an experience.
SKIP: greetings, thanks, filler ("ok", "sounds good"), process talk ("let me check"), and anything already stated earlier in the same text.
Merge statements about the same thing into one fact. One clear sentence beats several vague ones.

For each fact give:
- "what": the fact itself in one or two self-contained sentences. Replace pronouns and vague references with names ("my sister" + "Emma" -> "Emma (the user's sister)"). Rewrite relative times as absolute dates using the Event Date ("yesterday" -> "on 12 November 2024").
- "when": when it happened or holds, in words, or "N/A".
- "where": the place, or "N/A".
- "who": the people involved and how they relate to the user, or "N/A".
- "why": the reason or significance if it matters, or "N/A".
- "fact_kind": "event" for something that happened at a particular time; "conversation" for an ongoing state, trait or preference.
- "occurred_start" / "occurred_end": for events only, ISO dates (YYYY-MM-DD, or a full timestamp). A point event has start = end. A vague date covers its whole period: "in 2015" is 2015-01-01 to 2015-12-31, "in March 2026" is 2026-03-01 to 2026-03-31. Never shrink a period to its first day, and never use the Event Date itself unless the event happened then. null for states.
- "fact_type": "assistant" only for something the assistant or agent itself did ("I fixed the build"); "world" for everything else, including the user's own preferences, plans and corrections.
- "entities": a plain array of strings naming the people, organisations, places, objects and concepts the fact is about. Include "user" when the fact is about the user. Never objects, never null.

Write facts in the language of the input. Keep names, identifiers, code and quotations verbatim.`

const causalSection = `

CAUSES: when one fact happened because of an earlier fact in your list, add "causal_relations": [{"target_index": <index of the earlier fact>, "relation_type": "caused_by"}] (at most two, and target_index must be smaller than the fact's own index). Otherwise null.`

const verbatimSystem = `You annotate one piece of a conversation or document. Return EXACTLY ONE entry for the whole piece, without a "what" field: give "when", "where", "who" (or "N/A"), "fact_kind" ("event" or "conversation"), "occurred_start" / "occurred_end" for events (ISO dates; a vague date covers its whole period), "fact_type" ("assistant" only for what the assistant itself did, else "world"), and "entities": a plain array of strings naming the people, places, organisations, objects and concepts in it.`

const outputContract = `

Reply with JSON only: {"facts": [ ... ]}. An empty list is a valid answer when nothing is worth keeping.`

func extractUserMessage(in extractInput) string {
	var b strings.Builder
	if m := strings.TrimSpace(in.Mission); m != "" {
		// The bank's retain mission rides in the user message, not the system
		// prompt, so the system prompt is identical for every bank and stays
		// cacheable by providers that cache prefixes.
		b.WriteString("WHAT THIS MEMORY IS FOR (takes priority over the general rules):\n")
		b.WriteString(m)
		b.WriteString("\n\n")
	}
	b.WriteString("Extract facts from this piece.\n\n")
	fmt.Fprintf(&b, "Piece: %d/%d\n", in.Index+1, max(in.Total, 1))
	if in.EventDate.IsZero() {
		b.WriteString("Event Date: Unknown\n")
	} else {
		fmt.Fprintf(&b, "Event Date: %s (%s)\n", in.EventDate.Format("Monday, January 2, 2006"), in.EventDate.Format("2006-01-02"))
	}
	ctx := strings.TrimSpace(in.Context)
	if ctx == "" {
		ctx = "none"
	}
	b.WriteString("Context: " + ctx + "\n")
	if len(in.Metadata) > 0 {
		keys := make([]string, 0, len(in.Metadata))
		for k := range in.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("Metadata:\n")
		for _, k := range keys {
			b.WriteString("  " + k + ": " + in.Metadata[k] + "\n")
		}
	}
	b.WriteString("\nContent:\n")
	b.WriteString(renderForModel(in.Chunk))
	return b.String()
}

// errTruncated means the reply hit the output budget.
var errTruncated = errors.New("extraction reply truncated")

const (
	extractAttempts    = 4
	minSplittableChunk = 500
	extractMaxTokens   = 16000
)

// extractChunk extracts one chunk with a model, retrying malformed replies and
// splitting a chunk whose reply overflowed the output budget.
func (e *Engine) extractChunk(ctx context.Context, client *ai.Client, in extractInput) ([]extracted, usage.Tokens, error) {
	var spent usage.Tokens
	system := extractSystem
	if in.Mode == ModeVerbatim {
		system = verbatimSystem
	} else if in.Causal {
		system += causalSection
	}
	system += outputContract
	var lastErr error
	for attempt := 0; attempt < extractAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, spent, err
		}
		comp, err := client.CompleteWith(ctx, extractUserMessage(in), ai.CompleteOpts{
			System: system, Temperature: ai.Temp(0.1), MaxTokens: extractMaxTokens, JSON: true})
		spent.Input += comp.Usage.Input
		spent.Output += comp.Usage.Output
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, spent, ctx.Err()
			}
			continue
		}
		if comp.Truncated() {
			lastErr = errTruncated
			break
		}
		facts, malformed, perr := parseExtraction(comp.Text, in)
		if perr != nil {
			lastErr = perr
			continue
		}
		total := len(facts) + malformed
		// A reply in which a fifth of the facts did not parse is retried:
		// the model lost the format, and what did parse is likely partial.
		if malformed > 0 && total > 0 && float64(len(facts)) < 0.8*float64(total) && attempt < extractAttempts-1 {
			lastErr = fmt.Errorf("%d of %d facts malformed", malformed, total)
			continue
		}
		return facts, spent, nil
	}
	if errors.Is(lastErr, errTruncated) {
		if runeLen(in.Chunk) <= minSplittableChunk {
			return nil, spent, nil // too small to split; nothing recoverable
		}
		a, b := splitChunkInTwo(in.Chunk)
		left, t1, err := e.extractChunk(ctx, client, withChunk(in, a))
		spent.Input, spent.Output = spent.Input+t1.Input, spent.Output+t1.Output
		if err != nil {
			return nil, spent, err
		}
		right, t2, err := e.extractChunk(ctx, client, withChunk(in, b))
		spent.Input, spent.Output = spent.Input+t2.Input, spent.Output+t2.Output
		if err != nil {
			return nil, spent, err
		}
		for i := range right {
			for j := range right[i].Causes {
				right[i].Causes[j] += len(left)
			}
		}
		return append(left, right...), spent, nil
	}
	return nil, spent, fmt.Errorf("extracting chunk %d: %w", in.Index, lastErr)
}

func withChunk(in extractInput, chunk string) extractInput {
	in.Chunk = chunk
	return in
}

// splitChunkInTwo halves a chunk whose extraction overflowed: a JSON
// conversation between turns, prose at a sentence break near the middle.
func splitChunkInTwo(s string) (string, string) {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "[") {
		var turns []json.RawMessage
		if json.Unmarshal([]byte(t), &turns) == nil && len(turns) >= 2 && allObjects(turns) {
			half := func(xs []json.RawMessage) string {
				parts := make([]string, len(xs))
				for i, x := range xs {
					parts[i] = compact(x)
				}
				return "[" + strings.Join(parts, ",") + "]"
			}
			return half(turns[:len(turns)/2]), half(turns[len(turns)/2:])
		}
	}
	r := []rune(s)
	mid := len(r) / 2
	lo, hi := mid-len(r)/5, mid+len(r)/5
	best := -1
	for _, sep := range []string{"\n\n", ". ", "! ", "? "} {
		sr := []rune(sep)
		for i := min(hi, len(r)-len(sr)); i >= max(lo, 0); i-- {
			if string(r[i:i+len(sr)]) == sep {
				best = i + len(sr)
				break
			}
		}
		if best >= 0 {
			break
		}
	}
	if best < 0 {
		best = mid
	}
	return string(r[:best]), string(r[best:])
}

// parseExtraction reads a model's reply leniently: a bare list is accepted
// for {"facts": list}, alternate key names are tolerated, and a fact is
// skipped rather than failing the whole reply.
func parseExtraction(reply string, in extractInput) (out []extracted, malformed int, err error) {
	var top any
	if err := ai.DecodeJSON(reply, &top); err != nil {
		return nil, 0, err
	}
	var list []any
	switch t := top.(type) {
	case map[string]any:
		l, ok := t["facts"].([]any)
		if !ok {
			if _, has := t["facts"]; has && t["facts"] == nil {
				return nil, 0, nil
			}
			return nil, 0, fmt.Errorf("reply has no facts list")
		}
		list = l
	case []any:
		list = t
	default:
		return nil, 0, fmt.Errorf("reply is not an object")
	}
	if in.Mode == ModeVerbatim {
		return verbatimFacts(list, in), 0, nil
	}
	// raw index → position in out, for causal relations
	posOf := map[int]int{}
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			malformed++
			continue
		}
		what, found := firstValue(m, "what", "factual_core", "text", "fact")
		if !found {
			malformed++
			continue
		}
		if what == "" {
			continue
		}
		parts := []string{what}
		if v, _ := firstValue(m, "when"); v != "" {
			parts = append(parts, "When: "+v)
		}
		if v, _ := firstValue(m, "who"); v != "" {
			parts = append(parts, "Involving: "+v)
		}
		if v, _ := firstValue(m, "why"); v != "" {
			parts = append(parts, v)
		}
		f := extracted{Text: normFactText(strings.Join(parts, " | "))}
		if degenerate(f.Text) {
			continue
		}
		f.Type, f.Kind = factTypeKind(m)
		if f.Kind == "event" {
			f.OccStart, f.OccEnd = eventDates(m, f.Text, in.EventDate)
		}
		f.Entities = entityList(m["entities"])
		if in.Causal {
			if rels, ok := m["causal_relations"].([]any); ok {
				for _, r := range rels {
					rm, ok := r.(map[string]any)
					if !ok {
						continue
					}
					tgt, ok := rm["target_index"].(float64)
					rt, _ := rm["relation_type"].(string)
					if !ok || (rt != "" && rt != "caused_by") {
						continue
					}
					if p, ok := posOf[int(tgt)]; ok && int(tgt) < i {
						f.Causes = append(f.Causes, p)
					}
				}
			}
		}
		posOf[i] = len(out)
		out = append(out, f)
	}
	return out, malformed, nil
}

func verbatimFacts(list []any, in extractInput) []extracted {
	f := extracted{Text: normFactText(in.Chunk), Type: "world", Kind: "conversation"}
	seen := map[string]bool{}
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if i == 0 {
			f.Type, f.Kind = factTypeKind(m)
			if f.Kind == "event" {
				f.OccStart, f.OccEnd = eventDates(m, "", in.EventDate)
			}
		}
		for _, en := range entityList(m["entities"]) {
			if !seen[strings.ToLower(en)] {
				seen[strings.ToLower(en)] = true
				f.Entities = append(f.Entities, en)
			}
		}
	}
	if f.Text == "" {
		return nil
	}
	return []extracted{f}
}

// firstValue reads the first present key as text. found reports whether any
// of the keys existed; an "N/A" or empty value is present but empty.
func firstValue(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			s := strings.TrimSpace(t)
			if strings.EqualFold(s, "n/a") || strings.EqualFold(s, "none") || strings.EqualFold(s, "null") {
				s = ""
			}
			return s, true
		case nil:
			return "", true
		case float64, bool:
			return fmt.Sprint(t), true
		default:
			return "", true
		}
	}
	return "", false
}

func factTypeKind(m map[string]any) (typ, kind string) {
	ft, _ := m["fact_type"].(string)
	fk, _ := m["fact_kind"].(string)
	ft, fk = strings.ToLower(strings.TrimSpace(ft)), strings.ToLower(strings.TrimSpace(fk))
	switch {
	case ft == "assistant" || ft == "experience":
		typ = "experience"
	case ft == "world":
		typ = "world"
	case fk == "assistant":
		typ = "experience"
	default:
		typ = "world"
	}
	switch fk {
	case "event":
		kind = "event"
	default:
		kind = "conversation"
	}
	return typ, kind
}

// eventDates reads an event's dates, widening coarse values, and falls back to
// dates written in the fact text itself, resolved against when it was said.
func eventDates(m map[string]any, text string, ref time.Time) (time.Time, time.Time) {
	base := ref
	if base.IsZero() {
		base = time.Now().UTC()
	}
	var start, end time.Time
	if s, _ := firstValue(m, "occurred_start"); s != "" {
		if w, ok := parseLooseDate(s, base); ok {
			start, end = w.Start, w.End
		}
	}
	if s, _ := firstValue(m, "occurred_end"); s != "" && !start.IsZero() {
		if w, ok := parseLooseDate(s, base); ok && !w.End.Before(start) {
			end = w.End
		}
	}
	if start.IsZero() && text != "" && !ref.IsZero() {
		if w, ok := ParseWindow(text, ref); ok {
			start, end = w.Start, w.End
		}
	}
	return start, storedEnd(start, end)
}

// storedEnd records an inclusive day-granular end as the day itself, so a
// point event reads start = end in the file.
func storedEnd(start, end time.Time) time.Time {
	if start.IsZero() {
		return time.Time{}
	}
	if end.IsZero() {
		return start
	}
	if end.Hour() == 23 && end.Minute() == 59 && end.Second() == 59 {
		return day(end.Year(), end.Month(), end.Day())
	}
	return end
}

func entityList(v any) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = NormalizeEntity(s)
		if s == "" || seen[strings.ToLower(s)] {
			return
		}
		switch strings.ToLower(s) {
		case "none", "null", "n/a":
			return
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	switch t := v.(type) {
	case []any:
		for _, x := range t {
			switch y := x.(type) {
			case string:
				add(y)
			case map[string]any:
				if s, ok := y["text"].(string); ok {
					add(s)
				} else if s, ok := y["name"].(string); ok {
					add(s)
				}
			}
		}
	case string:
		for _, x := range strings.Split(t, ",") {
			add(x)
		}
	}
	return out
}

// degenerate reports text that carries no fact: blank, punctuation only, or
// two characters or fewer of anything else.
func degenerate(s string) bool {
	alnum := 0
	for _, r := range s {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r > 127 {
			alnum++
		}
	}
	return alnum <= 2
}

// ruleExtract is extraction without a model: each sentence of the chunk is a
// fact, its entities are the capitalised names in it, and a date written in it
// ("last Tuesday", "in March 2023") is resolved against when it was said and
// makes it an event.
func ruleExtract(in extractInput) []extracted {
	var out []extracted
	for _, line := range strings.Split(renderForModel(in.Chunk), "\n") {
		speaker := ""
		if i := strings.Index(line, ": "); i > 0 && i < 40 && !strings.Contains(line[:i], ". ") {
			speaker = strings.TrimSpace(line[:i])
			if j := strings.Index(speaker, " ["); j > 0 {
				speaker = speaker[:j]
			}
			line = line[i+2:]
		}
		for _, s := range memory.ExtractFacts(line) {
			s = normFactText(s)
			if degenerate(s) || len(strings.Fields(s)) < 3 {
				continue
			}
			text := s
			if speaker != "" {
				text = speaker + ": " + s
			}
			f := extracted{Text: text, Type: "world", Kind: "conversation", Entities: ruleEntities(s)}
			if speaker != "" && !strings.EqualFold(speaker, "unknown") {
				f.Entities = append([]string{speaker}, f.Entities...)
				f.Entities = entityList(toAny(f.Entities))
			}
			if !in.EventDate.IsZero() {
				if w, ok := ParseWindow(s, in.EventDate); ok {
					f.Kind, f.OccStart, f.OccEnd = "event", w.Start, storedEnd(w.Start, w.End)
				}
			}
			out = append(out, f)
		}
	}
	return out
}

func toAny(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}
