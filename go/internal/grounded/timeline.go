package grounded

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/bank"
)

// Event is one dated thing that happened (or one standing fact) about one or
// more entities, with the record it came from.
type Event struct {
	Entities []string  `json:"entities"`
	Text     string    `json:"text"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	// Dated is true when the date is the event's own; false when it is only
	// the date of the record that mentions it.
	Dated   bool      `json:"dated"`
	State   bool      `json:"state,omitempty"`
	DocID   string    `json:"doc"`
	DocDate time.Time `json:"doc_date"`
}

// Timeline is the per-entity chronology of a corpus, built once at ingestion.
type Timeline struct {
	Events []Event `json:"events"`
}

// Extractor turns one piece of text into facts. The default is the memory
// bank's extractor.
type Extractor func(ctx context.Context, req bank.PieceRequest) ([]bank.PieceFact, error)

// BankExtractor uses the bank's fact extractor through an ai client.
func BankExtractor(c *ai.Client) Extractor {
	return func(ctx context.Context, req bank.PieceRequest) ([]bank.PieceFact, error) {
		f, _, err := bank.ExtractPiece(ctx, c, req)
		return f, err
	}
}

// TimelineMission tells the extractor what the facts are for. It is about the
// shape of the output, not about any particular corpus.
const TimelineMission = "Build a per-person timeline. Keep every distinct event, activity, trip, purchase, meeting, achievement, plan and change of circumstance each person mentions, even minor ones, one fact each, with the absolute date it happened. Name the person in every fact. Do not merge separate occurrences of a similar activity into one fact."

// BuildTimeline extracts events from every document, parallel workers wide.
// A document whose extraction fails is reported and skipped; the rest of the
// timeline is still returned.
func BuildTimeline(ctx context.Context, docs []Doc, ex Extractor, workers int, mission string, logf func(string, ...any)) (*Timeline, error) {
	if workers < 1 {
		workers = 1
	}
	if mission == "" {
		mission = TimelineMission
	}
	type result struct {
		idx    int
		events []Event
		err    error
	}
	results := make([]result, len(docs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for i := range docs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			d := docs[i]
			names := speakers(d)
			ctxLine := "A conversation"
			if len(names) > 0 {
				ctxLine = "A conversation between " + strings.Join(names, " and ")
			}
			facts, err := ex(ctx, bank.PieceRequest{Text: d.RawText(), EventDate: d.Date, Context: ctxLine,
				Mission: mission, Index: i, Total: len(docs)})
			if err != nil {
				results[i] = result{idx: i, err: err}
				return
			}
			var evs []Event
			for _, f := range facts {
				e := Event{Entities: f.Entities, Text: f.Text, DocID: d.ID, DocDate: d.Date, State: !f.Event}
				if f.Event && !f.Start.IsZero() {
					e.Start, e.End, e.Dated = f.Start, f.End, true
				} else {
					e.Start, e.End = d.Date, d.Date
				}
				evs = append(evs, e)
			}
			results[i] = result{idx: i, events: evs}
			if logf != nil {
				logf("timeline %s: %d facts", d.ID, len(evs))
			}
		}(i)
	}
	wg.Wait()
	t := &Timeline{}
	var firstErr error
	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		t.Events = append(t.Events, r.events...)
	}
	t.sort()
	if failed > 0 {
		return t, fmt.Errorf("%d of %d documents failed: %w", failed, len(docs), firstErr)
	}
	return t, nil
}

func speakers(d Doc) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range d.Lines {
		if l.Speaker != "" && !seen[l.Speaker] {
			seen[l.Speaker] = true
			out = append(out, l.Speaker)
		}
	}
	return out
}

func (t *Timeline) sort() {
	sort.SliceStable(t.Events, func(i, j int) bool {
		if !t.Events[i].Start.Equal(t.Events[j].Start) {
			return t.Events[i].Start.Before(t.Events[j].Start)
		}
		return t.Events[i].DocDate.Before(t.Events[j].DocDate)
	})
}

var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the and or but of to in on at for with from by about as is are was were be been being do does did has have had how what when where which who whom whose why many much times time it its this that these those they them their he she his her him i me my we our you your not no yes any all some does would could should can will just than then there here also after before during since until up down out over under again more most other such only own same so too very s t`) {
		stop[w] = true
	}
}

func tokens(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	var out []string
	for _, w := range f {
		w = strings.TrimSuffix(w, "s")
		if len(w) >= 3 && !stop[w] {
			out = append(out, w)
		}
	}
	return out
}

