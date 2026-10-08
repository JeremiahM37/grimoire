package bank

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Consolidation turns facts into observations: it reads the facts no
// consolidation has read yet, a few at a time, shows a model each batch with
// the observations it could bear on, and applies what the model decides —
// create an observation, revise one, or retire one — to observations.md.
//
// A person's observation is never revised or retired by a model. When the
// model asks to, its version is filed beside the person's as a challenge
// (chal=<id>), which recall ranks below the person's and reflect treats as
// disputed. A model observation a person edits becomes theirs the same way a
// fact does: its text no longer matches its sum.
//
// Which facts have been read is operational state (bank_consolidated), keyed
// by the fact's text, so a fact a person edits is read again. Losing that
// table only makes the next run re-read facts it has seen, which yields the
// observations it would have produced anyway.

// ConsolidationResult is what one consolidation did.
type ConsolidationResult struct {
	Status           string   `json:"status"` // completed | no_new_facts | disabled
	FactsProcessed   int      `json:"facts_processed"`
	FactsFailed      int      `json:"facts_failed"`
	Batches          int      `json:"batches"`
	LLMFailures      int      `json:"llm_failures"`
	Created          int      `json:"observations_created"`
	Updated          int      `json:"observations_updated"`
	Deleted          int      `json:"observations_deleted"`
	Merged           int      `json:"observations_merged"`
	Challenges       int      `json:"challenges_recorded"`
	Unresolved       int      `json:"unresolved_actions"`
	RefreshesQueued  []string `json:"mental_model_refreshes_queued"`
	Usage            Usage    `json:"usage"`
	ObservationTotal int      `json:"observations_total"`
}

const (
	defaultConsolidationBatch = 8
	maxFactsPerConsolidation  = 400
	nearDuplicate             = 0.97
)

// consolidationEnabled reads the bank's consolidation setting.
func (e *Engine) consolidationMode(p *Profile) string {
	return p.Setting("consolidation", "auto")
}

// autoConsolidate queues a consolidation after a retain when the bank wants
// one and a model is there to do it.
func (e *Engine) autoConsolidate(bankID string) {
	if !e.AI.Available() {
		return
	}
	p, err := e.Profile(bankID)
	if err != nil || e.consolidationMode(p) != "auto" {
		return
	}
	if _, _, err := e.EnqueueConsolidation(bankID); err != nil {
		log.Printf("banks: queueing consolidation for %s: %v", bankID, err)
	}
}

type ledgerRow struct {
	sum, status string
}

