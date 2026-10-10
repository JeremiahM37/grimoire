package bank

import (
	"sort"
	"strings"
)

// Cascade forgetting inside a memory bank.
//
// A bank derives its knowledge in layers: facts are extracted from documents,
// observations are consolidated from facts, mental models are answers drawn
// from observations and facts, and proposals are answers a refresh could not
// write over a person's text. Forgetting one piece of information has to reach
// every layer that carries it, so CascadeForget walks the layers in that order
// and applies one rule at each:
//
//   - a fact that carries the forgotten text is removed (a human fact is kept
//     when the forget was agent-initiated, and reported);
//   - an observation whose text carries it, or whose evidence was removed, is
//     retracted when nothing else supports it, and otherwise re-derived: it is
//     removed and its surviving facts are reset in the consolidation ledger so
//     the next consolidation rebuilds it from what is left. An observation is a
//     paraphrase, so redacting its words would not make it true;
//   - a mental model that cites a removed item, or whose answer carries the
//     text, is deleted when every basis is gone, and otherwise has its body
//     cleared and is marked needs_rederive, so the next refresh writes it again;
//   - a proposal that carries the text or cites a removed item is dropped.
//
// A person's own writing is never altered by an agent-initiated forget. A
// human observation or model that would have changed gets a challenge (an
// observation that disputes it, or a proposal over its body) and the original
// is left in place for the person to decide.
//
// Source documents are deliberately not edited: they are what the person
// retained, not derived state. CascadeForget reports them as residual through
// the caller's verification instead.

// Carrier reports whether a text carries the information being forgotten.
type Carrier func(text string) bool

// Redactor returns text with the forgotten span removed.
type Redactor func(text string) string

// CascadeOptions controls one cascade.
type CascadeOptions struct {
	// Agent is true when an agent initiated the forget. Human-written items
	// are then challenged or kept, never altered.
	Agent bool
	// DryRun computes the plan and writes nothing.
	DryRun bool
}

// CascadeAction is one artefact the cascade touched or decided to leave.
type CascadeAction struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref,omitempty"`
	Bank   string `json:"bank"`
	Path   string `json:"path,omitempty"`
	Action string `json:"action"`
}

// The action names a cascade reports.
const (
	ActRemoved       = "removed"
	ActRetracted     = "retracted"
	ActNeedsRederive = "needs_rederive"
	ActRedacted      = "redacted"
	ActChallenged    = "challenged"
	ActProposed      = "proposed"
	ActKeptHuman     = "kept_human"
	ActHistoryPurged = "history_purged"
)

