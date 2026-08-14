// Package crdt is a sequence CRDT for note text (conflict-free replicated document).
//
// Port of server/crdt.py, preserving the on-disk JSON format exactly — existing
// .grimoire/crdt state and any paired peer must keep working across the switch.
//
// Model: a Logoot/fractional-index sequence CRDT. Every character is an *atom*
// with a globally-unique, totally-ordered identifier
//
//	id = (key, site, clock)
//
// where key is a fractional-index digit path (a slice of ints) that densely
// orders atoms, and (site, clock) makes the id unique and breaks ties between
// concurrent inserts. Deletes are tombstones; document state is the set of live
// atoms.
//
// Merge is a state-based join (union of atoms, union of tombstones, minus any
// tombstoned atom). That join is commutative, associative and idempotent, so all
// replicas seeing the same edits converge to identical text regardless of
// arrival order.
package crdt

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Base is the digit radix for fractional keys.
const Base = 1 << 16

// ID identifies one atom. Key is the fractional index path.
type ID struct {
	Key   []int
	Site  string
	Clock int
}

// mapKey renders an ID usable as a Go map key (slices aren't comparable).
func (id ID) mapKey() string {
	var b strings.Builder
	for i, d := range id.Key {
		if i > 0 {
			b.WriteByte('.')
		}
		fmt.Fprintf(&b, "%d", d)
	}
	b.WriteByte('|')
	b.WriteString(id.Site)
	fmt.Fprintf(&b, "|%d", id.Clock)
	return b.String()
}

// KeyBetween returns a fractional key strictly between keys a and b (a < b).
// Keys compare lexicographically; a shorter key that is a prefix of a longer one
// sorts first (missing digit = -1 lower bound).
func KeyBetween(a, b []int) []int {
	out := []int{}
	for i := 0; ; i++ {
		da := -1
		if i < len(a) {
			da = a[i]
		}
		db := Base
		if i < len(b) {
			db = b[i]
		}
		if db-da > 1 {
			return append(out, (da+db)/2)
		}
		if da >= 0 {
			out = append(out, da)
		} else {
			out = append(out, 0)
		}
	}
}

// compareKeys orders two fractional keys lexicographically, prefix-first.
func compareKeys(x, y []int) int {
	n := len(x)
	if len(y) < n {
		n = len(y)
	}
	for i := 0; i < n; i++ {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(x) < len(y):
		return -1
	case len(x) > len(y):
		return 1
	}
	return 0
}

// lessID is the total order on atoms: (key, site, clock).
func lessID(x, y ID) bool {
	if c := compareKeys(x.Key, y.Key); c != 0 {
		return c < 0
	}
	if x.Site != y.Site {
		return x.Site < y.Site
	}
	return x.Clock < y.Clock
}

// Doc is a replicated text document. Site identifies this replica.
type Doc struct {
	Site  string
	Clock int

	atoms map[string]atom // mapKey -> atom
	tombs map[string]ID   // mapKey -> id
}

type atom struct {
	id ID
	ch rune
}

func New(site string) *Doc {
	if site == "" {
		site = "local"
	}
	return &Doc{Site: site, atoms: map[string]atom{}, tombs: map[string]ID{}}
}

func (d *Doc) orderedIDs() []ID {
	ids := make([]ID, 0, len(d.atoms))
	for _, a := range d.atoms {
		ids = append(ids, a.id)
	}
	sort.Slice(ids, func(i, j int) bool { return lessID(ids[i], ids[j]) })
	return ids
}

// Text renders the live atoms in order.
func (d *Doc) Text() string {
	var b strings.Builder
	for _, id := range d.orderedIDs() {
		b.WriteRune(d.atoms[id.mapKey()].ch)
	}
	return b.String()
}

func (d *Doc) newID(leftKey, rightKey []int) ID {
	d.Clock++
	return ID{Key: KeyBetween(leftKey, rightKey), Site: d.Site, Clock: d.Clock}
}

// insertRun inserts chars between visible positions leftIdx-1 and leftIdx.
func (d *Doc) insertRun(ids []ID, leftIdx int, chars []rune) {
	var leftKey []int
	if leftIdx-1 >= 0 {
		leftKey = ids[leftIdx-1].Key
	}
	rightKey := []int{Base}
	if leftIdx < len(ids) {
		rightKey = ids[leftIdx].Key
	}
	prev := leftKey
	for _, ch := range chars {
		id := d.newID(prev, rightKey)
		d.atoms[id.mapKey()] = atom{id: id, ch: ch}
		prev = id.Key
	}
}

// LocalEdit reconciles a full-text replacement (from a file or editor) into
// CRDT operations.
func (d *Doc) LocalEdit(newText string) {
	ids := d.orderedIDs()
	old := make([]rune, 0, len(ids))
	for _, id := range ids {
		old = append(old, d.atoms[id.mapKey()].ch)
	}
	next := []rune(newText)
	if string(old) == newText {
		return
	}
	for _, op := range newSequenceMatcher(old, next).getOpcodes() {
		switch op.Tag {
		case "equal":
			continue
		case "delete", "replace":
			for k := op.I1; k < op.I2; k++ {
				d.tombs[ids[k].mapKey()] = ids[k]
				delete(d.atoms, ids[k].mapKey())
			}
		}
		if op.Tag == "insert" || op.Tag == "replace" {
			d.insertRun(ids, op.I1, next[op.J1:op.J2])
		}
	}
}

