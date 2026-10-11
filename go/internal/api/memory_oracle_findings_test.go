package api

import (
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/modelcheck"
)

// Regression tests for the divergences the memory oracle found, each reduced
// to a minimal sequence by the shrinker. Each one asserts the documented
// behaviour and is skipped while the product does not match it. Remove the
// Skip when the finding is fixed; the test then guards the fix.
//
// The write-up, with the reasoning for which side is wrong, is in
// docs/lab/oracle.md.

func TestOracleF1SubjectSlotDoesNotSupersedeAnotherSubject(t *testing.T) {
	t.Skip("open finding F1 (docs/lab/oracle.md): the value rule ignores the subject")
	runOracle(t, []modelcheck.Op{
		{Kind: modelcheck.KRemember, Slot: 0, Value: 0, Writer: 1},
		{Kind: modelcheck.KRemember, Slot: 1, Value: 1, Writer: 1},
	})
}

func TestOracleF2OutrankedWriterCannotForgetAFact(t *testing.T) {
	t.Skip("open finding F2 (docs/lab/oracle.md): an agent or pulled writer can forget a person's fact")
	runOracle(t, []modelcheck.Op{
		{Kind: modelcheck.KRemember, Slot: 1, Value: 2, Writer: 0},
		{Kind: modelcheck.KHardForget, Writer: 2, Target: 445215},
	})
}

func TestOracleF3ForgetOfASupersededFactKeepsItsChain(t *testing.T) {
	runOracle(t, []modelcheck.Op{
		{Kind: modelcheck.KRemember, Slot: 0, Value: 1, Writer: 2},
		{Kind: modelcheck.KRemember, Slot: 0, Value: 0, Writer: 1},
		{Kind: modelcheck.KForget, Writer: 0, Target: 795976},
	})
}

func TestOracleF4CorrectionClearsEveryContender(t *testing.T) {
	t.Skip("open finding F4 (docs/lab/oracle.md): one correction supersedes one of two contenders")
	runOracle(t, []modelcheck.Op{
		{Kind: modelcheck.KRemember, Slot: 0, Value: 2, Writer: 1},
		{Kind: modelcheck.KRemember, Slot: 0, Value: 1, Writer: 2},
		{Kind: modelcheck.KRemember, Slot: 0, Value: 0, Writer: 1},
	})
}

func TestOracleF6AcceptRetractsTheOtherChallengers(t *testing.T) {
	t.Skip("open finding F6 (docs/lab/oracle.md): accepting one of two challengers leaves the other believed")
	runOracle(t, []modelcheck.Op{
		{Kind: modelcheck.KRemember, Slot: 1, Value: 0, Writer: 1},
		{Kind: modelcheck.KRemember, Slot: 0, Value: 0, Writer: 1},
		{Kind: modelcheck.KRemember, Slot: 0, Value: 2, Writer: 0},
		{Kind: modelcheck.KRemember, Slot: 1, Value: 1, Writer: 0},
		{Kind: modelcheck.KRemember, Slot: 1, Value: 0, Writer: 2},
		{Kind: modelcheck.KRemember, Slot: 0, Value: 0, Writer: 2},
		{Kind: modelcheck.KRemember, Slot: 1, Value: 2, Writer: 1},
		{Kind: modelcheck.KResolve, Writer: 0, Target: 553164, Challenger: 283387, Accept: true},
	})
}

// F4 again, the other shape: a same-rung contender is found first, so the
// store supersedes it and never considers the agent fact above it, which the
// later write should have been recorded alongside.
func TestOracleF4StoreConsidersEveryContender(t *testing.T) {
	t.Skip("open finding F4 (docs/lab/oracle.md): the store decides on the first contender it finds")
	runOracle(t, []modelcheck.Op{
		{Kind: modelcheck.KRemember, Slot: 0, Value: 0, Writer: 1},
		{Kind: modelcheck.KRemember, Slot: 0, Value: 1, Writer: 2},
		{Kind: modelcheck.KRemember, Slot: 0, Value: 2, Writer: 2},
	})
}