// CascadeForget applies the cascade to one bank. It holds the bank lock for
// the whole pass, so the layers are read and written as one state.
func (e *Engine) CascadeForget(bankID string, carries Carrier, redact Redactor, o CascadeOptions) ([]CascadeAction, error) {
	if !ValidID(bankID) {
		return nil, invalid("invalid bank id")
	}
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()

	var acts []CascadeAction
	add := func(kind, ref, path, action string) {
		acts = append(acts, CascadeAction{Kind: kind, Ref: ref, Bank: bankID, Path: path, Action: action})
	}
	rels, err := e.Vault.WalkDir(Prefix(bankID))
	if err != nil {
		return nil, err
	}
	sort.Strings(rels)

	// Layer 1: facts.
	removedFacts := map[string]bool{}
	for _, rel := range rels {
		if id, kind := ParsePath(rel); id != bankID || kind != FactsKind {
			continue
		}
		n, err := e.Vault.Read(rel)
		if err != nil {
			continue
		}
		doc := strings.TrimSpace(n.Frontmatter.StringVal("document_id"))
		if doc == "" {
			doc = stemOf(rel)
		}
		ff := ParseFacts(n.Body, bankID, doc)
		var keep []Fact
		changed := false
		for _, f := range ff.Facts {
			switch {
			case !carries(f.Text):
				keep = append(keep, f)
			case f.IsHuman() && o.Agent:
				keep = append(keep, f)
				add("fact", f.ID, rel, ActKeptHuman)
			default:
				removedFacts[f.ID] = true
				changed = true
				add("fact", f.ID, rel, ActRemoved)
			}
		}
		if changed && !o.DryRun {
			ff.Facts = keep
			if err := e.rewriteFacts(bankID, doc, rel, n.Body, ff); err != nil {
				return acts, err
			}
		}
	}
	if len(removedFacts) > 0 && !o.DryRun {
		for f := range removedFacts {
			if err := e.Index.DB.Exec("DELETE FROM bank_consolidated WHERE bank=? AND fact=?", bankID, f); err != nil {
				return acts, err
			}
		}
	}

	// Layer 2: observations.
	removedObs := map[string]bool{}
	reset := map[string]bool{}
	of, old, err := e.readObservations(bankID)
	if err != nil {
		return acts, err
	}
	obsPath := ObservationsPath(bankID)
	obsChanged := false
	// A challenge already filed against a human observation is not filed again,
	// so a repeated forget does not stack up disputes.
	pending := map[string]bool{}
	for _, ob := range of.Current {
		if ob.Challenges != "" {
			pending[ob.Challenges] = true
		}
	}
	var current []Observation
	var challenges []Observation
	for _, ob := range of.Current {
		srcGone, srcLeft := 0, 0
		var surviving []string
		for _, s := range ob.Sources {
			if removedFacts[s] {
				srcGone++
			} else {
				srcLeft++
				surviving = append(surviving, s)
			}
		}
		textHit := carries(ob.Text)
		var ev []Evidence
		evChanged := false
		for _, x := range ob.Evidence {
			if removedFacts[x.FactID] {
				evChanged = true
				continue
			}
			if x.Quote != "" && carries(x.Quote) {
				x.Quote = ""
				evChanged = true
			}
			ev = append(ev, x)
		}
		if !textHit && srcGone == 0 && !evChanged {
			current = append(current, ob)
			continue
		}
		if ob.IsHuman() && o.Agent {
			switch {
			case textHit && pending[ob.ID]:
				add("observation", ob.ID, obsPath, "challenge_pending")
			case textHit:
				c := ob
				c.ID = newObsID(bankID, ob.Text, e.now())
				c.Text = redact(ob.Text)
				c.Challenges = ob.ID
				c.Human, c.HandEdited = false, false
				c.Sources, c.Evidence = surviving, ev
				challenges = append(challenges, c)
				obsChanged = true
				add("observation", ob.ID, obsPath, ActChallenged)
			default:
				add("observation", ob.ID, obsPath, ActKeptHuman)
			}
			current = append(current, ob)
			continue
		}
		obsChanged = true
		switch {
		case srcGone == 0 && !textHit:
			// Only its evidence quotes were touched: keep the observation with
			// the cited removals applied.
			ob.Evidence = ev
			current = append(current, ob)
			add("observation", ob.ID, obsPath, ActRedacted)
		case srcLeft == 0:
			removedObs[ob.ID] = true
			add("observation", ob.ID, obsPath, ActRetracted)
		default:
			// Partly built from what is forgotten: rebuild it from what remains.
			removedObs[ob.ID] = true
			for _, s := range surviving {
				reset[s] = true
			}
			add("observation", ob.ID, obsPath, ActNeedsRederive)
		}
	}
	var history []Observation
	for _, h := range of.History {
		if carries(h.Text) {
			obsChanged = true
			add("observation_history", h.ID, obsPath, ActHistoryPurged)
			continue
		}
		history = append(history, h)
	}
	if obsChanged && !o.DryRun {
		of.Current = append(current, challenges...)
		of.History = history
		if err := e.writeObservations(bankID, of, old); err != nil {
			return acts, err
		}
		for s := range reset {
			if err := e.Index.DB.Exec("DELETE FROM bank_consolidated WHERE bank=? AND fact=?", bankID, s); err != nil {
				return acts, err
			}
		}
	}

	// Layer 3: mental models and proposals.
	for _, rel := range rels {
		id, kind := ParsePath(rel)
		if id != bankID || (kind != ModelKind && kind != ProposalKind) {
			continue
		}
		if err := e.cascadeModelFile(bankID, rel, kind, carries, redact, removedFacts, removedObs, o, add); err != nil {
			return acts, err
		}
	}
	return acts, nil
}