func (e *Engine) ledger(bankID string) (map[string]ledgerRow, error) {
	rows, err := e.Index.DB.Query("SELECT fact, sum, status FROM bank_consolidated WHERE bank=?", bankID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ledgerRow{}
	for rows.Next() {
		var f string
		var r ledgerRow
		if rows.Scan(&f, &r.sum, &r.status) == nil {
			out[f] = r
		}
	}
	return out, rows.Err()
}

// pendingFacts are the facts consolidation has not read in their current
// wording, oldest first.
func (e *Engine) pendingFacts(bankID string) ([]int32, *bankCache, error) {
	c, err := e.cache(bankID)
	if err != nil {
		return nil, nil, err
	}
	led, err := e.ledger(bankID)
	if err != nil {
		return nil, nil, err
	}
	var out []int32
	for i := range c.units {
		u := &c.units[i]
		if u.Type == "observation" {
			continue
		}
		if r, ok := led[u.ID]; ok && r.sum == textSum(u.Text) {
			continue
		}
		out = append(out, int32(i))
	}
	sort.SliceStable(out, func(a, b int) bool {
		ua, ub := &c.units[out[a]], &c.units[out[b]]
		if ua.Mentioned != ub.Mentioned {
			return ua.Mentioned < ub.Mentioned
		}
		return out[a] < out[b]
	})
	return out, c, nil
}

// pendingConsolidation counts facts not yet consolidated.
func (e *Engine) pendingConsolidation(bankID string) int {
	p, _, err := e.pendingFacts(bankID)
	if err != nil {
		return 0
	}
	return len(p)
}

func tagKey(tags []string) string {
	t := append([]string(nil), tags...)
	sort.Strings(t)
	return strings.Join(t, "\x1f")
}

// Consolidate reads the bank's unread facts into observations.
func (e *Engine) Consolidate(ctx context.Context, bankID string) (*ConsolidationResult, error) {
	if !e.AI.Available() {
		return nil, ErrModelRequired
	}
	prof, err := e.Profile(bankID)
	if err != nil {
		return nil, err
	}
	res := &ConsolidationResult{Status: "completed", RefreshesQueued: []string{}}
	if e.consolidationMode(prof) == "off" {
		res.Status = "disabled"
		return res, nil
	}
	pending, c, err := e.pendingFacts(bankID)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		res.Status = "no_new_facts"
		return res, nil
	}
	if len(pending) > maxFactsPerConsolidation {
		pending = pending[:maxFactsPerConsolidation]
	}
	size, _ := strconv.Atoi(prof.Setting("consolidation_batch_size", strconv.Itoa(defaultConsolidationBatch)))
	if size <= 0 {
		size = defaultConsolidationBatch
	}
	// Never mix tag scopes in one call: an observation is scoped by the tags
	// of the facts it came from.
	groups := map[string][]int32{}
	var order []string
	for _, p := range pending {
		k := tagKey(c.units[p].Tags)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], p)
	}
	client := e.AI.WithSurface("bank.consolidate", "")
	cs := &consolidation{e: e, bankID: bankID, prof: prof, cache: c, client: client, res: res}
	for _, k := range order {
		list := groups[k]
		for start := 0; start < len(list); start += size {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			batch := list[start:min(start+size, len(list))]
			if err := cs.batch(ctx, batch); err != nil {
				return res, err
			}
		}
	}
	of, _, _ := e.readObservations(bankID)
	res.ObservationTotal = len(of.Current)
	// Auto-refreshing mental models whose scope changed get a refresh.
	if ids, err := e.staleAutoModels(bankID); err == nil {
		for _, id := range ids {
			if opID, _, err := e.EnqueueRefresh(bankID, id); err == nil {
				res.RefreshesQueued = append(res.RefreshesQueued, opID)
			}
		}
	}
	return res, nil
}

type consolidation struct {
	e      *Engine
	bankID string
	prof   *Profile
	cache  *bankCache
	client *ai.Client
	res    *ConsolidationResult
}

// batch consolidates one batch, halving it on a model failure until a single
// fact fails alone, which is then recorded as failed so it is not retried
// forever.
func (cs *consolidation) batch(ctx context.Context, batch []int32) error {
	cs.res.Batches++
	acts, shown, err := cs.ask(ctx, batch)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cs.res.LLMFailures++
		if len(batch) == 1 {
			cs.res.FactsFailed++
			return cs.mark(batch, "failed")
		}
		mid := len(batch) / 2
		if err := cs.batch(ctx, batch[:mid]); err != nil {
			return err
		}
		return cs.batch(ctx, batch[mid:])
	}
	if err := cs.apply(acts, batch, shown); err != nil {
		return err
	}
	cs.res.FactsProcessed += len(batch)
	return cs.mark(batch, "done")
}

func (cs *consolidation) mark(batch []int32, status string) error {
	db := cs.e.Index.DB
	db.Lock()
	defer db.Unlock()
	return withTx(db.Conn(), func(tx *sql.Tx) error {
		now := cs.e.now().UnixMilli()
		for _, p := range batch {
			u := &cs.cache.units[p]
			if _, err := tx.Exec("INSERT OR REPLACE INTO bank_consolidated(bank,fact,sum,status,at) VALUES(?,?,?,?,?)",
				cs.bankID, u.ID, textSum(u.Text), status, now); err != nil {
				return err
			}
		}
		return nil
	})
}

