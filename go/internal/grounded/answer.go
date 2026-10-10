package grounded

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
)

// Reply is one model reply and what it cost.
type Reply struct {
	Text    string
	In, Out int
	CostUSD float64
	Millis  int64
}

// LLM is the one model call the procedure needs.
type LLM interface {
	Complete(ctx context.Context, system, prompt string) (Reply, error)
}

// ClientLLM adapts an ai.Client.
type ClientLLM struct {
	C       *ai.Client
	Backend string // empty = the client's configured backend
}

// Complete implements LLM.
func (c ClientLLM) Complete(ctx context.Context, system, prompt string) (Reply, error) {
	opts := ai.CompleteOpts{System: system, Backend: c.Backend}
	backend := c.Backend
	if backend == "" {
		backend = c.C.Backend()
	}
	if backend == ai.BackendClaudeCLI {
		comp, cli, err := c.C.CompleteCLI(ctx, prompt, opts)
		return Reply{Text: comp.Text, In: comp.Usage.Input, Out: comp.Usage.Output, CostUSD: cli.CostUSD, Millis: cli.Millis}, err
	}
	started := time.Now()
	comp, err := c.C.CompleteWith(ctx, prompt, opts)
	return Reply{Text: comp.Text, In: comp.Usage.Input, Out: comp.Usage.Output, Millis: time.Since(started).Milliseconds()}, err
}

// Options choose which parts of the feature are on.
type Options struct {
	// Procedure turns on the gather -> answer -> self-check steps. Off, the
	// question is answered in one call from the records.
	Procedure bool
	// Timeline appends the question's slice of the entity timeline to the
	// records the model sees.
	Timeline  bool
	MaxEvents int
	// DirectTemplate replaces the single-call prompt. It may contain
	// {context} and {question}. Empty uses the built-in wording.
	DirectTemplate string
	// System is the system prompt for every call.
	System string
}

// DefaultSystem is the system prompt used when none is given.
const DefaultSystem = "You answer questions strictly from the supplied records."

// Source supplies the records a question is answered from. A source that
// holds everything returns the whole text; one backed by search returns the
// passages it retrieved. The procedure is the same either way.
type Source interface {
	Records(ctx context.Context, question string) (Records, error)
}

// Records is what a source hands the answerer.
type Records struct {
	Text     string
	Timeline *Timeline
}

// WholeSource serves an entire prepared corpus as the context, the right
// choice when it fits the model's window.
type WholeSource struct {
	P        *Prepared
	Annotate bool
}

// Records implements Source.
func (w WholeSource) Records(_ context.Context, _ string) (Records, error) {
	return Records{Text: w.P.Render(RenderOpts{Annotate: w.Annotate}), Timeline: w.P.Timeline}, nil
}

// Passage is one retrieved piece of a record.
type Passage struct {
	Title string
	Date  time.Time
	Text  string
}

// RetrievalSource serves the passages a search returned. Annotate resolves
// relative times against each passage's own date, so a passage lifted out of
// its record still says which day "yesterday" was.
type RetrievalSource struct {
	Fetch    func(ctx context.Context, question string) ([]Passage, error)
	Annotate bool
	Timeline *Timeline
}

// Records implements Source.
func (r RetrievalSource) Records(ctx context.Context, q string) (Records, error) {
	ps, err := r.Fetch(ctx, q)
	if err != nil {
		return Records{}, err
	}
	var b strings.Builder
	for i, p := range ps {
		if i > 0 {
			b.WriteString("\n\n")
		}
		title := p.Title
		if !p.Date.IsZero() {
			title += " — " + p.Date.Format("2 January 2006")
		}
		b.WriteString("## " + strings.TrimSpace(title) + "\n")
		text := p.Text
		if r.Annotate && !p.Date.IsZero() {
			text = AnnotateText(text, p.Date)
		}
		b.WriteString(strings.TrimSpace(text))
	}
	return Records{Text: b.String(), Timeline: r.Timeline}, nil
}

// Result is one answer with the working that produced it.
type Result struct {
	Answer   string  `json:"answer"`
	Evidence string  `json:"evidence,omitempty"` // step 1: the gathered list
	Check    string  `json:"check,omitempty"`    // steps 2-3: draft and self-check notes
	Calls    int     `json:"calls"`
	In       int     `json:"in_tokens"`
	Out      int     `json:"out_tokens"`
	CostUSD  float64 `json:"cost_usd"`
	Millis   int64   `json:"ms"`
	Events   int     `json:"timeline_events,omitempty"`
}

// Answerer runs the procedure.
type Answerer struct {
	LLM LLM
	Opt Options
}

const defaultDirect = `You are answering a question about recorded conversations or notes. Use ONLY the records below.

<records>
{context}
</records>

Question: {question}

Reply with ONLY the short answer (a few words; write dates like "7 May 2023"). If the records do not contain the answer, give your best guess. Do not explain, do not use tools.`

const recordsGuide = `Each record has a header with its date. Where a relative time expression ("last Saturday", "two weeks ago") is followed by a bracket such as [= Sat 20 May 2023], the bracket is the absolute date computed from that record's own date; trust it over doing the arithmetic yourself. A bracket starting with ≈ is approximate.`