// cascadeModelFile applies the model/proposal rules to one file.
func (e *Engine) cascadeModelFile(bankID, rel string, kind PathKind, carries Carrier, redact Redactor,
	removedFacts, removedObs map[string]bool, o CascadeOptions, add func(kind, ref, path, action string)) error {

	n, err := e.Vault.Read(rel)
	if err != nil {
		return nil
	}
	if kind == ProposalKind {
		id := strings.TrimSuffix(strings.TrimPrefix(rel, Prefix(bankID)+"proposals/"), ".md")
		based := listOf(n.Frontmatter, "based_on")
		if !carries(n.Body) && !anyIn(based, removedFacts, removedObs) {
			return nil
		}
		if !o.DryRun {
			if err := e.dropProposal(bankID, id); err != nil {
				return err
			}
		}
		add("proposal", id, rel, ActRemoved)
		return nil
	}

	m := parseModel(bankID, rel, n.Frontmatter, n.Body)
	if !ValidModelID(m.ID) {
		return nil
	}
	basedGone := 0
	var basedLeft []string
	for _, b := range m.BasedOn {
		if removedFacts[b] || removedObs[b] {
			basedGone++
		} else {
			basedLeft = append(basedLeft, b)
		}
	}
	bodyHit := carries(m.Body)
	if !bodyHit && basedGone == 0 {
		return nil
	}
	if m.Authority == "human" && o.Agent {
		if bodyHit {
			if !o.DryRun {
				if err := e.writeProposal(bankID, m.ID, m, redact(m.Body), basedLeft, m.scopeSig, m.seen,
					e.now().Format("2006-01-02 15:04")); err != nil {
					return err
				}
			}
			add("model", m.ID, rel, ActProposed)
		} else {
			add("model", m.ID, rel, ActKeptHuman)
		}
		return nil
	}
	if len(basedLeft) == 0 {
		if !o.DryRun {
			if err := e.deleteModelLocked(bankID, m.ID); err != nil {
				return err
			}
		}
		add("model", m.ID, rel, ActRemoved)
		return nil
	}
	// Mixed or partly derived: clear the answer and mark it for a refresh.
	if !o.DryRun {
		if e.History != nil {
			e.History.Snapshot(rel, n.Body)
		}
		fm := n.Frontmatter.Clone()
		fm.Delete("scope_sig")
		fm.Delete("body_sum")
		fm.Delete("seen")
		fm.Delete("section_sums")
		fm.Set("needs_rederive", "true")
		fm.Set("based_on", listValue(basedLeft))
		if _, err := e.Vault.Write(rel, "", fm); err != nil {
			return err
		}
		if _, err := e.Index.Upsert(rel); err != nil {
			return err
		}
	}
	add("model", m.ID, rel, ActNeedsRederive)
	return nil
}

func anyIn(ids []string, sets ...map[string]bool) bool {
	for _, id := range ids {
		for _, s := range sets {
			if s[id] {
				return true
			}
		}
	}
	return false
}

// deleteModelLocked is DeleteModel for a caller that already holds the bank lock.
func (e *Engine) deleteModelLocked(bankID, id string) error {
	_, n, err := e.readModel(bankID, id)
	if err != nil {
		return err
	}
	if e.History != nil {
		e.History.Snapshot(n.Path, n.Body)
	}
	if err := e.Vault.Delete(n.Path); err != nil {
		return err
	}
	if err := e.Index.Remove(n.Path); err != nil {
		return err
	}
	return e.dropProposal(bankID, id)
}