type obsAction struct {
	ObservationID string     `json:"observation_id"`
	ID            string     `json:"id"`
	Text          string     `json:"text"`
	SourceFactIDs []string   `json:"source_fact_ids"`
	Evidence      []Evidence `json:"evidence"`
	Reason        string     `json:"reason"`
}

func (a obsAction) target() string {
	if a.ObservationID != "" {
		return a.ObservationID
	}
	return a.ID
}

type obsActions struct {
	Creates []obsAction `json:"creates"`
	Updates []obsAction `json:"updates"`
	Deletes []obsAction `json:"deletes"`
}

const consolidationSystem = `You keep a memory bank's observations: durable statements of what is known, each backed by the facts that support it. You are given new facts and the existing observations they may bear on, and you decide how the observations should change.

1. Prefer revising an existing observation over adding a near-copy. When a new fact repeats, confirms or extends what an observation says about the same subject, update that observation and add the fact as a source. One observation with many sources beats many with one each.
2. Related new facts belong together. When several of the new facts are about the same subject (one person's garden, one project, one trip), write ONE observation that covers them and cite every one of those facts, even when there are no existing observations yet. Do not write one observation per fact. Keep separate only what is genuinely a different subject: another person, another project, an unrelated event.
3. Match on the same entity and subject, not merely the same loose topic. Two facts about one person's hobby are one subject; a fact about their job is another.
4. When something changed, revise the observation to say what holds now and what held before, with dates ("Dana led billing until March 2024, then moved to search"). Say only what the facts support.
5. Never compute, extrapolate or adjust numbers. Change a count only when a fact states the new count.
6. Retire an observation only when it is restated elsewhere or says nothing. Keep history by revising instead.
7. Leave out facts with nothing durable in them (greetings, filler, one-off logistics) unless the mission asks for them.
8. Write observations as plain sentences: names instead of pronouns, absolute dates, no ids or metadata in the text.
9. An observation with authority "human" was written by a person. You may still propose revising or retiring it with an update or delete as usual; the change will be filed for the person to review, not applied.
10. Where the mission conflicts with these rules, the mission wins.

Reply with one JSON object and nothing else:
{"creates": [{"text": "...", "source_fact_ids": ["..."], "evidence": [{"fact_id": "...", "quote": "..."}], "reason": "..."}],
 "updates": [{"observation_id": "...", "text": "...", "source_fact_ids": ["..."], "evidence": [{"fact_id": "...", "quote": "..."}], "reason": "..."}],
 "deletes": [{"observation_id": "...", "reason": "..."}]}
Copy ids exactly. At most one update per observation. Every create and update cites every one of the new facts it draws on, and at least one. A quote is a short exact excerpt of the fact it cites. Use empty arrays when nothing should change.`

const defaultObservationsMission = "Track anything durable in the new facts: people and their relationships, roles and preferences; names, numbers and dates; places; decisions, plans and their reasons; events; and patterns that recur."

