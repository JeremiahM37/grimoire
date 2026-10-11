package api

import (
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/settings"
)

// ipsServer returns a server whose only setting is memory_position_explore.
func ipsServer(t *testing.T, explore string) *Server {
	t.Helper()
	st := settings.New(t.TempDir())
	if explore != "" {
		if err := st.Update(map[string]string{"memory_position_explore": explore}); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{Settings: st}
}

// TestIpsPositionsAssignsSlatePositions checks that every item gets its 1-based
// slate position and the slate keeps its items, with exploration off or on.
func TestIpsPositionsAssignsSlatePositions(t *testing.T) {
	restore := exp.Set(ipsFlag)
	defer restore()
	s := ipsServer(t, "0.5")
	lg := ctxLog{session: "sess", query: "q"}
	sawExplored, sawPlain := false, false
	for trial := 0; trial < 200; trial++ {
		items := []contextItem{{Key: "a"}, {Key: "b"}, {Key: "c"}, {Key: "d"}}
		out, got := s.ipsPositions(items, lg)
		if got.slate == "" || len(got.slate) != 16 {
			t.Fatalf("slate id %q", got.slate)
		}
		seen := map[string]bool{}
		for i, it := range out {
			if it.pos != i+1 {
				t.Fatalf("item %s at index %d has pos %d", it.Key, i, it.pos)
			}
			if it.explored != out[0].explored {
				t.Fatalf("explored flag differs within one slate")
			}
			seen[it.Key] = true
		}
		if len(seen) != 4 {
			t.Fatalf("slate lost or duplicated items: %v", seen)
		}
		if out[0].explored {
			sawExplored = true
		} else {
			sawPlain = true
		}
	}
	if !sawExplored || !sawPlain {
		t.Fatalf("exploration at 0.5 should give both arms in 200 slates (explored %v, plain %v)", sawExplored, sawPlain)
	}
}

// TestIpsPositionsWithoutExplorationKeepsRank checks the default: exploration
// off, so the slate is never shuffled and positions follow the ranking.
func TestIpsPositionsWithoutExplorationKeepsRank(t *testing.T) {
	restore := exp.Set(ipsFlag)
	defer restore()
	s := ipsServer(t, "")
	items := []contextItem{{Key: "a"}, {Key: "b"}, {Key: "c"}}
	out, _ := s.ipsPositions(items, ctxLog{session: "x"})
	for i, want := range []string{"a", "b", "c"} {
		if out[i].Key != want || out[i].pos != i+1 || out[i].explored {
			t.Fatalf("index %d: got %s pos %d explored %v", i, out[i].Key, out[i].pos, out[i].explored)
		}
	}
}

// TestIpsFieldsAreZeroWhenUnpositioned checks that flag-off items produce the
// default exposure columns.
func TestIpsFieldsAreZeroWhenUnpositioned(t *testing.T) {
	pos, slate, explored, feats := ipsFields(ctxLog{slate: "abc"}, contextItem{Key: "k", feat: []float64{1}})
	if pos != 0 || slate != "" || explored || feats != "" {
		t.Fatalf("unpositioned item logged %d %q %v %q", pos, slate, explored, feats)
	}
}
