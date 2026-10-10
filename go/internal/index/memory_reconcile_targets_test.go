package index

import (
	"fmt"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Reconcile probes: each one restates an older attribute fact with a new
// value, the way an agent writes a correction. The probe's true target is the
// old fact. Facts share their subject, attribute and service vocabulary with
// many others, so only the full triple identifies the target.

const (
	probeCorpusFacts = 3000
	probeNotes       = 60
	probeScanLimit   = 500
	probeReconcileK  = 12
	probePool        = 500 // 0 = production pool; set by the experiment under test
)

var probeSubjects = []string{"Arden", "Brisa", "Corin", "Dessa", "Eamon", "Fenna", "Gideon", "Hollis",
	"Ilse", "Jorah", "Kestra", "Lorcan", "Maeve", "Nolan", "Orla", "Peregrin", "Quinn", "Rowan",
	"Sable", "Tobin", "Ursel", "Vance", "Wren", "Xavi", "Yara", "Zeno", "Alder", "Bryn", "Cato",
	"Delphine", "Ewan", "Frida", "Galen", "Hesper", "Ivo", "Juno", "Kell", "Lysander", "Mireille", "Niall"}

func probeAttr(k int) string  { return fmt.Sprintf("setting%02d", k) }
func probeSvc(k int) string   { return fmt.Sprintf("node%02d", k) }
func probeValue(n int) string { return fmt.Sprintf("%d minutes", n) }

// probeFact is fact i of the corpus. The (subject, attribute, service) triple
// is unique across the corpus, and every term is shared by many facts.
func probeFact(i int) (subj, attr, svc string, n int) {
	return probeSubjects[i%len(probeSubjects)], probeAttr((i / len(probeSubjects)) % 60),
		probeSvc(i % 37), (i * 7) % 90
}

func probeText(subj, attr, svc string, value string) string {
	return fmt.Sprintf("%s keeps the %s for %s at %s", subj, attr, svc, value)
}

type probe struct {
	id, query string
	target    string
}

// buildProbeCorpus writes the corpus and returns the probes, spread over ages.
func buildProbeCorpus(t *testing.T, ix *Index) []probe {
	t.Helper()
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var all []memory.Entry
	for i := 0; i < probeCorpusFacts; i++ {
		subj, attr, svc, n := probeFact(i)
		all = append(all, memory.Entry{
			ID:       fmt.Sprintf("p%05d", i),
			Text:     probeText(subj, attr, svc, probeValue(n)),
			Agent:    "claude",
			Stamp:    base.Add(time.Duration(i) * 53 * time.Minute).Format(memory.StampFormat),
			Category: "fact",
		})
	}
	per := probeCorpusFacts / probeNotes
	for n := 0; n < probeNotes; n++ {
		memNote(t, ix, fmt.Sprintf("memory/probe%03d.md", n), all[n*per:(n+1)*per]...)
	}
	var out []probe
	for k := 0; k < 100; k++ {
		i := 30*k + 7 // spread over the whole age range
		subj, attr, svc, n := probeFact(i)
		newN := (n + 17 + k%50) % 90
		if newN == n {
			newN = (n + 1) % 90
		}
		v := probeValue(newN)
		out = append(out, probe{
			id:     fmt.Sprintf("p%05d", i),
			query:  probeText(subj, attr, svc, v),
			target: fmt.Sprintf("p%05d", i),
		})
		// Variants that drop identifying terms, as a terse correction would.
		out = append(out, probe{id: fmt.Sprintf("p%05d", i), target: fmt.Sprintf("p%05d", i),
			query: fmt.Sprintf("the %s for %s is now %s", attr, svc, v)})
		out = append(out, probe{id: fmt.Sprintf("p%05d", i), target: fmt.Sprintf("p%05d", i),
			query: fmt.Sprintf("the %s is now %s", attr, v)})
	}
	return out
}

// oldWindowRank is the pre-candidate behaviour: score the newest scanLimit
// rows that pass the filter, rank them, and keep the top k.
func oldWindowRank(t *testing.T, ix *Index, q MemoryQuery) []MemoryHit {
	t.Helper()
	rows, err := ix.DB.Query("SELECT rowid FROM memory_entries ORDER BY stamp DESC, id DESC LIMIT ?", probeScanLimit)
	if err != nil {
		t.Fatal(err)
	}
	var rids []int64
	for rows.Next() {
		var r int64
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		rids = append(rids, r)
	}
	rows.Close()
	cands, err := ix.fetchMemoryRows(rids, q)
	if err != nil {
		t.Fatal(err)
	}
	return ix.rankMemory(cands, q)
}

func TestReconcileTargetsAgainstExactOracle(t *testing.T) {
	if raceEnabled {
		t.Skip("corpus-scale ranking check; adds nothing under the race detector and takes minutes there")
	}
	ix := testIndex(t)
	probes := buildProbeCorpus(t, ix)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	const everything = 1 << 30

	type tally struct{ window, window12, cand, top12, oracleCand, oracle12, lost12, gained12, lostCand int }
	var tot [3]tally
	for pi, p := range probes {
		v := pi % 3
		q := MemoryQuery{Query: p.query, Now: now, Limit: probeReconcileK, ScanLimit: probeScanLimit,
			CandidatePool: ReconcileCandidatePool}
		win := oldWindowRank(t, ix, q)
		tt := &tot[v]
		if containsID(win, p.target) {
			tt.window++
		}
		if containsID(win[:min(len(win), probeReconcileK)], p.target) {
			tt.window12++
		}
		qAll := q
		qAll.Limit = everything
		cand, err := ix.MemoryEntries(qAll)
		if err != nil {
			t.Fatal(err)
		}
		if containsID(cand, p.target) {
			tt.cand++
		}
		newTop := containsID(cand[:min(len(cand), probeReconcileK)], p.target)
		oldTop := containsID(win[:min(len(win), probeReconcileK)], p.target)
		if newTop {
			tt.top12++
		}
		if oldTop && !newTop {
			tt.lost12++
		}
		if !oldTop && newTop {
			tt.gained12++
		}
		if containsID(win, p.target) && !containsID(cand, p.target) {
			tt.lostCand++
		}
		qo := qAll
		qo.ScanLimit = everything
		orc, err := ix.MemoryEntries(qo)
		if err != nil {
			t.Fatal(err)
		}
		if containsID(orc, p.target) {
			tt.oracleCand++
		}
		if containsID(orc[:min(len(orc), probeReconcileK)], p.target) {
			tt.oracle12++
		}
	}
	names := []string{"full triple", "attr+service", "attr only"}
	for v, tt := range tot {
		// The guard: no probe the old window placed in the candidate set may
		// fall out of it. Top-12 is reported, not guarded: the window's top-12
		// for an ambiguous probe reflected how few rivals it could see.
		if tt.lostCand != 0 {
			t.Errorf("%s: %d targets the old window found are missing from the candidate set", names[v], tt.lostCand)
		}
		t.Logf("%-12s probes=100 | old window: in-set %d top12 %d | new path: in-set %d top12 %d | exact oracle: in-set %d top12 %d | new loses vs window: in-set %d top12 %d (gains top12 %d)",
			names[v], tt.window, tt.window12, tt.cand, tt.top12, tt.oracleCand, tt.oracle12, tt.lostCand, tt.lost12, tt.gained12)
	}
}

func containsID(hits []MemoryHit, id string) bool {
	for _, h := range hits {
		if h.ID == id {
			return true
		}
	}
	return false
}