// ask runs one model call for a batch and returns its actions and the
// observations it was shown.
func (cs *consolidation) ask(ctx context.Context, batch []int32) (*obsActions, map[string]bool, error) {
	c := cs.cache
	tags := c.units[batch[0]].Tags
	shown := map[string]bool{}
	var shownOrder []string
	match := "any"
	if len(tags) > 0 {
		match = "all_strict"
	}
	for _, p := range batch {
		budget := 512
		rec, err := cs.e.Recall(ctx, cs.bankID, RecallRequest{Query: c.units[p].Text, Types: []string{"observation"},
			Budget: "low", MaxTokens: &budget, Tags: tags, TagsMatch: match, NoRerank: true})
		if err != nil {
			if strings.Contains(err.Error(), "at least one word") {
				continue
			}
			return nil, nil, err
		}
		for _, r := range rec.Results {
			if !shown[r.ID] {
				shown[r.ID] = true
				shownOrder = append(shownOrder, r.ID)
			}
		}
	}
	of, _, err := cs.e.readObservations(cs.bankID)
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]Observation{}
	for _, o := range of.Current {
		byID[o.ID] = o
	}
	type srcOut struct {
		Text      string `json:"text"`
		Mentioned string `json:"mentioned_at,omitempty"`
	}
	type obsOut struct {
		ID        string   `json:"id"`
		Text      string   `json:"text"`
		Authority string   `json:"authority"`
		Proof     int      `json:"proof_count"`
		Sources   []srcOut `json:"source_memories,omitempty"`
	}
	existing := []obsOut{}
	for _, id := range shownOrder {
		o, ok := byID[id]
		if !ok {
			continue
		}
		a := "agent"
		if o.IsHuman() {
			a = "human"
		}
		item := obsOut{ID: o.ID, Text: o.Text, Authority: a, Proof: len(o.Sources)}
		for _, sid := range o.Sources[:min(len(o.Sources), 4)] {
			if sp, ok := c.byID[sid]; ok {
				item.Sources = append(item.Sources, srcOut{Text: runeCut(c.units[sp].Text, 300),
					Mentioned: fmtTime(fromMS(c.units[sp].Mentioned))})
			}
		}
		existing = append(existing, item)
	}
	var b strings.Builder
	b.WriteString("## Mission\n\n")
	mission := strings.TrimSpace(cs.prof.Setting("observations_mission", ""))
	if mission == "" {
		mission = defaultObservationsMission
	}
	b.WriteString(mission + "\n\n## New facts\n\n")
	for _, p := range batch {
		u := &c.units[p]
		fmt.Fprintf(&b, "[%s] %s", u.ID, u.Text)
		var when []string
		if u.OccStart != 0 {
			when = append(when, "occurred="+fmtTime(fromMS(u.OccStart)))
			if u.OccEnd != 0 && u.OccEnd != u.OccStart {
				when[len(when)-1] += ".." + fmtTime(fromMS(u.OccEnd))
			}
		}
		if u.Mentioned != 0 {
			when = append(when, "mentioned="+fmtTime(fromMS(u.Mentioned)))
		}
		if u.Human {
			when = append(when, "authority=human")
		}
		if len(when) > 0 {
			b.WriteString(" (" + strings.Join(when, ", ") + ")")
		}
		b.WriteString("\n")
	}
	raw, _ := json.MarshalIndent(existing, "", "  ")
	b.WriteString("\n## Existing observations\n\n" + string(raw) + "\n")
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		comp, err := cs.client.CompleteWith(ctx, b.String(), ai.CompleteOpts{System: consolidationSystem,
			Temperature: ai.Temp(0), MaxTokens: 8000, JSON: true})
		cs.res.Usage.add(comp.Usage.Input, comp.Usage.Output)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			continue
		}
		if comp.Truncated() {
			return nil, nil, fmt.Errorf("consolidation reply truncated")
		}
		var acts obsActions
		if err := ai.DecodeJSON(comp.Text, &acts); err != nil {
			lastErr = err
			continue
		}
		bad := false
		for _, d := range acts.Deletes {
			// A delete with no target is a malformed reply, not a no-op.
			bad = bad || d.target() == ""
		}
		if bad {
			lastErr = fmt.Errorf("a delete names no observation")
			continue
		}
		return &acts, shown, nil
	}
	return nil, nil, fmt.Errorf("consolidation: %w", lastErr)
}

