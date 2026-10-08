package bank

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/index"
)

// Reflect answers a question by reasoning over a bank: a model drives a short
// loop of retrieval calls — mental models first, then observations, then raw
// facts — and finishes with an answer it must ground in what it retrieved.
//
// The loop speaks plain JSON rather than a provider's native tool calling,
// so it runs unchanged on Ollama, any OpenAI-compatible server and Claude:
// each turn the model returns one JSON object naming the tool to run, the
// engine runs it, and the next prompt carries every result so far. The order
// of the levels is enforced by the engine, not merely asked for, and the
// final answer's citations are checked against what was actually retrieved.
//
// With no model configured, reflect returns an extractive answer — the facts
// recall finds, verbatim — rather than failing.
//
// Reflect never writes to the bank.

// ReflectRequest is one reflect call.
type ReflectRequest struct {
	Query   string
	Context string
	// Budget sets the loop's depth: low (5 turns, the default), mid (10) or
	// high (20).
	Budget string
	// MaxTokens is the answer's length target (default 4096); a longer
	// answer is rewritten to fit.
	MaxTokens *int
	Tags      []string
	TagsMatch string
	TagGroups []TagGroup
	// FactTypes limits the levels: without "observation" the observation
	// search is off, without world and experience recall is.
	FactTypes []string
	// ApplyAllDirectives applies every directive, ignoring tag scoping.
	ApplyAllDirectives bool
	ExcludeModels      bool
	ExcludeModelIDs    []string
	// ResponseSchema asks for a JSON object of this shape extracted from the
	// answer.
	ResponseSchema map[string]any
	QueryTimestamp *time.Time
	Agent          string
	// RequireModel fails instead of falling back to an extractive answer.
	RequireModel bool
}

// Usage is model tokens spent.
type Usage struct {
	Input  int `json:"input_tokens"`
	Output int `json:"output_tokens"`
	Total  int `json:"total_tokens"`
}

func (u *Usage) add(in, out int) {
	u.Input += in
	u.Output += out
	u.Total = u.Input + u.Output
}

// ModelRef is a mental model an answer drew on.
type ModelRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Text string `json:"text"`
}

// BasedOn is the evidence behind an answer.
type BasedOn struct {
	Memories     []RecallFact `json:"memories"`
	Observations []RecallFact `json:"observations"`
	MentalModels []ModelRef   `json:"mental_models"`
	Directives   []Directive  `json:"directives"`
}

func (b BasedOn) ids() []string {
	out := []string{}
	for _, m := range b.Memories {
		out = append(out, m.ID)
	}
	for _, o := range b.Observations {
		out = append(out, o.ID)
	}
	for _, m := range b.MentalModels {
		out = append(out, "model:"+m.ID)
	}
	return out
}

// ToolCall is one retrieval step in a reflect trace.
type ToolCall struct {
	Tool       string         `json:"tool"`
	Input      map[string]any `json:"input"`
	Output     any            `json:"output,omitempty"`
	DurationMS float64        `json:"duration_ms"`
	Iteration  int            `json:"iteration"`
	// Forced is set when the engine ran this step because the level order
	// required it, whatever the model asked for.
	Forced bool   `json:"forced,omitempty"`
	Error  string `json:"error,omitempty"`
}

// LLMCall is one model call in a reflect trace.
type LLMCall struct {
	Scope      string  `json:"scope"`
	DurationMS float64 `json:"duration_ms"`
	Error      string  `json:"error,omitempty"`
}

// ReflectTrace explains a reflect.
type ReflectTrace struct {
	ToolCalls []ToolCall `json:"tool_calls"`
	LLMCalls  []LLMCall  `json:"llm_calls"`
	// Levels is the retrieval order the engine enforced.
	Levels []string `json:"levels"`
	// Rejected lists citations the model gave that no tool had returned.
	Rejected []string `json:"rejected_citations,omitempty"`
}

// ReflectResponse is reflect's answer.
type ReflectResponse struct {
	Text string `json:"text"`
	// Mode is "llm", or "extractive" when no model was available and the
	// answer is recalled facts quoted as they are.
	Mode                  string        `json:"mode"`
	BasedOn               BasedOn       `json:"based_on"`
	StructuredOutput      any           `json:"structured_output,omitempty"`
	StructuredOutputError string        `json:"structured_output_error,omitempty"`
	Usage                 Usage         `json:"usage"`
	Iterations            int           `json:"iterations"`
	DirectivesChecked     bool          `json:"directives_checked"`
	Trace                 *ReflectTrace `json:"trace"`
}

// reflectIterations maps a budget onto the loop's turn limit.
func reflectIterations(budget string) (int, error) {
	switch budget {
	case "", "low":
		return 5, nil
	case "mid":
		return 10, nil
	case "high":
		return 20, nil
	}
	return 0, invalid("budget must be low, mid or high")
}

// The retrieval levels.
const (
	toolSearchModels = "search_mental_models"
	toolReadModel    = "read_mental_model"
	toolSearchObs    = "search_observations"
	toolRecall       = "recall"
	toolExpand       = "expand"
	toolDone         = "done"
)

// reflectRun is the state of one reflect.
type reflectRun struct {
	e       *Engine
	bankID  string
	req     ReflectRequest
	prof    *Profile
	client  *ai.Client
	cache   *bankCache
	now     time.Time
	maxTok  int
	match   string
	dirs    []Directive
	levels  []string
	enabled map[string]bool

	history []step
	usage   Usage
	trace   *ReflectTrace

	seenMem    map[string]bool
	seenObs    map[string]bool
	seenModels map[string]ModelRef
	memOrder   []string
	obsOrder   []string
	modelOrder []string
}

type step struct {
	tool   string
	args   map[string]any
	result string
}

