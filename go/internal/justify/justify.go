// Package justify records why a derived item exists and keeps that record
// honest when the things it was derived from change.
//
// A derived item (a bank observation, a mental model's answer) is supported by
// one or more justifications. A justification is a set of premises, each named
// by id and the content hash it had when the item was derived from it. This is
// a truth maintenance system in the sense of Doyle's justification-based TMS:
// an item is valid while it is present and at least one justification is
// valid, and a justification is valid while every premise is valid and still
// has the hash it was derived from.
//
// Three events change validity, and each one propagates along the recorded
// dependencies and nowhere else:
//
//   - Invalidate: a premise is forgotten. Items that lose their last valid
//     justification become invalid, transitively. Items with another valid
//     justification survive.
//   - Recompute: a premise or item has new content. If the hash is unchanged
//     nothing happens (early cutoff): its dependents are not touched. If it
//     changed, the justifications that recorded the old hash stop being valid.
//   - Derive: an item gains justifications, which can also make it valid again.
//
// Validity is recomputed by a worklist over the dependency edges. For
// acyclic dependencies that yields the same answer as a from-scratch least
// fixpoint, which is what the property test checks. Derive refuses a
// justification that would make the graph cyclic, so that holds by
// construction.
//
// Nothing here deletes an item. An invalid item keeps its row and a reason, so
// a caller can mark it stale and show why, and decide what to do with it.
//
// The store is a pair of tables in the index database. Operations run in one
// transaction under the database write lock. Preview forms run the same
// propagation and roll back, so a cascade can report its dependent set before
// it writes anything.
package justify

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

// KindBase marks a premise that is not itself derived: a fact, a document.
// A base item is valid exactly while it is present.
const KindBase = "base"

// ErrCycle is returned by Derive when a justification would make an item
// depend on itself.
var ErrCycle = errors.New("justify: justification would be circular")

// Premise is one input of a justification: the premise's id and the content
// hash it had when the justification was recorded.
type Premise struct {
	ID   string
	Hash string
}

// Node is the stored state of one item.
type Node struct {
	ID      string
	Kind    string
	Hash    string
	Present bool
	Valid   bool
	// Reason says why an invalid derived item is invalid: the first failing
	// premise of its first justification, or "forgotten".
	Reason string
}

// StaleItem is a derived item whose recorded premises no longer match the
// world, or which is no longer valid. Reasons lists every mismatch found.
type StaleItem struct {
	ID      string
	Valid   bool
	Reasons []string
}

// Store is the justification store. Create one with New.
type Store struct {
	d *db.DB
	// Evals counts node re-evaluations in the last operation. It is how the
	// tests see early cutoff, and how the lab reports propagation cost.
	Evals int
}

const schema = `
CREATE TABLE IF NOT EXISTS justify_node (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	hash TEXT NOT NULL,
	present INTEGER NOT NULL DEFAULT 1,
	valid INTEGER NOT NULL DEFAULT 1,
	reason TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS justify_just (
	derived TEXT NOT NULL,
	jid INTEGER NOT NULL,
	premise TEXT NOT NULL,
	premise_hash TEXT NOT NULL,
	PRIMARY KEY (derived, jid, premise)
);
CREATE INDEX IF NOT EXISTS justify_just_premise ON justify_just (premise);
`

// New creates the justification tables if they are missing and returns the
// store.
func New(d *db.DB) (*Store, error) {
	d.Lock()
	defer d.Unlock()
	if _, err := d.Conn().Exec(schema); err != nil {
		return nil, fmt.Errorf("justify schema: %w", err)
	}
	return &Store{d: d}, nil
}

// Derive records justifications for a derived item. The item is created if it
// does not exist; if it does and its hash differs, the change propagates as
// Recompute does. Premises that are not yet known are registered as base items
// with the hash given. Each justification must name at least one premise, and
// one identical to an existing justification is not recorded twice.
func (s *Store) Derive(id, kind, hash string, justs ...[]Premise) error {
	if id == "" || kind == "" || kind == KindBase {
		return errors.New("justify: derived item needs an id and a non-base kind")
	}
	if len(justs) == 0 {
		return errors.New("justify: derived item needs a justification")
	}
	for _, j := range justs {
		if len(j) == 0 {
			return errors.New("justify: justification has no premises")
		}
	}
	_, err := s.run(false, func(o *op) error {
		for _, j := range justs {
			for _, p := range j {
				if p.ID == id {
					return ErrCycle
				}
				reach, err := o.reaches(p.ID, id)
				if err != nil {
					return err
				}
				if reach {
					return ErrCycle
				}
			}
		}
		row, ok, err := o.node(id)
		if err != nil {
			return err
		}
		seeds := []string{id}
		if !ok {
			if err := o.insert(id, kind, hash, false); err != nil {
				return err
			}
		} else {
			if row.kind == KindBase && kind != KindBase {
				if err := o.exec("UPDATE justify_node SET kind=? WHERE id=?", kind, id); err != nil {
					return err
				}
			}
			if row.hash != hash {
				if err := o.exec("UPDATE justify_node SET hash=? WHERE id=?", hash, id); err != nil {
					return err
				}
				owners, err := o.owners(id)
				if err != nil {
					return err
				}
				seeds = append(seeds, owners...)
			}
		}
		for _, j := range justs {
			for _, p := range j {
				_, known, err := o.node(p.ID)
				if err != nil {
					return err
				}
				if !known {
					if err := o.insert(p.ID, KindBase, p.Hash, true); err != nil {
						return err
					}
				}
			}
			if _, err := o.addJust(id, j); err != nil {
				return err
			}
		}
		return o.propagate(seeds)
	})
	return err
}