func TestOracleF5PersonCanSettleADisputeWithPrivateChallenger(t *testing.T) {
	t.Skip("open finding F5 (docs/lab/oracle.md): a private challenger hides the dispute from its settler")
	runOracle(t, []modelcheck.Op{
		{Kind: modelcheck.KRemember, Slot: 0, Value: 0, Writer: 0},
		{Kind: modelcheck.KRemember, Slot: 0, Value: 2, Writer: 1, Private: true},
		{Kind: modelcheck.KResolve, Writer: 0, Target: 1001780, Challenger: 116452, Accept: false},
	})
}

// TestOracleFindingsStillReproduce keeps the skipped tests honest. Each
// minimal sequence must still diverge with its recorded signature. When a
// finding is fixed this fails on purpose: remove the entry here and the Skip
// above, and the regression test takes over.
func TestOracleFindingsStillReproduce(t *testing.T) {
	cases := []struct {
		sig string
		ops []modelcheck.Op
	}{
		{"[subject-slot]", []modelcheck.Op{
			{Kind: modelcheck.KRemember, Slot: 0, Value: 0, Writer: 1},
			{Kind: modelcheck.KRemember, Slot: 1, Value: 1, Writer: 1},
		}},
		{"[lattice-forget]", []modelcheck.Op{
			{Kind: modelcheck.KRemember, Slot: 1, Value: 2, Writer: 0},
			{Kind: modelcheck.KHardForget, Writer: 2, Target: 445215},
		}},
		{"[supersession]", []modelcheck.Op{
			{Kind: modelcheck.KRemember, Slot: 0, Value: 2, Writer: 1},
			{Kind: modelcheck.KRemember, Slot: 0, Value: 1, Writer: 2},
			{Kind: modelcheck.KRemember, Slot: 0, Value: 0, Writer: 1},
		}},
		{"[multi-contender]", []modelcheck.Op{
			{Kind: modelcheck.KRemember, Slot: 0, Value: 0, Writer: 1},
			{Kind: modelcheck.KRemember, Slot: 0, Value: 1, Writer: 2},
			{Kind: modelcheck.KRemember, Slot: 0, Value: 2, Writer: 2},
		}},
		{"[retraction]", []modelcheck.Op{
			{Kind: modelcheck.KRemember, Slot: 1, Value: 0, Writer: 1},
			{Kind: modelcheck.KRemember, Slot: 0, Value: 0, Writer: 1},
			{Kind: modelcheck.KRemember, Slot: 0, Value: 2, Writer: 0},
			{Kind: modelcheck.KRemember, Slot: 1, Value: 1, Writer: 0},
			{Kind: modelcheck.KRemember, Slot: 1, Value: 0, Writer: 2},
			{Kind: modelcheck.KRemember, Slot: 0, Value: 0, Writer: 2},
			{Kind: modelcheck.KRemember, Slot: 1, Value: 2, Writer: 1},
			{Kind: modelcheck.KResolve, Writer: 0, Target: 553164, Challenger: 283387, Accept: true},
		}},
		{"[resolve-mismatch]", []modelcheck.Op{
			{Kind: modelcheck.KRemember, Slot: 0, Value: 0, Writer: 0},
			{Kind: modelcheck.KRemember, Slot: 0, Value: 2, Writer: 1, Private: true},
			{Kind: modelcheck.KResolve, Writer: 0, Target: 1001780, Challenger: 116452, Accept: false},
		}},
	}
	for _, c := range cases {
		msg := runSequence(c.ops)
		if msg == "" {
			t.Errorf("%s no longer diverges: the finding looks fixed. Remove its Skip and this entry.", c.sig)
			continue
		}
		if got := signature(msg); got != c.sig {
			t.Errorf("%s now diverges as %s: %s", c.sig, got, msg)
		}
	}
}