// Reflect answers a question over a bank.
func (e *Engine) Reflect(ctx context.Context, bankID string, req ReflectRequest) (*ReflectResponse, error) {
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		return nil, invalid("query must not be empty")
	}
	if !ValidID(bankID) {
		return nil, invalid("invalid bank id")
	}
	prof, err := e.Profile(bankID)
	if err != nil {
		return nil, err
	}
	iters, err := reflectIterations(req.Budget)
	if err != nil {
		return nil, err
	}
	if req.ResponseSchema != nil {
		if err := validateResponseSchema(req.ResponseSchema); err != nil {
			return nil, err
		}
	}
	match := req.TagsMatch
	if match == "" {
		match = "any"
	}
	if !tagMatchModes[match] {
		return nil, invalid("tags_match must be any, all, any_strict, all_strict or exact")
	}
	for i := range req.TagGroups {
		if err := req.TagGroups[i].validate(0); err != nil {
			return nil, invalid("tag_groups[%d]: %v", i, err)
		}
	}
	types := map[string]bool{"world": true, "experience": true, "observation": true}
	if req.FactTypes != nil {
		if len(req.FactTypes) == 0 {
			return nil, invalid("fact_types must not be empty")
		}
		types = map[string]bool{}
		for _, t := range req.FactTypes {
			switch t {
			case "world", "experience", "observation":
				types[t] = true
			default:
				return nil, invalid("unknown fact type %q", t)
			}
		}
	}
	maxTok := 4096
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTok = *req.MaxTokens
	}
	now := time.Now().UTC()
	if req.QueryTimestamp != nil {
		now = req.QueryTimestamp.UTC()
	}
	c, err := e.cache(bankID)
	if err != nil {
		return nil, err
	}
	r := &reflectRun{e: e, bankID: bankID, req: req, prof: prof, cache: c, now: now, maxTok: maxTok, match: match,
		client: e.AI.WithSurface("bank.reflect", req.Agent), trace: &ReflectTrace{ToolCalls: []ToolCall{}, LLMCalls: []LLMCall{}},
		seenMem: map[string]bool{}, seenObs: map[string]bool{}, seenModels: map[string]ModelRef{},
		enabled: map[string]bool{}}
	r.dirs = selectDirectives(prof.Directives, req.Tags, match, req.ApplyAllDirectives)

	// Which levels exist for this question.
	hasModels := false
	if !req.ExcludeModels {
		n, _ := e.Index.DB.Count("SELECT COUNT(*) FROM bank_models WHERE bank=? AND body<>''", bankID)
		hasModels = n > len(req.ExcludeModelIDs)
	}
	hasObs := false
	if types["observation"] {
		for i := range c.units {
			if c.units[i].Type == "observation" {
				hasObs = true
				break
			}
		}
	}
	if hasModels {
		r.levels = append(r.levels, toolSearchModels)
		r.enabled[toolSearchModels], r.enabled[toolReadModel] = true, true
	}
	if hasObs {
		r.levels = append(r.levels, toolSearchObs)
		r.enabled[toolSearchObs] = true
	}
	if types["world"] || types["experience"] {
		r.levels = append(r.levels, toolRecall)
		r.enabled[toolRecall], r.enabled[toolExpand] = true, true
	}
	r.enabled[toolDone] = true
	r.trace.Levels = append([]string{}, r.levels...)

	if !e.AI.Available() {
		if req.RequireModel {
			return nil, ErrModelRequired
		}
		return r.extractive(ctx)
	}
	resp, err := r.loop(ctx, iters)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

var tagMatchModes = map[string]bool{"any": true, "all": true, "any_strict": true, "all_strict": true, "exact": true}

// selectDirectives picks the directives a reflect must obey: untagged ones
// always, tagged ones when the reflect's tags reach them, all with
// applyAll. Inactive directives never apply.
func selectDirectives(all []Directive, tags []string, match string, applyAll bool) []Directive {
	out := []Directive{}
	for _, d := range all {
		if d.Inactive {
			continue
		}
		if applyAll || len(d.Tags) == 0 {
			out = append(out, d)
			continue
		}
		if len(tags) > 0 && tagsAllow(d.Tags, tags, "any_strict") {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Priority > out[b].Priority })
	return out
}

// ------------------------------------------------------------ prompts

func directiveLine(d Directive) string {
	if d.Name != "" {
		return "**" + d.Name + "**: " + d.Text
	}
	return d.Text
}

var traitWords = map[string][6]string{
	"skepticism": {"", "You take what the memories say at face value and give sources the benefit of the doubt.",
		"You mostly trust the memories, questioning only plain inconsistencies.", "",
		"You are wary of claims the memories do not corroborate and point out where support is thin.",
		"You scrutinise every claim for reliability and hidden motives, and trust only what the memories corroborate."},
	"literalism": {"", "You read between the lines and infer what people meant beyond their exact words.",
		"You weigh implied meaning alongside what was literally said.", "",
		"You keep close to what was literally said and are careful about reading in intent.",
		"You hold to the exact wording and commitments in the memories and do not read in intent."},
	"empathy": {"", "You stick to facts and data and set feelings aside.",
		"You lead with facts while acknowledging that feelings play a part.", "",
		"You give real weight to people's feelings and circumstances.",
		"You put people's feelings, circumstances and wellbeing at the centre of your answer."},
}

