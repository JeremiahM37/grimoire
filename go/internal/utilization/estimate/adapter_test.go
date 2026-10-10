package estimate

import (
	"errors"
	"math/rand"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/utilization"
)

func TestCausalAdapterRecoversAKnownLift(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	var recs []utilization.Record
	for i := 0; i < 4000; i++ {
		shown := rng.Float64() >= 0.2 // 20% withheld
		p := 0.5
		if shown {
			p = 0.7 // the memory raises the good-outcome rate by 0.2
		}
		y := 0.0
		if rng.Float64() < p {
			y = 1
		}
		recs = append(recs, utilization.Record{Treated: shown, PWithhold: 0.2, Y: y, Stage: "action", Tool: "Bash"})
	}
	e, err := NewCausal().Lift(recs)
	if err != nil {
		t.Fatal(err)
	}
	if e.Lo > 0.2 || e.Hi < 0.2 || e.Effect < 0.1 {
		t.Fatalf("lift %.3f [%.3f, %.3f] does not cover 0.2", e.Effect, e.Lo, e.Hi)
	}
}

func TestCausalAdapterSaysInsufficientRatherThanANumber(t *testing.T) {
	recs := []utilization.Record{{Treated: true, PWithhold: 0.2, Y: 1}, {Treated: false, PWithhold: 0.2, Y: 0}}
	if _, err := NewCausal().Lift(recs); !errors.Is(err, utilization.ErrInsufficient) {
		t.Fatalf("err = %v, want ErrInsufficient", err)
	}
}