// Invalidate forgets the given premises and returns the derived items that
// lose their last valid justification, transitively, sorted by id. Items that
// keep another valid justification are not returned. The forgotten ids are
// never returned.
func (s *Store) Invalidate(ids ...string) ([]string, error) {
	return s.run(false, func(o *op) error { return o.forget(ids) })
}

// Plan is Invalidate without the write: it reports the dependent set the
// store would produce, and leaves the store as it was.
func (s *Store) Plan(ids ...string) ([]string, error) {
	return s.run(true, func(o *op) error { return o.forget(ids) })
}

// Recompute records the content hash an item now has. If it is unchanged
// nothing else is touched (early cutoff). If it changed, the justifications
// that recorded the old hash stop being valid, and the derived items that
// lose their last valid justification are returned.
func (s *Store) Recompute(id, hash string) ([]string, error) {
	return s.run(false, func(o *op) error {
		row, ok, err := o.node(id)
		if err != nil {
			return err
		}
		if !ok {
			return o.insert(id, KindBase, hash, true)
		}
		if row.hash == hash {
			return nil
		}
		if err := o.exec("UPDATE justify_node SET hash=? WHERE id=?", hash, id); err != nil {
			return err
		}
		owners, err := o.owners(id)
		if err != nil {
			return err
		}
		return o.propagate(owners)
	})
}

