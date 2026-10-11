package conflict

import (
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

var now = time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)

func opts() Options { return Options{Now: now} }

func near(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }

// Forged evidence is capped: the same agent with verified evidence outscores
// it, and the difference is the verification term alone.
func TestForgedEvidenceScoresBelowVerified(t *testing.T) {
	real := Candidate{Agent: "claude", Authority: memory.AuthorityAgent, Verification: memory.VerifiedEvidence}
	forged := real
	forged.Verification = memory.UnverifiedEvidence
	r, f := Score(real, opts()), Score(forged, opts())
	if !(r.Total > f.Total) {
		t.Fatalf("verified %.3f should beat forged %.3f", r.Total, f.Total)
	}
	if !near(r.Total-f.Total, WeightVerification) {
		t.Fatalf("the gap should be the verification term alone, got %.3f", r.Total-f.Total)
	}
}

// Real evidence keeps its weight: a verified agent claim beats the same claim
// with no evidence at all.
func TestRealEvidenceKeepsItsWeightOverNone(t *testing.T) {
	withEv := Candidate{Agent: "claude", Authority: memory.AuthorityAgent, Verification: memory.VerifiedEvidence}
	none := Candidate{Agent: "claude", Authority: memory.AuthorityAgent}
	if !(Score(withEv, opts()).Total > Score(none, opts()).Total) {
		t.Fatal("verified evidence must outscore no evidence")
	}
}

// A two-vs-one corroborated pulled claim (two extra independent origins) against
// a lone unverified agent claim: the corroborated claim ranks first. The agent's
// higher authority rung is not enough to beat two independent origins that
// also cost nothing to check.
func TestCorroboratedPulledClaimBeatsLoneUnverifiedAgent(t *testing.T) {
	pulled := Candidate{ID: "pulled", Agent: "connector", Authority: memory.AuthorityPulled,
		Corroboration: 3, ValidFrom: now.AddDate(0, 0, -10)}
	agent := Candidate{ID: "agent", Agent: "claude", Authority: memory.AuthorityAgent,
		Verification: memory.UnverifiedEvidence, Corroboration: 1, ValidFrom: now.AddDate(0, 0, -10)}
	ranked := Rank([]Candidate{agent, pulled}, opts())
	if ranked[0].Candidate.ID != "pulled" {
		t.Fatalf("ranked %s first (%.3f vs %.3f), want the corroborated pulled claim",
			ranked[0].Candidate.ID, ranked[0].Breakdown.Total, ranked[1].Breakdown.Total)
	}
}

// A person's fact never loses to a higher raw score: even with every term
// maxed for the agent and the human side with nothing going for it, the human
// side ranks first.
func TestHumanFactNeverLosesToAHigherRawScore(t *testing.T) {
	human := Candidate{ID: "human", Agent: "me", Authority: memory.AuthorityHuman}
	agent := Candidate{ID: "agent", Agent: "claude", Authority: memory.AuthorityAgent,
		Verification: memory.VerifiedEvidence, Corroboration: 9, ValidFrom: now}
	rel := func(string) float64 { return 1 }
	o := Options{Now: now, Reliability: rel}
	if Score(agent, o).Total <= Score(human, o).Total {
		t.Fatalf("setup: the agent must actually score higher (%.3f vs %.3f)",
			Score(agent, o).Total, Score(human, o).Total)
	}
	if got := Rank([]Candidate{agent, human}, o)[0].Candidate.ID; got != "human" {
		t.Fatalf("ranked %s first; a human fact must rank above any agent or pulled fact", got)
	}
}

// Ties on authority are settled by total, so the human ordering rule does not
// hide the score among human sides.
func TestWithinAnAuthorityRungTheScoreDecides(t *testing.T) {
	a := Candidate{ID: "a", Authority: memory.AuthorityAgent, Verification: memory.UnverifiedEvidence}
	b := Candidate{ID: "b", Authority: memory.AuthorityAgent, Verification: memory.VerifiedEvidence}
	if got := Rank([]Candidate{a, b}, opts())[0].Candidate.ID; got != "b" {
		t.Fatalf("ranked %s first, want the verified one", got)
	}
}

// The breakdown is the reason a person reads, so its parts must add up to the
// total exactly, and every term must be visible.
func TestBreakdownSumsToTheTotal(t *testing.T) {
	c := Candidate{Agent: "claude", Authority: memory.AuthorityAgent,
		Verification: memory.VerifiedEvidence, Corroboration: 2, ValidFrom: now.AddDate(0, 0, -30)}
	b := Score(c, opts())
	sum := b.Authority + b.Verification + b.Corroboration + b.Recency + b.Reliability
	if !near(sum, b.Total) {
		t.Fatalf("terms sum to %.6f but total is %.6f", sum, b.Total)
	}
	if b.Authority <= 0 || b.Verification <= 0 || b.Corroboration <= 0 || b.Recency <= 0 || b.Reliability <= 0 {
		t.Fatalf("a populated candidate should show every term: %+v", b)
	}
}

// An unmeasured agent gets the neutral 0.5, and a supplied reliability is used
// in its place. Out-of-range values are clamped rather than trusted.
func TestReliabilityDefaultsAndIsClamped(t *testing.T) {
	c := Candidate{Agent: "claude", Authority: memory.AuthorityAgent}
	def := Score(c, Options{Now: now})
	if !near(def.Reliability, WeightReliability*DefaultReliability) {
		t.Fatalf("default reliability term = %.4f, want %.4f", def.Reliability, WeightReliability*DefaultReliability)
	}
	high := Score(c, Options{Now: now, Reliability: func(string) float64 { return 7 }})
	if !near(high.Reliability, WeightReliability) {
		t.Fatalf("reliability above 1 must clamp to 1, got %.4f", high.Reliability)
	}
	low := Score(c, Options{Now: now, Reliability: func(string) float64 { return -1 }})
	if low.Reliability != 0 {
		t.Fatalf("reliability below 0 must clamp to 0, got %.4f", low.Reliability)
	}
}

// Recency needs a clock. With none, the term is zero, and a future fact is no
// fresher than now.
func TestRecencyNeedsAClockAndDoesNotRewardTheFuture(t *testing.T) {
	c := Candidate{Agent: "claude", Authority: memory.AuthorityAgent, ValidFrom: now.AddDate(0, 0, -5)}
	if Score(c, Options{}).Recency != 0 {
		t.Fatal("recency without a clock must be zero")
	}
	future := Candidate{Agent: "claude", Authority: memory.AuthorityAgent, ValidFrom: now.AddDate(1, 0, 0)}
	if got, want := Score(future, opts()).Recency, WeightRecency; !near(got, want) {
		t.Fatalf("a future fact scored recency %.4f, want the maximum %.4f", got, want)
	}
}

// Scoring is deterministic: the same input ranks the same way every time, with
// ties broken by id rather than map order.
func TestRankIsDeterministic(t *testing.T) {
	cands := []Candidate{
		{ID: "z", Agent: "a", Authority: memory.AuthorityAgent},
		{ID: "m", Agent: "a", Authority: memory.AuthorityAgent},
		{ID: "b", Agent: "a", Authority: memory.AuthorityAgent},
	}
	for i := 0; i < 20; i++ {
		got := Rank(cands, opts())
		if got[0].Candidate.ID != "b" || got[1].Candidate.ID != "m" || got[2].Candidate.ID != "z" {
			t.Fatalf("run %d ranked %s,%s,%s", i, got[0].Candidate.ID, got[1].Candidate.ID, got[2].Candidate.ID)
		}
	}
}