const timelineGuide = `A timeline of dated events extracted from the records follows them. It is a convenience index, may be incomplete, and the records win wherever they disagree.`

const gatherTemplate = `You are step 1 of a careful question-answering procedure over recorded conversations or notes. Do NOT answer the question yet.

` + recordsGuide + `

<records>
{context}
</records>

Question: {question}

First line: "TYPE: <single fact | list | count | date or time | yes/no | inference>".
Then list every passage or event in the records that bears on the question, one per line, as:
- <date> | <who said or did it> | <the relevant words, quoted or tightly paraphrased>
Rules:
- Be exhaustive. For a question asking for all / every / which ones / what kinds / how many / how often, scan the whole of the records and list every matching item or occurrence separately, including minor ones and ones mentioned only in passing. Do not stop at the first few.
- Give each item's own date: resolve relative expressions from the date of the record that contains them (use a bracketed absolute date when one is given); otherwise give the record's date.
- Say who spoke and who the item is about; they can differ.
- Include passages that seem to disagree with each other, marked as such.
- Output only the TYPE line and the list.`

const checkTemplate = `You are steps 2 and 3 of a careful question-answering procedure over recorded conversations or notes.

` + recordsGuide + `

<records>
{context}
</records>

Question: {question}

Step 1 produced this list of relevant passages (it may be incomplete or contain mistakes):
<evidence>
{evidence}
</evidence>

Step 2: draft an answer from the evidence.
Step 3: check the draft against the records and fix it if any check fails:
(a) Completeness: if the question asks for a list or a count, scan the records again for items or occurrences the evidence missed, and add them. Count only distinct occurrences.
(b) Dates: recompute every date, duration and "how long ago" from the dates of the records involved, and show the arithmetic briefly. A relative expression is relative to the record it appears in.
(c) Attribution: confirm who said or did it. Answer about the person asked about, not the other speaker.
(d) Commitment: if the records strongly imply an answer without stating it, give the best-supported answer rather than saying it is unknown.

Write your brief working first. Then end with one final line of the form
FINAL: <the answer>
The answer is as short as the question allows: a few words, "yes"/"no", a date written like "7 May 2023", or the complete list of items. Give no justification or restatement of the question in the FINAL line, and nothing after it.`

var finalRE = regexp.MustCompile(`(?is)FINAL\s*:\s*(.+)$`)

// ErrEmpty is returned when the model gave no usable answer.
var ErrEmpty = errors.New("empty answer")

func fill(t string, kv ...string) string {
	for i := 0; i+1 < len(kv); i += 2 {
		t = strings.ReplaceAll(t, kv[i], kv[i+1])
	}
	return t
}

// Answer answers a question from the source.
func (a *Answerer) Answer(ctx context.Context, question string, src Source) (Result, error) {
	recs, err := src.Records(ctx, question)
	if err != nil {
		return Result{}, err
	}
	var res Result
	text := recs.Text
	if a.Opt.Timeline && recs.Timeline != nil {
		max := a.Opt.MaxEvents
		if max <= 0 {
			max = 120
		}
		if evs := recs.Timeline.Select(question, max); len(evs) > 0 {
			text += "\n\n## Timeline of dated events (extracted index)\n" + timelineGuide + "\n" + RenderEvents(evs)
			res.Events = len(evs)
		}
	}
	system := a.Opt.System
	if system == "" {
		system = DefaultSystem
	}
	call := func(prompt string) (string, error) {
		rep, err := a.LLM.Complete(ctx, system, prompt)
		res.Calls++
		res.In += rep.In
		res.Out += rep.Out
		res.CostUSD += rep.CostUSD
		res.Millis += rep.Millis
		return rep.Text, err
	}
	if !a.Opt.Procedure {
		t := a.Opt.DirectTemplate
		if t == "" {
			t = defaultDirect
		}
		out, err := call(fill(t, "{question}", question, "{context}", text))
		if err != nil {
			return res, err
		}
		res.Answer = strings.TrimSpace(out)
		if res.Answer == "" {
			return res, ErrEmpty
		}
		return res, nil
	}
	ev, err := call(fill(gatherTemplate, "{question}", question, "{context}", text))
	if err != nil {
		return res, err
	}
	res.Evidence = strings.TrimSpace(ev)
	out, err := call(fill(checkTemplate, "{question}", question, "{context}", text, "{evidence}", res.Evidence))
	if err != nil {
		return res, err
	}
	res.Check = strings.TrimSpace(out)
	res.Answer = ExtractFinal(out)
	if res.Answer == "" {
		return res, ErrEmpty
	}
	return res, nil
}

// ExtractFinal returns the text after the last FINAL: marker, or the whole
// reply when the model forgot the marker.
func ExtractFinal(s string) string {
	s = strings.TrimSpace(s)
	i := strings.LastIndex(strings.ToUpper(s), "FINAL:")
	if i < 0 {
		return s
	}
	out := strings.TrimSpace(s[i+len("FINAL:"):])
	out = strings.Trim(out, "*` \n")
	return strings.TrimSpace(out)
}
