package memory

import "testing"

// What the value-slot path costs on the write path, which is the whole basis
// of the claim that it is cheaper than a model call.

var benchKnown = func() []Entry {
	out := make([]Entry, 0, 40)
	for i := 0; i < 40; i++ {
		out = append(out, Entry{ID: string(rune('a' + i)),
			Text: "the deploy pipeline for service number takes some minutes end to end"})
	}
	return out
}()

const benchFact = "I'm hoping to beat my personal best time of 25:50 this time around"

func BenchmarkDecide40Candidates(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Decide(benchFact, benchKnown)
	}
}

func BenchmarkValueUpdateOnePair(b *testing.B) {
	prev := "I set a personal best time in the charity 5K run with a time of 27:12"
	for i := 0; i < b.N; i++ {
		ValueUpdate(prev, benchFact)
	}
}
