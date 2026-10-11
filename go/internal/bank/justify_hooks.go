package bank

// Justification records for derived items, behind the "justify" experiment
// flag (see internal/justify). With the flag off every function here returns
// before touching the store, so the bank behaves as it did.
//
// Two derivation sites record justifications: observations (each consolidation
// batch that supports an observation is one justification, its facts the
// premises) and mental models (the units a refresh was based on). Premises and
// derived items live in one namespace per bank: "u:<bank>:<unit id>" for facts
// and observations, "m:<bank>:<model id>" for models. Premise hashes are the
// hash of the unit's text, so a person's edit to a fact shows as a changed
// premise without anything having to notify the store.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/justify"
)

const justifyFlag = "justify"

// The actions a justify-backed cascade report adds. They describe what the
// store says, next to what the cascade did.
const (
	ActJustifyLost = "justify_lost"
	ActJustifyKept = "justify_kept"
)

func justifyOn() bool { return exp.On(justifyFlag) }

func (e *Engine) justifyStore() (*justify.Store, error) {
	if e.Index == nil || e.Index.DB == nil {
		return nil, errors.New("justify: no index database")
	}
	return justify.New(e.Index.DB)
}

func justifyTextHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func unitNode(bankID, id string) string  { return "u:" + bankID + ":" + id }
func modelNode(bankID, id string) string { return "m:" + bankID + ":" + id }

// unitHashes maps every unit id in the bank to the hash of its text.
func unitHashes(c *bankCache) map[string]string {
	out := make(map[string]string, len(c.units))
	for i := range c.units {
		out[c.units[i].ID] = justifyTextHash(c.units[i].Text)
	}
	return out
}

// justifyObservations records, for each observation the batch touched, every
// set of sources that supported it. notes maps an observation id to those sets.
func (e *Engine) justifyObservations(bankID string, c *bankCache, current []Observation, notes map[string][][]string) error {
	if !justifyOn() || len(notes) == 0 {
		return nil
	}
	st, err := e.justifyStore()
	if err != nil {
		return err
	}
	hashes := unitHashes(c)
	for _, o := range current {
		srcSets := notes[o.ID]
		if len(srcSets) == 0 {
			continue
		}
		var js [][]justify.Premise
		for _, srcs := range srcSets {
			var j []justify.Premise
			for _, s := range srcs {
				j = append(j, justify.Premise{ID: unitNode(bankID, s), Hash: hashes[s]})
			}
			js = append(js, j)
		}
		if err := st.Derive(unitNode(bankID, o.ID), "observation", justifyTextHash(o.Text), js...); err != nil {
			return err
		}
	}
	return nil
}

// justifyModel records the units a refresh of a model was based on. A model
// refreshed from nothing has no justification to record and is left alone.
func (e *Engine) justifyModel(bankID, id, body string, based []string, c *bankCache) error {
	if !justifyOn() || len(based) == 0 {
		return nil
	}
	st, err := e.justifyStore()
	if err != nil {
		return err
	}
	hashes := unitHashes(c)
	var j []justify.Premise
	for _, b := range based {
		j = append(j, justify.Premise{ID: unitNode(bankID, b), Hash: hashes[b]})
	}
	return st.Derive(modelNode(bankID, id), "mental_model", bodySum(body), j)
}

// justifyStale maps each derived item of the bank that the store finds stale
// to its state, keyed by the bare id. Premise hashes are compared with the
// bank's current unit texts.
func (e *Engine) justifyStale(bankID string) (map[string]justify.StaleItem, error) {
	st, err := e.justifyStore()
	if err != nil {
		return nil, err
	}
	c, err := e.cache(bankID)
	if err != nil {
		return nil, err
	}
	hashes := unitHashes(c)
	prefix := unitNode(bankID, "")
	current := func(node string) (string, bool) {
		if !strings.HasPrefix(node, prefix) {
			return "", false
		}
		h, ok := hashes[strings.TrimPrefix(node, prefix)]
		return h, ok
	}
	items, err := st.Stale(current)
	if err != nil {
		return nil, err
	}
	out := make(map[string]justify.StaleItem, len(items))
	for _, it := range items {
		out[bareJustifyID(bankID, it.ID)] = it
	}
	return out, nil
}

