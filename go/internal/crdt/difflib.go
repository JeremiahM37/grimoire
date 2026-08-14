package crdt

// A faithful port of the subset of Python's difflib.SequenceMatcher that
// local_edit() depends on.
//
// This exists because the diff algorithm is load-bearing for compatibility, not
// an implementation detail. SequenceMatcher is Ratcliff-Obershelp (recursive
// longest-matching-block), NOT the Myers diff every Go diff library implements.
// A different opcode decomposition produces different CRDT atom identifiers for
// the same edit, so a Go replica would write documents Python would never have
// written. The CRDT would still converge — but the on-disk state would stop
// being reproducible across implementations, which is exactly the property the
// strict-compatibility port exists to preserve.
//
// Scope: isjunk=None and autojunk=False, which is how server/crdt.py calls it.
// The junk-handling branches are therefore omitted rather than ported dead.
//
// Sequences are []rune, not []byte: Python compares strings by code point, and
// byte-wise diffing would corrupt every non-ASCII edit.

// match is one matching block: a[A:A+Size] == b[B:B+Size].
type match struct{ A, B, Size int }

// opcode is one edit instruction; Tag is "equal", "replace", "delete" or "insert".
type opcode struct {
	Tag            string
	I1, I2, J1, J2 int
}

type sequenceMatcher struct {
	a, b []rune
	b2j  map[rune][]int
}

func newSequenceMatcher(a, b []rune) *sequenceMatcher {
	sm := &sequenceMatcher{a: a, b: b, b2j: make(map[rune][]int, len(b))}
	// b2j maps each element of b to the ascending list of its indices. With
	// autojunk=False there is no popular-element filtering to apply.
	for i, ch := range b {
		sm.b2j[ch] = append(sm.b2j[ch], i)
	}
	return sm
}

// findLongestMatch returns the earliest-in-a, then earliest-in-b, then longest
// matching block within a[alo:ahi] and b[blo:bhi] — the tie-breaking order is
// part of the contract, since it decides which decomposition we get.
func (sm *sequenceMatcher) findLongestMatch(alo, ahi, blo, bhi int) match {
	besti, bestj, bestsize := alo, blo, 0

	// j2len[j] = length of the longest match ending at a[i-1], b[j-1]
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for _, j := range sm.b2j[sm.a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len = newj2len
	}

	// Extend the block over equal elements on both sides. With no junk this is
	// usually a no-op, but it is part of the algorithm and cheap to keep exact.
	for besti > alo && bestj > blo && sm.a[besti-1] == sm.b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi &&
		sm.a[besti+bestsize] == sm.b[bestj+bestsize] {
		bestsize++
	}
	return match{besti, bestj, bestsize}
}

// getMatchingBlocks returns non-adjacent matching blocks in ascending order,
// terminated by the conventional empty sentinel (len(a), len(b), 0).
func (sm *sequenceMatcher) getMatchingBlocks() []match {
	la, lb := len(sm.a), len(sm.b)
	type region struct{ alo, ahi, blo, bhi int }
	queue := []region{{0, la, 0, lb}}
	var blocks []match

	for len(queue) > 0 {
		// LIFO, matching Python's queue.pop()
		r := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		m := sm.findLongestMatch(r.alo, r.ahi, r.blo, r.bhi)
		if m.Size > 0 {
			blocks = append(blocks, m)
			if r.alo < m.A && r.blo < m.B {
				queue = append(queue, region{r.alo, m.A, r.blo, m.B})
			}
			if m.A+m.Size < r.ahi && m.B+m.Size < r.bhi {
				queue = append(queue, region{m.A + m.Size, r.ahi, m.B + m.Size, r.bhi})
			}
		}
	}
	sortMatches(blocks)

	// Collapse adjacent blocks into single runs.
	var i1, j1, k1 int
	var out []match
	for _, m := range blocks {
		if i1+k1 == m.A && j1+k1 == m.B {
			k1 += m.Size
			continue
		}
		if k1 > 0 {
			out = append(out, match{i1, j1, k1})
		}
		i1, j1, k1 = m.A, m.B, m.Size
	}
	if k1 > 0 {
		out = append(out, match{i1, j1, k1})
	}
	return append(out, match{la, lb, 0})
}

// getOpcodes turns matching blocks into the edit script local_edit consumes.
func (sm *sequenceMatcher) getOpcodes() []opcode {
	var i, j int
	var out []opcode
	for _, m := range sm.getMatchingBlocks() {
		tag := ""
		switch {
		case i < m.A && j < m.B:
			tag = "replace"
		case i < m.A:
			tag = "delete"
		case j < m.B:
			tag = "insert"
		}
		if tag != "" {
			out = append(out, opcode{tag, i, m.A, j, m.B})
		}
		i, j = m.A+m.Size, m.B+m.Size
		if m.Size > 0 {
			out = append(out, opcode{"equal", m.A, i, m.B, j})
		}
	}
	return out
}

// sortMatches orders blocks by (A, B, Size), the tuple order Python sorts on.
func sortMatches(ms []match) {
	// insertion sort: block counts here are small and this keeps the ordering
	// rule explicit rather than hidden in a comparator closure
	for i := 1; i < len(ms); i++ {
		for j := i; j > 0 && lessMatch(ms[j], ms[j-1]); j-- {
			ms[j], ms[j-1] = ms[j-1], ms[j]
		}
	}
}

func lessMatch(x, y match) bool {
	if x.A != y.A {
		return x.A < y.A
	}
	if x.B != y.B {
		return x.B < y.B
	}
	return x.Size < y.Size
}
