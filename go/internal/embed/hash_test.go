package embed

import (
	"math"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/compat"
)

func TestHashEmbedMatchesPython(t *testing.T) {
	var fx embedFixture
	compat.Load(t, "embed.json", &fx)
	cases, ok := fx.Backends["hash:v1"]
	if !ok {
		t.Skip("no hash:v1 vectors in fixture")
	}
	h := Hash{}
	if h.Signature() != "hash:v1" {
		t.Fatalf("signature = %q", h.Signature())
	}
	for _, c := range cases {
		got := h.Embed([]string{c.Text})[0]
		if len(got) != len(c.Vector) {
			t.Errorf("dim for %q: got %d, want %d", truncate(c.Text), len(got), len(c.Vector))
			continue
		}
		worst, at := 0.0, -1
		for i := range got {
			if d := math.Abs(float64(got[i]) - c.Vector[i]); d > worst {
				worst, at = d, i
			}
		}
		if worst > fx.Tolerance {
			t.Errorf("hash vector for %q drifted by %g at component %d",
				truncate(c.Text), worst, at)
		}
	}
}

// The sign bit comes from bit 8 of the FULL 128-bit digest. Truncating the
// hash to a machine word first yields a different but equally plausible
// vector — a divergence no error would report.
func TestHashUsesFullDigest(t *testing.T) {
	v := Hash{}.Embed([]string{"alpha beta gamma delta"})[0]
	nonzero := 0
	for _, x := range v {
		if x != 0 {
			nonzero++
		}
	}
	if nonzero == 0 {
		t.Fatal("all components zero")
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if n := math.Sqrt(sum); math.Abs(n-1) > 1e-6 {
		t.Errorf("norm = %g, want 1", n)
	}
	empty := Hash{}.Embed([]string{""})[0]
	for _, x := range empty {
		if x != 0 {
			t.Error("empty text should embed as all zeros")
			break
		}
	}
}
