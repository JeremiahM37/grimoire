package modelcheck

import (
	"fmt"
	"math/rand"
	"strings"
)

// Kind is one kind of operation the generator draws.
type Kind int

const (
	KRemember   Kind = iota // write a fact to a slot as a writer
	KForget                 // soft-forget (retract) a fact, as a writer
	KHardForget             // hard-forget a fact (tombstone), as a writer
	KResolve                // settle a dispute as a writer (keep or accept)
	KReindex                // rebuild the index from the files
	KRestart                // close and reopen the server over the same vault
	KProbe                  // as_of and default-recall probe at a sampled step
)

var kindNames = [...]string{"KRemember", "KForget", "KHardForget", "KResolve", "KReindex", "KRestart", "KProbe"}

func (k Kind) String() string { return kindNames[k] }

// Op is one operation. Target and Challenger are ledger ordinals taken modulo
// the number of facts that exist when the op runs, so a sequence stays
// meaningful after shrinking removes earlier operations. An op with nothing
// to name (empty ledger) is a no-op in both the model and the server.
type Op struct {
	Kind       Kind
	Slot       int
	Value      int
	Writer     int
	Private    bool
	Target     int
	Challenger int
	Accept     bool // KResolve: accept the challenger instead of keep
	Sample     int  // KProbe: the step to probe, modulo the current clock
}

// GoLiteral renders the op as a Go composite literal for a regression test.
func (o Op) GoLiteral() string {
	var b strings.Builder
	fmt.Fprintf(&b, "{Kind: modelcheck.%s", o.Kind)
	switch o.Kind {
	case KRemember:
		fmt.Fprintf(&b, ", Slot: %d, Value: %d, Writer: %d", o.Slot, o.Value, o.Writer)
		if o.Private {
			b.WriteString(", Private: true")
		}
	case KForget, KHardForget:
		fmt.Fprintf(&b, ", Writer: %d, Target: %d", o.Writer, o.Target)
	case KResolve:
		fmt.Fprintf(&b, ", Writer: %d, Target: %d, Challenger: %d, Accept: %v", o.Writer, o.Target, o.Challenger, o.Accept)
	case KProbe:
		fmt.Fprintf(&b, ", Slot: %d, Sample: %d", o.Slot, o.Sample)
	}
	b.WriteString("}")
	return b.String()
}

// Generate returns a deterministic sequence of n operations for a seed. The
// weights favour writes, because the interesting states come from writes that
// collide on a slot; resolves and forgets are common enough to reach disputes.
func Generate(seed int64, n int) []Op {
	r := rand.New(rand.NewSource(seed))
	ops := make([]Op, 0, n)
	for len(ops) < n {
		ops = append(ops, genOne(r))
	}
	return ops
}

func genOne(r *rand.Rand) Op {
	roll := r.Intn(100)
	op := Op{
		Slot:       r.Intn(len(Slots)),
		Value:      r.Intn(len(Values)),
		Writer:     r.Intn(len(Writers)),
		Target:     r.Intn(1 << 20),
		Challenger: r.Intn(1 << 20),
		Sample:     r.Intn(1 << 20),
	}
	switch {
	case roll < 50:
		op.Kind = KRemember
		op.Private = r.Intn(10) == 0
	case roll < 60:
		op.Kind = KForget
	case roll < 67:
		op.Kind = KHardForget
	case roll < 80:
		op.Kind = KResolve
		op.Accept = r.Intn(2) == 0
	case roll < 84:
		op.Kind = KReindex
	case roll < 88:
		op.Kind = KRestart
	default:
		op.Kind = KProbe
	}
	return op
}

// Shrink reduces a failing sequence to a smaller one that still fails. It
// removes chunks, then single operations, until no removal keeps the failure.
// fails must be deterministic.
func Shrink(ops []Op, fails func([]Op) bool) []Op {
	cur := append([]Op(nil), ops...)
	for chunk := len(cur) / 2; chunk >= 1; chunk /= 2 {
		for start := 0; start+chunk <= len(cur); {
			cand := append(append([]Op(nil), cur[:start]...), cur[start+chunk:]...)
			if fails(cand) {
				cur = cand
			} else {
				start++
			}
		}
	}
	return cur
}

// RegressionTest renders a shrunk sequence as a ready-to-paste Go test that
// calls the runner in internal/api.
func RegressionTest(name string, ops []Op) string {
	var b strings.Builder
	fmt.Fprintf(&b, "func TestOracleRegression%s(t *testing.T) {\n\trunOracle(t, []modelcheck.Op{\n", name)
	for _, op := range ops {
		fmt.Fprintf(&b, "\t\t%s,\n", op.GoLiteral())
	}
	b.WriteString("\t})\n}\n")
	return b.String()
}