// Entities returns every distinct entity name with its event count.
func (t *Timeline) Entities() map[string]int {
	m := map[string]int{}
	for _, e := range t.Events {
		for _, n := range e.Entities {
			m[n]++
		}
	}
	return m
}

// Select returns up to max events relevant to a question, oldest first.
// Relevance is the entities the question names (an event about them) plus
// shared content words. When the question names no entity, only word overlap
// counts. It never returns events unrelated to both.
func (t *Timeline) Select(question string, max int) []Event {
	if t == nil || len(t.Events) == 0 {
		return nil
	}
	qlow := strings.ToLower(question)
	qtok := map[string]bool{}
	for _, w := range tokens(question) {
		qtok[w] = true
	}
	named := map[string]bool{}
	for n := range t.Entities() {
		ln := strings.ToLower(strings.TrimSpace(n))
		if len(ln) < 3 || ln == "user" || ln == "assistant" {
			continue
		}
		if containsWord(qlow, ln) {
			named[ln] = true
		}
	}
	type scored struct {
		e Event
		s float64
	}
	var sc []scored
	for _, e := range t.Events {
		s := 0.0
		for _, n := range e.Entities {
			if named[strings.ToLower(strings.TrimSpace(n))] {
				s += 3
				break
			}
		}
		if s == 0 {
			lt := strings.ToLower(e.Text)
			for n := range named {
				if containsWord(lt, n) {
					s += 2
					break
				}
			}
		}
		overlap := 0
		seen := map[string]bool{}
		for _, w := range tokens(e.Text) {
			if qtok[w] && !seen[w] {
				seen[w] = true
				overlap++
			}
		}
		s += float64(overlap)
		if len(named) > 0 && s < 2 { // entity-named question: need the entity
			continue
		}
		if s > 0 {
			sc = append(sc, scored{e, s})
		}
	}
	if len(sc) > max {
		sort.SliceStable(sc, func(i, j int) bool { return sc[i].s > sc[j].s })
		sc = sc[:max]
	}
	out := make([]Event, len(sc))
	for i, s := range sc {
		out[i] = s.e
	}
	(&Timeline{Events: out}).sort()
	return out
}

func containsWord(s, w string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		a, b := i+j, i+j+len(w)
		okL := a == 0 || !isWordRune(rune(s[a-1]))
		okR := b >= len(s) || !isWordRune(rune(s[b]))
		if okL && okR {
			return true
		}
		i = a + 1
	}
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func fmtISO(t time.Time) string { return t.Format("2006-01-02") }

// RenderEvents prints events one per line, oldest first.
func RenderEvents(evs []Event) string {
	var b strings.Builder
	for _, e := range evs {
		switch {
		case e.Dated && e.Start.Equal(e.End):
			fmt.Fprintf(&b, "- [%s] %s\n", fmtISO(e.Start), e.Text)
		case e.Dated:
			fmt.Fprintf(&b, "- [%s to %s] %s\n", fmtISO(e.Start), fmtISO(e.End), e.Text)
		default:
			fmt.Fprintf(&b, "- [mentioned %s] %s\n", fmtISO(e.DocDate), e.Text)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// TimelineFromFacts builds a timeline from facts a memory bank already holds,
// so a vault that has retained its notes into a bank gets the entity timeline
// with no second extraction.
func TimelineFromFacts(facts []bank.RecallFact) *Timeline {
	t := &Timeline{}
	parse := func(s string) time.Time {
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
			if v, err := time.Parse(layout, s); err == nil {
				return v
			}
		}
		return time.Time{}
	}
	for _, f := range facts {
		if f.DocRemoved || strings.TrimSpace(f.Text) == "" {
			continue
		}
		e := Event{Entities: f.Entities, Text: f.Text, DocID: f.DocumentID}
		mentioned := parse(f.MentionedAt)
		e.DocDate = mentioned
		if s := parse(f.OccurredStart); !s.IsZero() {
			e.Start, e.End, e.Dated = s, s, true
			if en := parse(f.OccurredEnd); !en.IsZero() && !en.Before(s) {
				e.End = en
			}
		} else if !mentioned.IsZero() {
			e.Start, e.End, e.State = mentioned, mentioned, true
		} else {
			continue // nothing to place it on a timeline
		}
		t.Events = append(t.Events, e)
	}
	t.sort()
	return t
}