func dispositionText(d Disposition) string {
	var lines []string
	for _, t := range []struct {
		name string
		v    int
	}{{"skepticism", d.Skepticism}, {"literalism", d.Literalism}, {"empathy", d.Empathy}} {
		if t.v != 3 && t.v >= 1 && t.v <= 5 {
			lines = append(lines, "- "+traitWords[t.name][t.v])
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "## Disposition\n" + strings.Join(lines, "\n") + "\n"
}

func (r *reflectRun) directivesBlock() string {
	if len(r.dirs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Rules you must always follow\n")
	for _, d := range r.dirs {
		b.WriteString("- " + directiveLine(d) + "\n")
	}
	b.WriteString("These rules override everything else here. Follow them without commenting on them in the answer.\n")
	return b.String()
}

var toolDocs = map[string]string{
	toolSearchModels: `search_mental_models {"query": string, "max_results": int} — curated standing answers; the best match comes back in full, the rest as snippets`,
	toolReadModel:    `read_mental_model {"ids": [string]} — the full text of mental models by id`,
	toolSearchObs:    `search_observations {"query": string, "max_tokens": int} — knowledge consolidated from many memories, with how many memories support each`,
	toolRecall:       `recall {"query": string, "max_tokens": int} — individual memories (the raw facts everything else is built from)`,
	toolExpand:       `expand {"memory_ids": [string], "depth": "chunk" | "document"} — the source passage or whole document a memory came from`,
	toolDone:         `done {"answer": markdown, "memory_ids": [string], "observation_ids": [string], "model_ids": [string]} — finish with the answer and the ids it rests on`,
}

func (r *reflectRun) systemPrompt() string {
	var b strings.Builder
	b.WriteString("You answer questions from what one memory bank holds, and from nothing else. You work by calling " +
		"retrieval tools, one JSON reply at a time, and you finish by calling done.\n\n")
	b.WriteString(r.directivesBlock())
	if len(r.dirs) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("## Purpose\n")
	if m := strings.TrimSpace(r.prof.Mission); m != "" {
		b.WriteString(m + "\n\n")
	} else {
		b.WriteString("Answer the user's question by reasoning over the memories you retrieve.\n\n")
	}
	b.WriteString("## Searching\n")
	names := map[string]string{toolSearchModels: "mental models — curated standing answers (search_mental_models, read_mental_model). " +
		"Use one only if it actually states the answer, and check is_stale.",
		toolSearchObs: "observations — knowledge consolidated from many memories (search_observations). " +
			"When freshness is not up_to_date, confirm the specifics with recall.",
		toolRecall: "memories — the raw facts everything else is built from (recall, then expand for the source text). " +
			"Before saying the bank does not record something, run recall with the question's key terms."}
	if len(r.levels) > 1 {
		b.WriteString("The bank keeps knowledge at several levels. Search them in this order, and stop descending as soon " +
			"as what you have states the answer — a result that only shares the question's topic does not:\n")
	}
	for i, l := range r.levels {
		fmt.Fprintf(&b, "%d. %s\n", i+1, names[l])
	}
	b.WriteString("Break the question into the people, things and ideas it involves and search for each; do not just " +
		"repeat the question.\n\n")
	b.WriteString("## Weighing what you find\n" +
		"- Use only what the tools returned. Never invent names, numbers, dates or events.\n" +
		"- authority \"human\" marks what a person wrote or corrected. It is authoritative: it overrides any memory a " +
		"model extracted. A result with disputed_by contradicts a person's memory — mention it only as a disputed claim, " +
		"never as the answer.\n" +
		"- When memories about the same thing disagree, the most recently mentioned one is current, unless a person's " +
		"memory says otherwise.\n" +
		"- Infer what plainly follows from the memories. Never produce a value (a number, date, name or status) for " +
		"something no memory covers; say the bank does not record it and give what it does record. Call an estimate an " +
		"estimate.\n\n")
	b.WriteString(dispositionText(r.prof.Disposition))
	b.WriteString("\n## Replying\nReply with exactly one JSON object and nothing else:\n" +
		`{"tool": "<name>", "args": {...}}` + "\nor, to run several searches at once,\n" +
		`{"calls": [{"tool": "<name>", "args": {...}}, ...]}` + "\n\nTools:\n")
	for _, t := range []string{toolSearchModels, toolReadModel, toolSearchObs, toolRecall, toolExpand, toolDone} {
		if r.enabled[t] {
			b.WriteString("- " + toolDocs[t] + "\n")
		}
	}
	b.WriteString("\nThe answer in done is markdown for a reader who cannot see the tool results. Be concise: " +
		"the first sentence answers the question directly, then add only the facts, dates and numbers that matter to it. " +
		"No preamble, no restating the question, no account of how you searched. Never put ids in the answer text.\n")
	if r.req.MaxTokens != nil && *r.req.MaxTokens > 0 {
		fmt.Fprintf(&b, "Keep the answer within about %d tokens.\n", r.maxTok)
	}
	b.WriteString("In the id arrays list only the memories, observations and models whose content you actually used in the " +
		"answer — not everything you retrieved. Never ask follow-up questions or offer further help; the reader cannot reply.\n\n")
	fmt.Fprintf(&b, "## Now\nThe current time is %s UTC.\nMemory bank: %s\n", r.now.Format("2006-01-02 15:04"), r.prof.Name)
	if len(r.dirs) > 0 {
		b.WriteString("\n## Before you answer\nCheck that your answer obeys every rule:\n")
		for i, d := range r.dirs {
			fmt.Fprintf(&b, "%d. %s\n", i+1, directiveLine(d))
		}
	}
	return b.String()
}

// maxTranscriptTokens bounds the tool results carried into each prompt.
const maxTranscriptTokens = 60000

func (r *reflectRun) transcript() string {
	if len(r.history) == 0 {
		return "(none yet)\n"
	}
	// Newest results are kept whole; the oldest give way first when the
	// transcript would outgrow its budget. They stay in the trace.
	parts := make([]string, len(r.history))
	total := 0
	for i := len(r.history) - 1; i >= 0; i-- {
		h := r.history[i]
		args, _ := json.Marshal(h.args)
		block := fmt.Sprintf("### %d. %s %s\n%s\n", i+1, h.tool, args, h.result)
		t := CountTokens(block)
		if total+t > maxTranscriptTokens {
			block = fmt.Sprintf("### %d. %s %s\n[result omitted to fit the prompt]\n", i+1, h.tool, args)
			t = CountTokens(block)
		}
		total += t
		parts[i] = block
	}
	return strings.Join(parts, "\n")
}

func (r *reflectRun) userPrompt(next string) string {
	var b strings.Builder
	b.WriteString("## Question\n" + r.req.Query + "\n\n")
	if c := strings.TrimSpace(r.req.Context); c != "" {
		b.WriteString("## Context\n" + c + "\n\n")
	}
	fmt.Fprintf(&b, "## Length\nAim for a complete answer of about %d tokens at most; finish cleanly rather than "+
		"stopping mid-sentence.\n\n", r.maxTok)
	b.WriteString("## Tool results so far\n" + r.transcript() + "\n")
	b.WriteString("## Next step\n" + next + "\n")
	return b.String()
}

// ------------------------------------------------------------ the loop

type call struct {
	tool string
	args map[string]any
}

func normToolName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "functions.")
	if i := strings.Index(s, "<|"); i >= 0 {
		s = s[:i]
	}
	switch s {
	case "read_mental_models":
		return toolReadModel
	case "search_observation":
		return toolSearchObs
	}
	return s
}

func parseCalls(text string) ([]call, error) {
	var raw map[string]any
	if err := ai.DecodeJSON(text, &raw); err != nil {
		var list []map[string]any
		if ai.DecodeJSON(text, &list) != nil {
			return nil, err
		}
		raw = map[string]any{"calls": toAnySlice(list)}
	}
	one := func(m map[string]any) (call, bool) {
		name := ""
		for _, k := range []string{"tool", "name", "action", "function"} {
			if v, ok := m[k].(string); ok && v != "" {
				name = v
				break
			}
		}
		if name == "" {
			return call{}, false
		}
		var args map[string]any
		for _, k := range []string{"args", "arguments", "parameters", "input"} {
			if v, ok := m[k].(map[string]any); ok {
				args = v
				break
			}
			if s, ok := m[k].(string); ok {
				_ = json.Unmarshal([]byte(s), &args)
				break
			}
		}
		if args == nil {
			args = map[string]any{}
			for k, v := range m {
				if k != "tool" && k != "name" && k != "action" && k != "function" {
					args[k] = v
				}
			}
		}
		return call{tool: normToolName(name), args: args}, true
	}
	var out []call
	if list, ok := raw["calls"].([]any); ok {
		for _, x := range list {
			if m, ok := x.(map[string]any); ok {
				if c, ok := one(m); ok {
					out = append(out, c)
				}
			}
		}
	} else if c, ok := one(raw); ok {
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("reply names no tool")
	}
	if len(out) > 6 {
		out = out[:6]
	}
	return out, nil
}

func toAnySlice(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

func (r *reflectRun) complete(ctx context.Context, scope, system, prompt string, opts ai.CompleteOpts) (string, error) {
	t := time.Now()
	opts.System = system
	comp, err := r.client.CompleteWith(ctx, prompt, opts)
	r.usage.add(comp.Usage.Input, comp.Usage.Output)
	lc := LLMCall{Scope: scope, DurationMS: ms2(time.Since(t))}
	if err != nil {
		lc.Error = err.Error()
	}
	r.trace.LLMCalls = append(r.trace.LLMCalls, lc)
	if err != nil {
		return "", err
	}
	return comp.Text, nil
}

func ms2(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func (r *reflectRun) hasEvidence() bool {
	return len(r.memOrder)+len(r.obsOrder)+len(r.modelOrder) > 0
}

const reflectTemperature = 0.3

func (r *reflectRun) loop(ctx context.Context, iters int) (*ReflectResponse, error) {
	system := r.systemPrompt()
	forced, released := 0, false
	consecutiveErr := 0
	var done *call
	iter := 0
	toolRuns, capped := 0, false
	for ; iter < iters && done == nil; iter++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		last := iter == iters-1 || capped
		var next string
		required := ""
		switch {
		case last:
			next = "Stop searching. Call done now with your answer, built from the tool results above."
		case !released && forced < len(r.levels):
			required = r.levels[forced]
			next = fmt.Sprintf("Call %s next.", required)
		default:
			next = "Call another tool if the results above do not yet state the answer; otherwise call done."
		}
		text, err := r.complete(ctx, "reflect_turn", system, r.userPrompt(next),
			ai.CompleteOpts{JSON: true, Temperature: ai.Temp(reflectTemperature), MaxTokens: 4096})
		if err != nil {
			consecutiveErr++
			if consecutiveErr >= 2 {
				return r.fallback(ctx, err)
			}
			continue
		}
		consecutiveErr = 0
		calls, perr := parseCalls(text)
		if perr != nil {
			if required != "" {
				calls = []call{{tool: required, args: map[string]any{"query": r.req.Query}}}
			} else if last {
				break
			} else {
				r.history = append(r.history, step{tool: "error", args: map[string]any{},
					result: `{"error":"your reply was not a JSON tool call; reply with exactly one JSON object"}`})
				continue
			}
		}
		// The level order is the engine's to keep. A turn that skips the
		// required level gets it run anyway, with the model's query.
		if required != "" {
			has := false
			for _, c := range calls {
				if c.tool == required {
					has = true
				}
			}
			if !has {
				q := r.req.Query
				for _, c := range calls {
					if s, ok := c.args["query"].(string); ok && strings.TrimSpace(s) != "" {
						q = s
						break
					}
				}
				calls = []call{{tool: required, args: map[string]any{"query": q}}}
				r.trace.ToolCalls = append(r.trace.ToolCalls, ToolCall{Tool: required, Input: map[string]any{"query": q},
					Iteration: iter, Forced: true, Output: "level order enforced"})
			}
			forced++
		}
		for _, c := range calls {
			if c.tool != toolDone && toolRuns >= iters {
				// The budget bounds tool calls as well as turns: a reply
				// that fans out many searches at once cannot outrun it.
				r.history = append(r.history, step{tool: c.tool, args: c.args,
					result: `{"error":"search budget used up; call done now with your answer"}`})
				capped = true
				continue
			}
			if c.tool == toolDone {
				if !r.hasEvidence() && !last {
					r.history = append(r.history, step{tool: toolDone, args: map[string]any{},
						result: `{"error":"search before answering: nothing has been retrieved yet"}`})
					continue
				}
				cc := c
				done = &cc
				break
			}
			if !r.enabled[c.tool] {
				r.history = append(r.history, step{tool: c.tool, args: c.args,
					result: fmt.Sprintf(`{"error":"tool %q is not available; use only the tools listed"}`, c.tool)})
				continue
			}
			toolRuns++
			result, fresh := r.run(ctx, c, iter)
			r.history = append(r.history, step{tool: c.tool, args: c.args, result: result})
			if c.tool == toolSearchModels && fresh && r.req.Budget != "high" {
				// Every mental model found is fresh and has an answer: the
				// remaining levels become optional.
				released = true
			}
		}
	}
	if done == nil {
		done = r.closingDone(ctx, system)
	}
	var answer string
	var cited citedIDs
	if done != nil {
		answer, _ = done.args["answer"].(string)
		cited = citedIDs{mem: strList(done.args["memory_ids"]), obs: strList(done.args["observation_ids"]),
			models: strList(done.args["model_ids"])}
	}
	if strings.TrimSpace(answer) == "" {
		var err error
		answer, err = r.synthesize(ctx)
		if err != nil || strings.TrimSpace(answer) == "" {
			return r.fallback(ctx, err)
		}
	}
	return r.finish(ctx, iter, answer, cited)
}

// closingDone asks once more for a done call after the loop ran out.
func (r *reflectRun) closingDone(ctx context.Context, system string) *call {
	if !r.hasEvidence() {
		return nil
	}
	text, err := r.complete(ctx, "closing_done", system,
		r.userPrompt("Stop searching. Call done now with your answer, built from the tool results above."),
		ai.CompleteOpts{JSON: true, Temperature: ai.Temp(reflectTemperature), MaxTokens: 4096})
	if err != nil {
		return nil
	}
	calls, err := parseCalls(text)
	if err != nil {
		return nil
	}
	for _, c := range calls {
		if c.tool == toolDone {
			cc := c
			return &cc
		}
	}
	return nil
}

// synthesize writes the answer from the collected results without tools,
// for a model that would not finish the loop properly.
func (r *reflectRun) synthesize(ctx context.Context) (string, error) {
	if !r.hasEvidence() {
		return "", fmt.Errorf("nothing retrieved")
	}
	system := "You write the final answer to a question from retrieved memories, and from nothing else.\n\n" +
		r.directivesBlock() + "\nOutput only the answer, as markdown, with no commentary about how you produced it and " +
		"no follow-up questions. Memories marked authority \"human\" override conflicting ones."
	text, err := r.complete(ctx, "final", system, r.userPrompt("Write the final answer now as plain markdown (not JSON)."),
		ai.CompleteOpts{Temperature: ai.Temp(reflectTemperature), MaxTokens: min(ai.MaxOutputTokens, r.maxTok*2)})
	return strings.TrimSpace(text), err
}

type citedIDs struct{ mem, obs, models []string }

func strList(v any) []string {
	var out []string
	switch t := v.(type) {
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case []string:
		out = append(out, t...)
	case string:
		for _, s := range strings.Split(t, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// finish validates citations, enforces directives and length, extracts
// structured output and assembles the response.
func (r *reflectRun) finish(ctx context.Context, iters int, answer string, cited citedIDs) (*ReflectResponse, error) {
	resp := &ReflectResponse{Mode: "llm", Iterations: iters, Trace: r.trace}
	answer = strings.TrimSpace(answer)
	if len(r.dirs) > 0 {
		answer = r.checkDirectives(ctx, answer)
		resp.DirectivesChecked = true
	}
	if CountTokens(answer) > r.maxTok {
		answer = r.rewriteToLength(ctx, answer)
	}
	resp.Text = answer
	resp.BasedOn = r.basedOn(cited, answer)
	if r.req.ResponseSchema != nil {
		resp.StructuredOutput, resp.StructuredOutputError = r.structured(ctx, answer)
	}
	resp.Usage = r.usage
	return resp, nil
}

// basedOn keeps the cited ids that were really retrieved. A model that cited
// nothing is taken to rest on what it retrieved and the answer actually draws
// on, not on everything it saw. A person's memories come first.
func (r *reflectRun) basedOn(cited citedIDs, answer string) BasedOn {
	b := BasedOn{Memories: []RecallFact{}, Observations: []RecallFact{}, MentalModels: []ModelRef{}, Directives: r.dirs}
	if b.Directives == nil {
		b.Directives = []Directive{}
	}
	pick := func(ids []string, seen map[string]bool, order []string) []string {
		if len(ids) == 0 {
			return order
		}
		var keep []string
		for _, id := range ids {
			if seen[id] {
				keep = append(keep, id)
			} else {
				r.trace.Rejected = append(r.trace.Rejected, id)
			}
		}
		return keep
	}
	none := len(cited.mem)+len(cited.obs)+len(cited.models) == 0
	memIDs, obsIDs, modelIDs := r.usedBy(answer, r.memOrder), r.usedBy(answer, r.obsOrder), r.modelOrder
	if !none {
		memIDs = pick(cited.mem, r.seenMem, nil)
		// An observation id cited as a memory id (or the reverse) still
		// counts if it was retrieved as the other kind.
		var stray []string
		for _, id := range cited.mem {
			if !r.seenMem[id] && r.seenObs[id] {
				stray = append(stray, id)
			}
		}
		obsIDs = pick(append(cited.obs, stray...), r.seenObs, nil)
		seenM := map[string]bool{}
		for id := range r.seenModels {
			seenM[id] = true
		}
		modelIDs = pick(cited.models, seenM, nil)
		r.trace.Rejected = dedupe(r.trace.Rejected, r.seenObs)
	}
	for _, id := range memIDs {
		if p, ok := r.cache.byID[id]; ok {
			b.Memories = append(b.Memories, r.cache.factOut(r.bankID, p))
		}
	}
	sort.SliceStable(b.Memories, func(i, j int) bool {
		return b.Memories[i].Authority == "human" && b.Memories[j].Authority != "human"
	})
	for _, id := range obsIDs {
		if p, ok := r.cache.byID[id]; ok {
			b.Observations = append(b.Observations, r.cache.factOut(r.bankID, p))
		}
	}
	for _, id := range modelIDs {
		if m, ok := r.seenModels[id]; ok {
			b.MentalModels = append(b.MentalModels, m)
		}
	}
	return b
}

// dedupe drops a rejected id that turned out to be a retrieved observation.
func dedupe(ids []string, ok map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		if ok[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// checkDirectives asks the model whether the answer obeys every directive,
// and takes its corrected answer when it does not.
func (r *reflectRun) checkDirectives(ctx context.Context, answer string) string {
	var rules strings.Builder
	for i, d := range r.dirs {
		fmt.Fprintf(&rules, "%d. %s\n", i+1, directiveLine(d))
	}
	system := "You check an answer against a list of rules it must obey. Reply with one JSON object: " +
		`{"complies": true|false, "violations": [string], "revised_answer": string}` + ". When it complies, " +
		"revised_answer is empty. When it does not, revised_answer is the whole answer corrected to obey every rule, " +
		"changing nothing else and adding no facts."
	prompt := "## Rules\n" + rules.String() + "\n## Answer\n" + answer
	text, err := r.complete(ctx, "directive_check", system, prompt, ai.CompleteOpts{JSON: true, Temperature: ai.Temp(0),
		MaxTokens: min(ai.MaxOutputTokens, r.maxTok*2)})
	if err != nil {
		return answer
	}
	var v struct {
		Complies   *bool    `json:"complies"`
		Violations []string `json:"violations"`
		Revised    string   `json:"revised_answer"`
	}
	if ai.DecodeJSON(text, &v) != nil || v.Complies == nil || *v.Complies {
		return answer
	}
	if s := strings.TrimSpace(v.Revised); s != "" {
		return s
	}
	return answer
}

func (r *reflectRun) rewriteToLength(ctx context.Context, answer string) string {
	system := "Rewrite the text to fit the token budget given. Keep the key facts and the structure; drop the least " +
		"important detail. Reply with the rewritten text only."
	text, err := r.complete(ctx, "length_rewrite", system, fmt.Sprintf("Budget: %d tokens.\n\nText:\n%s", r.maxTok, answer),
		ai.CompleteOpts{Temperature: ai.Temp(0.2), MaxTokens: min(ai.MaxOutputTokens, r.maxTok*2)})
	if err != nil || strings.TrimSpace(text) == "" {
		return answer
	}
	return strings.TrimSpace(text)
}

// validateResponseSchema accepts an object schema with typed properties.
func validateResponseSchema(s map[string]any) error {
	if t, _ := s["type"].(string); t != "object" {
		return invalid("response_schema must be an object schema (type: object)")
	}
	props, ok := s["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return invalid("response_schema needs properties")
	}
	for name, p := range props {
		pm, ok := p.(map[string]any)
		if !ok {
			return invalid("response_schema property %q must be an object", name)
		}
		switch t, _ := pm["type"].(string); t {
		case "string", "number", "integer", "boolean", "array", "object":
		default:
			return invalid("response_schema property %q has unsupported type %q", name, t)
		}
	}
	if req, ok := s["required"].([]any); ok {
		for _, x := range req {
			n, _ := x.(string)
			if _, ok := props[n]; !ok {
				return invalid("response_schema requires %q, which is not a property", n)
			}
		}
	}
	return nil
}

func (r *reflectRun) structured(ctx context.Context, answer string) (any, string) {
	schema, _ := json.MarshalIndent(r.req.ResponseSchema, "", "  ")
	system := "You extract information from a text into a JSON object that matches a schema. Use only what the text " +
		"says. Reply with the JSON object only."
	prompt := "## Schema\n" + string(schema) + "\n\n## Text\n" + answer
	text, err := r.complete(ctx, "structured_output", system, prompt, ai.CompleteOpts{JSON: true, Temperature: ai.Temp(0),
		MaxTokens: min(ai.MaxOutputTokens, max(1024, r.maxTok))})
	if err != nil {
		return nil, "LLMError: " + err.Error()
	}
	var out map[string]any
	if err := ai.DecodeJSON(text, &out); err != nil {
		return nil, "DecodeError: " + err.Error()
	}
	if err := checkAgainstSchema(out, r.req.ResponseSchema); err != nil {
		return nil, "ValidationError: " + err.Error()
	}
	return out, ""
}

// checkAgainstSchema is a shallow check: required properties are present and
// each present property has the declared JSON type.
func checkAgainstSchema(v map[string]any, s map[string]any) error {
	props, _ := s["properties"].(map[string]any)
	if req, ok := s["required"].([]any); ok {
		for _, x := range req {
			n, _ := x.(string)
			if _, ok := v[n]; !ok {
				return fmt.Errorf("missing required property %q", n)
			}
		}
	}
	for name, val := range v {
		pm, _ := props[name].(map[string]any)
		if pm == nil || val == nil {
			continue
		}
		want, _ := pm["type"].(string)
		ok := true
		switch want {
		case "string":
			_, ok = val.(string)
		case "number":
			_, ok = val.(float64)
		case "integer":
			f, isNum := val.(float64)
			ok = isNum && f == math.Trunc(f)
		case "boolean":
			_, ok = val.(bool)
		case "array":
			_, ok = val.([]any)
		case "object":
			_, ok = val.(map[string]any)
		}
		if !ok {
			return fmt.Errorf("property %q is not a %s", name, want)
		}
	}
	return nil
}

// fallback answers extractively when the model failed, unless the caller
// needs a model's answer.
func (r *reflectRun) fallback(ctx context.Context, cause error) (*ReflectResponse, error) {
	if r.req.RequireModel {
		if cause == nil {
			cause = fmt.Errorf("the model produced no answer")
		}
		return nil, fmt.Errorf("reflect: %w", cause)
	}
	return r.extractive(ctx)
}

// extractive is reflect with no model: the facts recall finds, quoted.
func (r *reflectRun) extractive(ctx context.Context) (*ReflectResponse, error) {
	types := []string{}
	for _, l := range r.levels {
		switch l {
		case toolSearchObs:
			types = append(types, "observation")
		case toolRecall:
			for _, t := range []string{"world", "experience"} {
				if r.req.FactTypes == nil || containsStr(r.req.FactTypes, t) {
					types = append(types, t)
				}
			}
		}
	}
	resp := &ReflectResponse{Mode: "extractive", Trace: r.trace, Usage: r.usage,
		BasedOn: BasedOn{Memories: []RecallFact{}, Observations: []RecallFact{}, MentalModels: []ModelRef{}, Directives: r.dirs}}
	if resp.BasedOn.Directives == nil {
		resp.BasedOn.Directives = []Directive{}
	}
	if len(types) == 0 {
		resp.Text = "The bank holds nothing that could answer this."
		return resp, nil
	}
	budget := min(r.maxTok, 1500)
	t := time.Now()
	rec, err := r.e.Recall(ctx, r.bankID, RecallRequest{Query: r.req.Query, Types: types, Budget: "mid", MaxTokens: &budget,
		Tags: r.req.Tags, TagsMatch: r.match, TagGroups: r.req.TagGroups, QueryTimestamp: r.req.QueryTimestamp})
	if err != nil {
		return nil, err
	}
	r.trace.ToolCalls = append(r.trace.ToolCalls, ToolCall{Tool: toolRecall, Input: map[string]any{"query": r.req.Query},
		DurationMS: ms2(time.Since(t)), Output: fmt.Sprintf("%d results", len(rec.Results))})
	if len(rec.Results) == 0 {
		resp.Text = "The bank holds nothing about this."
		return resp, nil
	}
	var b strings.Builder
	b.WriteString("What the bank records about this:\n\n")
	for _, f := range rec.Results {
		if f.DisputedBy != "" {
			continue // a person's fact said otherwise; it is listed instead
		}
		b.WriteString("- " + f.Text)
		if d := factDate(f); d != "" {
			b.WriteString(" (" + d + ")")
		}
		if f.Authority == "human" {
			b.WriteString(" — recorded by a person")
		}
		b.WriteString("\n")
		if f.Type == "observation" {
			resp.BasedOn.Observations = append(resp.BasedOn.Observations, f)
		} else {
			resp.BasedOn.Memories = append(resp.BasedOn.Memories, f)
		}
	}
	resp.Text = strings.TrimSpace(b.String())
	return resp, nil
}

func factDate(f RecallFact) string {
	s := f.OccurredStart
	if s == "" {
		s = f.MentionedAt
	}
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------ tools

func argInt(args map[string]any, key string, def, lo, hi int) int {
	n := def
	switch v := args[key].(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	case string:
		if x, err := atoi(v); err == nil {
			n = x
		}
	}
	return max(lo, min(hi, n))
}

func argStr(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}

// run executes one tool call and returns its result as compact JSON. fresh
// reports, for a mental-model search, that every model found is fresh and
// has content.
func (r *reflectRun) run(ctx context.Context, c call, iter int) (string, bool) {
	t := time.Now()
	var out any
	var err error
	fresh := false
	switch c.tool {
	case toolSearchModels:
		out, fresh, err = r.searchModels(ctx, c.args)
	case toolReadModel:
		out, err = r.readModels(c.args)
	case toolSearchObs:
		out, err = r.searchObservations(ctx, c.args)
	case toolRecall:
		out, err = r.recall(ctx, c.args)
	case toolExpand:
		out, err = r.expand(c.args)
	}
	tc := ToolCall{Tool: c.tool, Input: c.args, Iteration: iter, DurationMS: ms2(time.Since(t)), Output: out}
	if err != nil {
		tc.Error = err.Error()
		out = map[string]any{"error": err.Error()}
		tc.Output = nil
	}
	r.trace.ToolCalls = append(r.trace.ToolCalls, tc)
	raw, _ := json.Marshal(out)
	return string(raw), fresh
}

func (r *reflectRun) query(args map[string]any) string {
	if q := argStr(args, "query"); q != "" {
		return q
	}
	return r.req.Query
}

// memOut is a fact or observation as the model reads it: only what it can use.
type memOut struct {
	ID         string   `json:"id"`
	Text       string   `json:"text"`
	Type       string   `json:"type,omitempty"`
	Authority  string   `json:"authority"`
	DisputedBy string   `json:"disputed_by,omitempty"`
	Occurred   string   `json:"occurred,omitempty"`
	Mentioned  string   `json:"mentioned_at,omitempty"`
	Context    string   `json:"context,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Proof      int      `json:"proof_count,omitempty"`
	Sources    []string `json:"source_fact_ids,omitempty"`
}

func compactTime(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		if t.Hour() == 0 && t.Minute() == 0 {
			return t.Format("2006-01-02")
		}
		return t.Format("2006-01-02 15:04")
	}
	return s
}

func toMemOut(f RecallFact) memOut {
	m := memOut{ID: f.ID, Text: f.Text, Authority: f.Authority, DisputedBy: f.DisputedBy,
		Mentioned: compactTime(f.MentionedAt), Context: runeCut(f.Context, 120), Tags: f.Tags,
		Proof: f.ProofCount, Sources: f.SourceFactIDs}
	if f.Type != "observation" {
		m.Type = f.Type
	}
	if f.OccurredStart != "" {
		m.Occurred = compactTime(f.OccurredStart)
		if f.OccurredEnd != "" && f.OccurredEnd != f.OccurredStart {
			m.Occurred += " .. " + compactTime(f.OccurredEnd)
		}
	}
	if len(m.Tags) == 0 {
		m.Tags = nil
	}
	return m
}

func (r *reflectRun) recallReq(q string, types []string, maxTok int) RecallRequest {
	return RecallRequest{Query: q, Types: types, Budget: "mid", MaxTokens: &maxTok, Tags: r.req.Tags, TagsMatch: r.match,
		TagGroups: r.req.TagGroups, QueryTimestamp: r.req.QueryTimestamp}
}

func (r *reflectRun) recall(ctx context.Context, args map[string]any) (any, error) {
	var types []string
	for _, t := range []string{"world", "experience"} {
		if r.req.FactTypes == nil || containsStr(r.req.FactTypes, t) {
			types = append(types, t)
		}
	}
	maxTok := argInt(args, "max_tokens", 2048, 500, 8000)
	rec, err := r.e.Recall(ctx, r.bankID, r.recallReq(r.query(args), types, maxTok))
	if err != nil {
		return nil, err
	}
	mems := []memOut{}
	for _, f := range rec.Results {
		mems = append(mems, toMemOut(f))
		if !r.seenMem[f.ID] {
			r.seenMem[f.ID] = true
			r.memOrder = append(r.memOrder, f.ID)
		}
	}
	return map[string]any{"query": r.query(args), "memories": mems}, nil
}

func (r *reflectRun) searchObservations(ctx context.Context, args map[string]any) (any, error) {
	maxTok := argInt(args, "max_tokens", 5000, 500, 12000)
	req := r.recallReq(r.query(args), []string{"observation"}, maxTok)
	req.SourceFacts, req.SourceFactsMaxTokens, req.SourceFactsPerObs = true, 3000, 300
	rec, err := r.e.Recall(ctx, r.bankID, req)
	if err != nil {
		return nil, err
	}
	obs := []memOut{}
	for _, f := range rec.Results {
		obs = append(obs, toMemOut(f))
		if !r.seenObs[f.ID] {
			r.seenObs[f.ID] = true
			r.obsOrder = append(r.obsOrder, f.ID)
		}
	}
	src := map[string]memOut{}
	for id, f := range rec.SourceFacts {
		src[id] = toMemOut(f)
		if !r.seenMem[id] {
			r.seenMem[id] = true
			r.memOrder = append(r.memOrder, id)
		}
	}
	pending := r.e.pendingConsolidation(r.bankID)
	fresh := "up_to_date"
	switch {
	case pending >= 10:
		fresh = "stale"
	case pending > 0:
		fresh = "slightly_stale"
	}
	out := map[string]any{"query": r.query(args), "observations": obs, "freshness": fresh}
	if len(src) > 0 {
		out["source_facts"] = src
	}
	if pending > 0 {
		out["memories_not_yet_consolidated"] = pending
	}
	return out, nil
}

type modelHit struct {
	ID, Name, Question, Body string
	Tags                     []string
	score                    float64
}

// searchModelHits ranks a bank's mental models against a query by meaning
// and by shared words, fused by rank.
func (e *Engine) searchModelHits(bankID, q string, tags []string, match string, exclude []string, limit int) ([]modelHit, error) {
	rows, err := e.Index.DB.Query("SELECT id,name,question,tags,body,embedding FROM bank_models WHERE bank=? ORDER BY id", bankID)
	if err != nil {
		return nil, err
	}
	var all []modelHit
	var vecs [][]float32
	for rows.Next() {
		var h modelHit
		var tg string
		var blob []byte
		if err := rows.Scan(&h.ID, &h.Name, &h.Question, &tg, &h.Body, &blob); err != nil {
			rows.Close()
			return nil, err
		}
		h.Tags = splitList(tg)
		if containsStr(exclude, h.ID) || !tagsAllow(h.Tags, tags, match) {
			continue
		}
		all = append(all, h)
		v := index.Unpack(blob)
		normalize(v)
		vecs = append(vecs, v)
	}
	rows.Close()
	if len(all) == 0 {
		return nil, nil
	}
	qv := e.queryVector(q)
	terms := keywordTerms(q)
	type rk struct {
		i int
		s float64
	}
	var sem, kw []rk
	for i, h := range all {
		if qv != nil && len(vecs[i]) == len(qv) {
			sem = append(sem, rk{i, float64(dot(qv, vecs[i]))})
		}
		hay := strings.ToLower(h.Name + " " + h.Question + " " + h.Body)
		n := 0
		for _, t := range terms {
			if strings.Contains(hay, t) {
				n++
			}
		}
		if n > 0 {
			kw = append(kw, rk{i, float64(n)})
		}
	}
	for _, list := range [][]rk{sem, kw} {
		sort.SliceStable(list, func(a, b int) bool { return list[a].s > list[b].s })
		for rank, x := range list {
			all[x.i].score += 1 / float64(rrfK+rank+1)
		}
	}
	var hits []modelHit
	for _, h := range all {
		if h.score > 0 {
			hits = append(hits, h)
		}
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].score != hits[b].score {
			return hits[a].score > hits[b].score
		}
		return hits[a].ID < hits[b].ID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func (r *reflectRun) modelMatch() string { return r.match }

func (r *reflectRun) searchModels(_ context.Context, args map[string]any) (any, bool, error) {
	limit := argInt(args, "max_results", 5, 1, 20)
	hits, err := r.e.searchModelHits(r.bankID, r.query(args), r.req.Tags, r.modelMatch(), r.req.ExcludeModelIDs, limit)
	if err != nil {
		return nil, false, err
	}
	out := []map[string]any{}
	fresh := len(hits) > 0
	for i, h := range hits {
		m, _, err := r.e.readModel(r.bankID, h.ID)
		if err != nil {
			continue
		}
		r.e.markStale(r.cache, m)
		item := map[string]any{"id": h.ID, "name": h.Name, "question": h.Question, "is_stale": m.IsStale,
			"authority": m.Authority}
		if m.IsStale {
			item["stale_reason"] = m.StaleReason
			fresh = false
		}
		if strings.TrimSpace(m.Body) == "" {
			fresh = false
		}
		if i == 0 && CountTokens(m.Body) <= 4000 {
			item["content"] = m.Body
		} else {
			item["snippet"] = runeCut(m.Body, 280)
			item["content_chars"] = len([]rune(m.Body))
		}
		out = append(out, item)
		if _, ok := r.seenModels[h.ID]; !ok {
			r.modelOrder = append(r.modelOrder, h.ID)
		}
		r.seenModels[h.ID] = ModelRef{ID: h.ID, Name: h.Name, Text: m.Body}
	}
	return map[string]any{"query": r.query(args), "mental_models": out}, fresh, nil
}

func (r *reflectRun) readModels(args map[string]any) (any, error) {
	ids := strList(args["ids"])
	if len(ids) == 0 {
		ids = strList(args["mental_model_ids"])
	}
	if id := argStr(args, "id"); id != "" {
		ids = append(ids, id)
	}
	budget := 6000
	used := 0
	out := []map[string]any{}
	var missing, notRead []string
	for _, id := range ids {
		if containsStr(r.req.ExcludeModelIDs, id) {
			missing = append(missing, id)
			continue
		}
		m, _, err := r.e.readModel(r.bankID, id)
		if err != nil || !tagsAllow(m.Tags, r.req.Tags, r.modelMatch()) {
			missing = append(missing, id)
			continue
		}
		t := CountTokens(m.Body)
		if len(out) > 0 && used+t > budget {
			notRead = append(notRead, id)
			continue
		}
		used += t
		r.e.markStale(r.cache, m)
		out = append(out, map[string]any{"id": m.ID, "name": m.Name, "content": m.Body, "is_stale": m.IsStale,
			"authority": m.Authority})
		if _, ok := r.seenModels[m.ID]; !ok {
			r.modelOrder = append(r.modelOrder, m.ID)
		}
		r.seenModels[m.ID] = ModelRef{ID: m.ID, Name: m.Name, Text: m.Body}
	}
	res := map[string]any{"mental_models": out}
	if len(missing) > 0 {
		res["not_found"] = missing
	}
	if len(notRead) > 0 {
		res["not_read_budget"] = notRead
	}
	return res, nil
}

func (r *reflectRun) expand(args map[string]any) (any, error) {
	ids := strList(args["memory_ids"])
	if id := argStr(args, "memory_id"); id != "" {
		ids = append(ids, id)
	}
	depth := argStr(args, "depth")
	if depth != "document" {
		depth = "chunk"
	}
	var items []map[string]any
	for _, id := range ids[:min(len(ids), 10)] {
		item := map[string]any{"memory_id": id}
		p, ok := r.cache.byID[id]
		if !ok || !r.seenMem[id] && !r.seenObs[id] {
			item["error"] = "not a retrieved memory id"
			items = append(items, item)
			continue
		}
		u := &r.cache.units[p]
		if u.Doc == "" {
			item["error"] = "this memory has no source document"
			items = append(items, item)
			continue
		}
		if depth == "chunk" && u.Chunk >= 0 {
			if ch, err := r.e.GetChunk(r.bankID, ChunkID(r.bankID, u.Doc, u.Chunk)); err == nil {
				item["chunk"] = map[string]any{"text": renderForModel(ch.Text), "document_id": u.Doc}
			}
		} else if n, err := r.e.Vault.Read(DocumentPath(r.bankID, u.Doc)); err == nil {
			d := ParseDocument(n.Frontmatter, n.Body, u.Doc)
			if !tagsAllow(d.Tags, r.req.Tags, r.match) {
				item["error"] = "the source document is outside the requested tags"
			} else {
				item["document"] = map[string]any{"id": d.ID, "text": truncateTokens(renderForModel(d.Content), 6000)}
			}
		}
		items = append(items, item)
	}
	return map[string]any{"items": items}, nil
}

// usedBy keeps the retrieved ids whose text the answer draws on: at least half
// of a memory's distinctive words (four letters or more) appear in it. It is
// what stands in for a citation list the model did not give.
func (r *reflectRun) usedBy(answer string, ids []string) []string {
	have := map[string]bool{}
	for _, w := range words(answer) {
		have[w] = true
	}
	var out []string
	for _, id := range ids {
		p, ok := r.cache.byID[id]
		if !ok {
			continue
		}
		total, hit := 0, 0
		for _, w := range words(r.cache.units[p].Text) {
			if len([]rune(w)) < 4 {
				continue
			}
			total++
			if have[w] {
				hit++
			}
		}
		if total > 0 && hit*2 >= total {
			out = append(out, id)
		}
	}
	return out
}