// Merge is the CRDT join: union of atoms and tombstones, minus anything the
// union of tombstones covers.
func (d *Doc) Merge(other *Doc) *Doc {
	for k, id := range other.tombs {
		d.tombs[k] = id
	}
	for k, a := range other.atoms {
		if _, dead := d.tombs[k]; dead {
			continue
		}
		if _, exists := d.atoms[k]; !exists {
			d.atoms[k] = a
		}
	}
	for k := range d.atoms {
		if _, dead := d.tombs[k]; dead {
			delete(d.atoms, k)
		}
	}
	// advance our clock past anything observed, to avoid future id collisions
	for _, a := range other.atoms {
		if a.id.Site == d.Site && a.id.Clock > d.Clock {
			d.Clock = a.id.Clock
		}
	}
	for _, id := range other.tombs {
		if id.Site == d.Site && id.Clock > d.Clock {
			d.Clock = id.Clock
		}
	}
	return d
}

// ToJSON serializes in canonical order: identical document state must produce
// identical bytes, so the on-disk file doesn't churn and any implementation can
// reproduce it. Ordering is not semantic — FromJSON rebuilds the same state
// either way.
//
// The encoding deliberately matches Python's json.dumps(ensure_ascii=True,
// separators=(",", ":")) byte for byte, so switching implementations doesn't
// rewrite every CRDT file with different escaping.
func (d *Doc) ToJSON() string {
	ids := d.orderedIDs()
	tombIDs := make([]ID, 0, len(d.tombs))
	for _, id := range d.tombs {
		tombIDs = append(tombIDs, id)
	}
	sort.Slice(tombIDs, func(i, j int) bool { return lessID(tombIDs[i], tombIDs[j]) })

	var b strings.Builder
	b.WriteString(`{"site":`)
	b.WriteString(pyJSONString(d.Site))
	fmt.Fprintf(&b, `,"clock":%d,"atoms":[`, d.Clock)
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		writeIDJSON(&b, id)
		b.WriteByte(',')
		b.WriteString(pyJSONString(string(d.atoms[id.mapKey()].ch)))
		b.WriteByte(']')
	}
	b.WriteString(`],"tombs":[`)
	for i, id := range tombIDs {
		if i > 0 {
			b.WriteByte(',')
		}
		writeIDJSON(&b, id)
		b.WriteByte(']')
	}
	b.WriteString("]}")
	return b.String()
}

// writeIDJSON emits `[[key...],site,clock` — the caller closes the bracket,
// since atoms append a character and tombstones do not.
func writeIDJSON(b *strings.Builder, id ID) {
	b.WriteString("[[")
	for i, d := range id.Key {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(b, "%d", d)
	}
	b.WriteString("],")
	b.WriteString(pyJSONString(id.Site))
	fmt.Fprintf(b, ",%d", id.Clock)
}

type wireDoc struct {
	Site  string              `json:"site"`
	Clock int                 `json:"clock"`
	Atoms [][]json.RawMessage `json:"atoms"`
	Tombs [][]json.RawMessage `json:"tombs"`
}

// FromJSON parses a serialized document. Field order and escaping are
// irrelevant here — only the values matter — so documents written by any
// version or implementation load.
func FromJSON(data, site string) (*Doc, error) {
	var w wireDoc
	if err := json.Unmarshal([]byte(data), &w); err != nil {
		return nil, err
	}
	d := New(site)
	d.Clock = w.Clock
	for _, raw := range w.Atoms {
		if len(raw) != 4 {
			return nil, fmt.Errorf("atom entry must have 4 fields, got %d", len(raw))
		}
		id, err := parseID(raw)
		if err != nil {
			return nil, err
		}
		var ch string
		if err := json.Unmarshal(raw[3], &ch); err != nil {
			return nil, err
		}
		r := []rune(ch)
		if len(r) != 1 {
			return nil, fmt.Errorf("atom character must be one code point, got %q", ch)
		}
		d.atoms[id.mapKey()] = atom{id: id, ch: r[0]}
	}
	for _, raw := range w.Tombs {
		if len(raw) != 3 {
			return nil, fmt.Errorf("tomb entry must have 3 fields, got %d", len(raw))
		}
		id, err := parseID(raw)
		if err != nil {
			return nil, err
		}
		d.tombs[id.mapKey()] = id
	}
	return d, nil
}

func parseID(raw []json.RawMessage) (ID, error) {
	var id ID
	if err := json.Unmarshal(raw[0], &id.Key); err != nil {
		return id, err
	}
	if err := json.Unmarshal(raw[1], &id.Site); err != nil {
		return id, err
	}
	if err := json.Unmarshal(raw[2], &id.Clock); err != nil {
		return id, err
	}
	if id.Key == nil {
		id.Key = []int{}
	}
	return id, nil
}

// FromText builds a document from an initial string.
func FromText(text, site string) *Doc {
	d := New(site)
	d.LocalEdit(text)
	return d
}