// bareJustifyID strips the namespace a node id was stored under.
func bareJustifyID(bankID, node string) string {
	for _, p := range []string{unitNode(bankID, ""), modelNode(bankID, "")} {
		if strings.HasPrefix(node, p) {
			return strings.TrimPrefix(node, p)
		}
	}
	return node
}

// justifyStaleReason is what a listing shows for a stale item: a lost
// justification first, then each premise that no longer matches.
func justifyStaleReason(it justify.StaleItem) string {
	var parts []string
	if !it.Valid {
		parts = append(parts, "justification_lost")
	}
	return strings.Join(append(parts, it.Reasons...), ",")
}

// markObservations sets Stale and StaleReason on listed observations.
func (e *Engine) markObservations(bankID string, outs []ObservationOut) error {
	if !justifyOn() || len(outs) == 0 {
		return nil
	}
	stale, err := e.justifyStale(bankID)
	if err != nil {
		return err
	}
	for i := range outs {
		if it, ok := stale[outs[i].ID]; ok {
			outs[i].Stale = true
			outs[i].StaleReason = justifyStaleReason(it)
		}
	}
	return nil
}

// justifyCascade is the store's view of a forget. It forgets the removed facts
// in the store (or previews that on a dry run) and reports the observations and
// models that lose their last justification. The cascade decides what to do to
// each item; this report is the independent check. An observation or model the
// cascade acts on that the store still holds valid is reported as kept, so a
// disagreement is visible rather than hidden.
func (e *Engine) justifyCascade(bankID string, removed map[string]bool, dry bool, acts *[]CascadeAction) error {
	if len(removed) == 0 {
		return nil
	}
	st, err := e.justifyStore()
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(removed))
	for f := range removed {
		ids = append(ids, unitNode(bankID, f))
	}
	sort.Strings(ids)
	var lost []string
	if dry {
		lost, err = st.Plan(ids...)
	} else {
		lost, err = st.Invalidate(ids...)
	}
	if err != nil {
		return err
	}
	// Only items still in the bank's files are dependents to report: a
	// retracted or deleted item is a cascade action already, not a survivor.
	existing := map[string]string{}
	of, _, err := e.readObservations(bankID)
	if err != nil {
		return err
	}
	for _, o := range of.Current {
		existing[unitNode(bankID, o.ID)] = "observation"
	}
	models, err := e.ListModels(bankID, ModelQuery{})
	if err != nil {
		return err
	}
	for _, m := range models {
		existing[modelNode(bankID, m.ID)] = "mental_model"
	}
	lostSet := map[string]bool{}
	for _, node := range lost {
		kind, ok := existing[node]
		if !ok {
			continue
		}
		lostSet[node] = true
		bare := bareJustifyID(bankID, node)
		path := ObservationsPath(bankID)
		if kind == "mental_model" {
			path = ModelPath(bankID, bare)
		}
		*acts = append(*acts, CascadeAction{Kind: "justification", Ref: bare, Bank: bankID, Path: path, Action: ActJustifyLost})
	}
	for _, a := range *acts {
		if a.Action != ActRetracted && a.Action != ActNeedsRederive {
			continue
		}
		var node string
		switch a.Kind {
		case "observation":
			node = unitNode(bankID, a.Ref)
		case "mental_model":
			node = modelNode(bankID, a.Ref)
		default:
			continue
		}
		if lostSet[node] {
			continue
		}
		if n, ok, err := st.Get(node); err != nil {
			return err
		} else if ok && n.Valid {
			*acts = append(*acts, CascadeAction{Kind: "justification", Ref: a.Ref, Bank: bankID, Path: a.Path, Action: ActJustifyKept})
		}
	}
	return nil
}