// Stale lists the derived items that are invalid, or that have a justification
// whose premise is gone or has a different hash. current, when non-nil,
// supplies the premise's hash as the world has it now; when it is nil the
// stored hash is used. Items are sorted by id.
func (s *Store) Stale(current func(id string) (string, bool)) ([]StaleItem, error) {
	s.d.Lock()
	defer s.d.Unlock()
	rows, err := s.d.Conn().Query(`
		SELECT n.id, n.valid, n.reason, j.jid, j.premise, j.premise_hash,
		       p.id IS NOT NULL, COALESCE(p.present, 0), COALESCE(p.hash, '')
		FROM justify_node n
		LEFT JOIN justify_just j ON j.derived = n.id
		LEFT JOIN justify_node p ON p.id = j.premise
		WHERE n.kind <> ?
		ORDER BY n.id, j.jid, j.premise`, KindBase)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[string]*StaleItem{}
	var order []string
	for rows.Next() {
		var id, reason string
		var valid int
		var jid sql.NullInt64
		var premise, phash sql.NullString
		var pExists bool
		var present int
		var hash string
		if err := rows.Scan(&id, &valid, &reason, &jid, &premise, &phash, &pExists, &present, &hash); err != nil {
			return nil, err
		}
		it := byID[id]
		if it == nil {
			it = &StaleItem{ID: id, Valid: valid == 1}
			byID[id] = it
			order = append(order, id)
		}
		if !premise.Valid {
			continue
		}
		var cur string
		var known bool
		if current != nil {
			cur, known = current(premise.String)
		} else {
			cur, known = hash, pExists && present == 1
		}
		switch {
		case !known:
			it.Reasons = appendOnce(it.Reasons, "premise_gone:"+premise.String)
		case cur != phash.String:
			it.Reasons = appendOnce(it.Reasons, "premise_changed:"+premise.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []StaleItem
	for _, id := range order {
		it := byID[id]
		if it.Valid && len(it.Reasons) == 0 {
			continue
		}
		sort.Strings(it.Reasons)
		out = append(out, *it)
	}
	return out, nil
}

// Get returns the stored state of one item.
func (s *Store) Get(id string) (Node, bool, error) {
	s.d.Lock()
	defer s.d.Unlock()
	var n Node
	var present, valid int
	err := s.d.Conn().QueryRow(
		"SELECT id, kind, hash, present, valid, reason FROM justify_node WHERE id=?", id).
		Scan(&n.ID, &n.Kind, &n.Hash, &present, &valid, &n.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, false, nil
	}
	if err != nil {
		return Node{}, false, err
	}
	n.Present, n.Valid = present == 1, valid == 1
	return n, true, nil
}

// Size reports how many items and justification edges the store holds.
func (s *Store) Size() (items, derived, justs, edges int, err error) {
	s.d.Lock()
	defer s.d.Unlock()
	c := s.d.Conn()
	for _, q := range []struct {
		dst *int
		sql string
	}{
		{&items, "SELECT COUNT(*) FROM justify_node"},
		{&derived, "SELECT COUNT(*) FROM justify_node WHERE kind <> '" + KindBase + "'"},
		{&justs, "SELECT COUNT(*) FROM (SELECT DISTINCT derived, jid FROM justify_just)"},
		{&edges, "SELECT COUNT(*) FROM justify_just"},
	} {
		if err = c.QueryRow(q.sql).Scan(q.dst); err != nil {
			return
		}
	}
	return
}

// run executes one operation in a transaction and returns the derived items
// it made invalid. A dry run rolls back, so Plan leaves the store as it was.
func (s *Store) run(dry bool, fn func(o *op) error) ([]string, error) {
	s.d.Lock()
	defer s.d.Unlock()
	tx, err := s.d.Conn().Begin()
	if err != nil {
		return nil, err
	}
	o := &op{tx: tx, before: map[string]bool{}}
	if err := fn(o); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	s.Evals = o.evals
	lost, err := o.lostSet()
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if dry {
		_ = tx.Rollback()
		return lost, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return lost, nil
}

// op is one transaction's working state.
type op struct {
	tx *sql.Tx
	// before holds the validity each touched item had when the operation
	// began, so the lost set is the net change and not every intermediate.
	before map[string]bool
	// excluded are the ids the caller forgot; they are never reported lost.
	excluded map[string]bool
	evals    int
}

type nodeRow struct {
	kind, hash, reason string
	present, valid     bool
}

func (o *op) exec(q string, args ...any) error {
	_, err := o.tx.Exec(q, args...)
	return err
}

func (o *op) node(id string) (nodeRow, bool, error) {
	var r nodeRow
	var present, valid int
	err := o.tx.QueryRow("SELECT kind, hash, present, valid, reason FROM justify_node WHERE id=?", id).
		Scan(&r.kind, &r.hash, &present, &valid, &r.reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nodeRow{}, false, nil
	}
	if err != nil {
		return nodeRow{}, false, err
	}
	r.present, r.valid = present == 1, valid == 1
	return r, true, nil
}

func (o *op) insert(id, kind, hash string, base bool) error {
	valid := 0
	if base {
		valid = 1
	}
	if _, seen := o.before[id]; !seen {
		o.before[id] = false
	}
	return o.exec("INSERT INTO justify_node (id, kind, hash, present, valid, reason) VALUES (?, ?, ?, 1, ?, '')",
		id, kind, hash, valid)
}

// owners are the derived items with a justification that cites id.
func (o *op) owners(id string) ([]string, error) {
	return o.list("SELECT DISTINCT derived FROM justify_just WHERE premise=? ORDER BY derived", id)
}

func (o *op) list(q string, args ...any) ([]string, error) {
	rows, err := o.tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// reaches reports whether target is among the premises, direct or transitive,
// of from.
func (o *op) reaches(from, target string) (bool, error) {
	seen := map[string]bool{from: true}
	stack := []string{from}
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if x == target {
			return true, nil
		}
		ps, err := o.list("SELECT DISTINCT premise FROM justify_just WHERE derived=?", x)
		if err != nil {
			return false, err
		}
		for _, p := range ps {
			if !seen[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
	}
	return false, nil
}

// addJust records one justification unless an identical one exists.
func (o *op) addJust(id string, j []Premise) (bool, error) {
	sorted := append([]Premise(nil), j...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].ID < sorted[b].ID })
	sig := signature(sorted)
	rows, err := o.tx.Query("SELECT jid, premise, premise_hash FROM justify_just WHERE derived=? ORDER BY jid, premise", id)
	if err != nil {
		return false, err
	}
	have := map[int][]Premise{}
	maxJ := 0
	for rows.Next() {
		var jid int
		var p Premise
		if err := rows.Scan(&jid, &p.ID, &p.Hash); err != nil {
			rows.Close()
			return false, err
		}
		have[jid] = append(have[jid], p)
		if jid > maxJ {
			maxJ = jid
		}
	}
	rows.Close()
	for _, ps := range have {
		if signature(ps) == sig {
			return false, nil
		}
	}
	for _, p := range sorted {
		if err := o.exec("INSERT INTO justify_just (derived, jid, premise, premise_hash) VALUES (?, ?, ?, ?)",
			id, maxJ+1, p.ID, p.Hash); err != nil {
			return false, err
		}
	}
	return true, nil
}

func signature(ps []Premise) string {
	sorted := append([]Premise(nil), ps...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].ID < sorted[b].ID })
	var b strings.Builder
	for _, p := range sorted {
		b.WriteString(p.ID)
		b.WriteByte('\x1f')
		b.WriteString(p.Hash)
		b.WriteByte('\x1e')
	}
	return b.String()
}

// forget marks each id not present and propagates.
func (o *op) forget(ids []string) error {
	o.excluded = map[string]bool{}
	var seeds []string
	for _, id := range ids {
		o.excluded[id] = true
		_, ok, err := o.node(id)
		if err != nil {
			return err
		}
		if !ok {
			// Remember the forgetting even though nothing cited the premise
			// yet: a later derivation must not revive it.
			if err := o.exec("INSERT INTO justify_node (id, kind, hash, present, valid, reason) VALUES (?, ?, '', 0, 0, 'forgotten')",
				id, KindBase); err != nil {
				return err
			}
			continue
		}
		if err := o.exec("UPDATE justify_node SET present=0 WHERE id=?", id); err != nil {
			return err
		}
		seeds = append(seeds, id)
	}
	return o.propagate(seeds)
}

// propagate re-evaluates the seeds and, whenever an item's validity changes,
// its owners. It ends when no validity changes remain.
func (o *op) propagate(seeds []string) error {
	queue := append([]string(nil), seeds...)
	queued := map[string]bool{}
	for _, s := range queue {
		queued[s] = true
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		queued[id] = false
		row, ok, err := o.node(id)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		o.evals++
		valid, reason, err := o.eval(id, row)
		if err != nil {
			return err
		}
		if valid == row.valid && reason == row.reason {
			continue
		}
		if _, seen := o.before[id]; !seen {
			o.before[id] = row.valid
		}
		v := 0
		if valid {
			v = 1
		}
		if err := o.exec("UPDATE justify_node SET valid=?, reason=? WHERE id=?", v, reason, id); err != nil {
			return err
		}
		if valid == row.valid {
			continue
		}
		owners, err := o.owners(id)
		if err != nil {
			return err
		}
		for _, w := range owners {
			if !queued[w] {
				queued[w] = true
				queue = append(queue, w)
			}
		}
	}
	return nil
}

// eval computes an item's validity from its present flag and justifications.
func (o *op) eval(id string, row nodeRow) (bool, string, error) {
	if !row.present {
		return false, "forgotten", nil
	}
	if row.kind == KindBase {
		return true, "", nil
	}
	rows, err := o.tx.Query(`
		SELECT j.jid, j.premise, j.premise_hash, p.id IS NOT NULL, COALESCE(p.present, 0),
		       COALESCE(p.valid, 0), COALESCE(p.hash, '')
		FROM justify_just j LEFT JOIN justify_node p ON p.id = j.premise
		WHERE j.derived = ? ORDER BY j.jid, j.premise`, id)
	if err != nil {
		return false, "", err
	}
	defer rows.Close()
	type premState struct {
		id, phash string
		exists    bool
		present   int
		valid     int
		hash      string
	}
	type just struct {
		jid   int
		prems []premState
	}
	var js []just
	for rows.Next() {
		var jid int
		var ps premState
		if err := rows.Scan(&jid, &ps.id, &ps.phash, &ps.exists, &ps.present, &ps.valid, &ps.hash); err != nil {
			return false, "", err
		}
		if len(js) == 0 || js[len(js)-1].jid != jid {
			js = append(js, just{jid: jid})
		}
		js[len(js)-1].prems = append(js[len(js)-1].prems, ps)
	}
	if err := rows.Err(); err != nil {
		return false, "", err
	}
	if len(js) == 0 {
		return false, "no_justification", nil
	}
	first := ""
	for i, j := range js {
		reason := ""
		for _, p := range j.prems {
			switch {
			case !p.exists || p.present == 0:
				reason = "premise_gone:" + p.id
			case p.hash != p.phash:
				reason = "premise_changed:" + p.id
			case p.valid == 0:
				reason = "premise_lost:" + p.id
			}
			if reason != "" {
				break
			}
		}
		if reason == "" {
			return true, "", nil
		}
		if i == 0 {
			first = reason
		}
	}
	return false, first, nil
}

// lostSet returns, sorted, the derived items that were valid when the
// operation began and are not now.
func (o *op) lostSet() ([]string, error) {
	var out []string
	for id, was := range o.before {
		if !was || o.excluded[id] {
			continue
		}
		row, ok, err := o.node(id)
		if err != nil {
			return nil, err
		}
		if !ok || row.valid || row.kind == KindBase {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func appendOnce(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}