// apply writes a batch's actions into observations.md under the bank lock,
// against the file as it is now — a person may have edited it while the
// model was thinking.
func (cs *consolidation) apply(acts *obsActions, batch []int32, shown map[string]bool) error {
	e, c := cs.e, cs.cache
	lock := e.bankLock(cs.bankID)
	lock.Lock()
	defer lock.Unlock()
	of, old, err := e.readObservations(cs.bankID)
	if err != nil {
		return err
	}
	now := e.now()
	inBatch := map[string]*unit{}
	for _, p := range batch {
		inBatch[c.units[p].ID] = &c.units[p]
	}
	scopeTags := c.units[batch[0]].Tags
	resolve := func(ids []string, ev []Evidence) ([]string, []Evidence) {
		var out []string
		seen := map[string]bool{}
		for _, id := range ids {
			if inBatch[id] != nil && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		var keep []Evidence
		for _, x := range ev {
			if seen[x.FactID] {
				keep = append(keep, x)
			}
		}
		return out, keep
	}
	find := func(id string) int {
		for i := range of.Current {
			if of.Current[i].ID == id {
				return i
			}
		}
		return -1
	}
	widen := func(o *Observation, srcs []string) {
		for _, sid := range srcs {
			u := inBatch[sid]
			if u == nil {
				continue
			}
			if u.OccStart != 0 && (o.OccStart.IsZero() || fromMS(u.OccStart).Before(o.OccStart)) {
				o.OccStart = fromMS(u.OccStart)
			}
			end := u.OccEnd
			if end == 0 {
				end = u.OccStart
			}
			if end != 0 && (o.OccEnd.IsZero() || fromMS(end).After(o.OccEnd)) {
				o.OccEnd = fromMS(end)
			}
			if u.Mentioned != 0 && fromMS(u.Mentioned).After(o.Mentioned) {
				o.Mentioned = fromMS(u.Mentioned)
			}
		}
	}
	addSources := func(o *Observation, srcs []string, ev []Evidence) {
		o.Sources = unionTags(o.Sources, srcs)
		for _, x := range ev {
			dup := false
			for _, y := range o.Evidence {
				if y.FactID == x.FactID {
					dup = true
				}
			}
			if !dup {
				o.Evidence = append(o.Evidence, x)
			}
		}
		o.Tags = unionTags(o.Tags, scopeTags)
		widen(o, srcs)
		o.Updated = now
	}
	retire := func(i int, deleted bool) {
		h := of.Current[i]
		h.Of, h.At, h.Deleted = h.ID, now, deleted
		of.History = append(of.History, h)
		of.Current = append(of.Current[:i:i], of.Current[i+1:]...)
	}
	// challenge files the model's version beside a person's observation,
	// revising an earlier challenge of the same observation if there is one.
	challenge := func(humanID, text string, srcs []string, ev []Evidence) {
		cs.res.Challenges++
		for i := range of.Current {
			o := &of.Current[i]
			if o.Challenges == humanID && !o.IsHuman() {
				if normFactText(o.Text) != normFactText(text) {
					h := *o
					h.Of, h.At = o.ID, now
					of.History = append(of.History, h)
					o.Text = text
				}
				addSources(o, srcs, ev)
				return
			}
		}
		o := Observation{ID: newObsID(cs.bankID, text, now), Text: text, Challenges: humanID}
		addSources(&o, srcs, ev)
		of.Current = append(of.Current, o)
	}
	// twin finds a current observation that says the same thing.
	twin := func(text string, skip string) int {
		norm := memory.Normalize(text)
		for i := range of.Current {
			if of.Current[i].ID != skip && memory.Normalize(of.Current[i].Text) == norm {
				return i
			}
		}
		return e.nearDuplicate(of.Current, text, skip, scopeTags)
	}

	// Deletes.
	for _, d := range acts.Deletes {
		id := d.target()
		if !shown[id] {
			cs.res.Unresolved++
			continue
		}
		i := find(id)
		if i < 0 {
			continue
		}
		if of.Current[i].IsHuman() {
			reason := strings.TrimSpace(d.Reason)
			if reason == "" {
				reason = "Later facts may no longer support this."
			}
			challenge(id, oneLine(reason), nil, nil)
			continue
		}
		retire(i, true)
		cs.res.Deleted++
	}
	// Updates, one per observation (the last one wins, sources unite).
	merged := map[string]*obsAction{}
	var updOrder []string
	for i := range acts.Updates {
		u := acts.Updates[i]
		id := u.target()
		if prev, ok := merged[id]; ok {
			prev.Text = u.Text
			prev.SourceFactIDs = append(prev.SourceFactIDs, u.SourceFactIDs...)
			prev.Evidence = append(prev.Evidence, u.Evidence...)
			continue
		}
		merged[id] = &u
		updOrder = append(updOrder, id)
	}
	updTexts := map[string]bool{}
	for _, id := range updOrder {
		u := merged[id]
		text := oneLine(u.Text)
		srcs, ev := resolve(u.SourceFactIDs, u.Evidence)
		if !shown[id] || text == "" || len(srcs) == 0 {
			cs.res.Unresolved++
			continue
		}
		i := find(id)
		if i < 0 {
			cs.res.Unresolved++
			continue
		}
		updTexts[memory.Normalize(text)] = true
		if of.Current[i].IsHuman() {
			if memory.Normalize(of.Current[i].Text) == memory.Normalize(text) {
				// The model agrees with the person: nothing to file, and the
				// person's text is not touched even to add sources.
				continue
			}
			challenge(id, text, srcs, ev)
			continue
		}
		o := &of.Current[i]
		if normFactText(o.Text) != text {
			h := *o
			h.Of, h.At = o.ID, now
			of.History = append(of.History, h)
			o.Text = text
		}
		addSources(o, srcs, ev)
		cs.res.Updated++
		// Revised into a copy of another observation: fold it into that one.
		if j := e.nearDuplicate(of.Current, o.Text, o.ID, scopeTags); j >= 0 && !of.Current[j].IsHuman() {
			addSources(&of.Current[j], o.Sources, o.Evidence)
			retire(i, true)
			cs.res.Merged++
		}
	}
	// Creates.
	for _, cr := range acts.Creates {
		text := oneLine(cr.Text)
		srcs, ev := resolve(cr.SourceFactIDs, cr.Evidence)
		if text == "" || len(srcs) == 0 {
			cs.res.Unresolved++
			continue
		}
		if updTexts[memory.Normalize(text)] {
			continue // the same response already said this through an update
		}
		if j := twin(text, ""); j >= 0 {
			if !of.Current[j].IsHuman() {
				addSources(&of.Current[j], srcs, ev)
			}
			cs.res.Merged++
			continue
		}
		o := Observation{ID: newObsID(cs.bankID, text, now), Text: text}
		addSources(&o, srcs, ev)
		of.Current = append(of.Current, o)
		cs.res.Created++
	}
	return e.writeObservations(cs.bankID, of, old)
}

func newObsID(bankID, text string, now time.Time) string {
	return "o" + shortHash(bankID+"\x00"+text+"\x00"+now.String()+"\x00"+string(randBytes(4)), 15)
}

// nearDuplicate finds a current observation in the same tag scope whose
// meaning is at least 0.97 cosine to text; -1 for none.
func (e *Engine) nearDuplicate(cur []Observation, text, skip string, tags []string) int {
	if e.Index.Emb == nil {
		return -1
	}
	var idx []int
	texts := []string{text}
	for i, o := range cur {
		if o.ID == skip || o.ID == "" {
			continue
		}
		if len(tags) > 0 && !tagsAllow(o.Tags, tags, "all_strict") || len(tags) == 0 && len(o.Tags) > 0 {
			continue // another scope's observation is not a duplicate
		}
		idx = append(idx, i)
		texts = append(texts, o.Text)
	}
	if len(idx) == 0 {
		return -1
	}
	vecs, err := e.embedTexts(texts)
	if err != nil || len(vecs) != len(texts) || len(vecs[0]) == 0 {
		return -1
	}
	q := append([]float32(nil), vecs[0]...)
	normalize(q)
	best, bestSim := -1, nearDuplicate
	for k, i := range idx {
		v := append([]float32(nil), vecs[k+1]...)
		if len(v) != len(q) {
			continue
		}
		normalize(v)
		if s := float64(dot(q, v)); s >= bestSim {
			best, bestSim = i, s
		}
	}
	return best
}
