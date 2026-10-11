package api

import (
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/calibrate"
	"github.com/JeremiahM37/grimoire/go/internal/conflict"
)

// A dispute suggestion reads the learned per-agent reliability, but an agent
// with too little record is scored as unknown, so a new writer is not
// penalised for having no history.
func TestDisputeReliabilityIgnoresAgentsWithTooLittleRecord(t *testing.T) {
	s, _ := testServer(t)
	st, err := calibrate.Open(s.Index.DB)
	if err != nil {
		t.Fatal(err)
	}
	fn := s.learnedReliability()
	if fn == nil {
		t.Fatal("no reliability function with an index present")
	}
	if got := fn("newcomer"); got != conflict.DefaultReliability {
		t.Fatalf("unknown agent = %.3f, want the neutral default", got)
	}
	for i := 0; i < minReliabilityEvents-1; i++ {
		if err := st.Bump("sparse", calibrate.Challenged); err != nil {
			t.Fatal(err)
		}
	}
	if got := fn("sparse"); got != conflict.DefaultReliability {
		t.Fatalf("agent below the event floor = %.3f, want the neutral default", got)
	}
	for i := 0; i < 10; i++ {
		if err := st.Bump("sloppy", calibrate.Challenged); err != nil {
			t.Fatal(err)
		}
		if err := st.Bump("careful", calibrate.Confirmed); err != nil {
			t.Fatal(err)
		}
	}
	if sloppy, careful := fn("sloppy"), fn("careful"); !(sloppy < conflict.DefaultReliability && careful > sloppy) {
		t.Fatalf("sloppy=%.3f careful=%.3f, want sloppy below neutral and careful above sloppy", sloppy, careful)
	}
}
