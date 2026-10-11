package conflict

import "testing"

// Hop depth is a tie-break only: among exactly equal totals the fact nearer its
// source ranks first, and it never reorders sides whose totals differ.

func TestHopDepthBreaksAnExactTie(t *testing.T) {
	near := Candidate{ID: "near", Agent: "a", Verification: "verified", HopDepth: 0}
	far := Candidate{ID: "far", Agent: "a", Verification: "verified", HopDepth: 2}
	ranked := Rank([]Candidate{far, near}, Options{})
	if ranked[0].Candidate.ID != "near" {
		t.Fatalf("equal totals: top = %s, want the fact with hop depth 0", ranked[0].Candidate.ID)
	}
}

func TestHopDepthDoesNotOverrideTotal(t *testing.T) {
	// A corroborated fact two hops out still beats an uncorroborated direct one.
	corroborated := Candidate{ID: "far", Agent: "a", Verification: "verified", Corroboration: 3, HopDepth: 2}
	direct := Candidate{ID: "near", Agent: "b", Verification: "unverified", HopDepth: 0}
	ranked := Rank([]Candidate{direct, corroborated}, Options{})
	if ranked[0].Candidate.ID != "far" {
		t.Fatalf("top = %s, want the higher total regardless of hop depth", ranked[0].Candidate.ID)
	}
}
