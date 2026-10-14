package anchorlog

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/anchor"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), ".grimoire"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

var unitA = anchor.Anchor{Type: anchor.UnitActive, Args: map[string]string{"unit": "grimoire.service"}}

func pass(observed string) anchor.Result {
	return anchor.Result{Status: anchor.Pass, Observed: observed}
}

func fail() anchor.Result { return anchor.Result{Status: anchor.Fail, Message: "not active"} }

func TestStateTransitions(t *testing.T) {
	ok := func(obs string) Row { return Row{OK: true, Observed: obs} }
	bad := Row{OK: false}
	cases := []struct {
		name string
		prev *Row
		cur  Row
		want string
	}{
		{"first pass is new", nil, ok("a"), New},
		{"first fail is dead", nil, bad, Dead},
		{"fail after pass is dead", &Row{OK: true, Observed: "a"}, bad, Dead},
		{"fail after fail is dead", &Row{OK: false}, bad, Dead},
		{"same observed still live", &Row{OK: true, Observed: "a"}, ok("a"), Live},
		{"changed observed is drifted", &Row{OK: true, Observed: "a"}, ok("b"), Drifted},
		{"changed observed after a fail is live, not drifted", &Row{OK: false, Observed: "a"}, ok("b"), Live},
		{"empty observed never drifts", &Row{OK: true}, ok("b"), Live},
		{"observed appearing later never drifts", &Row{OK: true, Observed: "a"}, ok(""), Live},
		{"revived pass with no observed is live", &Row{OK: false}, ok(""), Live},
	}
	for _, c := range cases {
		if got := State(c.prev, c.cur); got != c.want {
			t.Errorf("%s: State = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestKeyOfIsStableAndSensitive(t *testing.T) {
	a := anchor.Anchor{Type: anchor.FileContains, Args: map[string]string{"path": "/x/y", "text": "v"}}
	b := anchor.Anchor{Type: anchor.FileContains, Args: map[string]string{"text": "v", "path": "/x/y"}}
	if KeyOf(a) != KeyOf(b) {
		t.Fatalf("argument order changed the key")
	}
	c := anchor.Anchor{Type: anchor.FileContains, Args: map[string]string{"path": "/x/y", "text": "w"}}
	if KeyOf(a) == KeyOf(c) {
		t.Fatalf("different arguments gave the same key")
	}
	if len(KeyOf(a)) != 32 {
		t.Fatalf("key is not 16 bytes of hex: %q", KeyOf(a))
	}
}

func TestLatestStateFollowsTheNewestVerdicts(t *testing.T) {
	st := openStore(t)
	if s, err := st.LatestState("fact1", unitA); err != nil || s != "" {
		t.Fatalf("empty history: %q, %v", s, err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.Record("fact1", "write", unitA, pass("h1")))
	if s, _ := st.LatestState("fact1", unitA); s != New {
		t.Fatalf("one pass: %q, want new", s)
	}
	must(st.Record("fact1", "recall", unitA, pass("h2")))
	if s, _ := st.LatestState("fact1", unitA); s != Drifted {
		t.Fatalf("changed observed: %q, want drifted", s)
	}
	must(st.Record("fact1", "recall", unitA, fail()))
	if s, _ := st.LatestState("fact1", unitA); s != Dead {
		t.Fatalf("fail: %q, want dead", s)
	}
	// A different fact has its own history.
	if s, _ := st.LatestState("fact2", unitA); s != "" {
		t.Fatalf("other fact leaked: %q", s)
	}
}

func TestRecordKeepsOnlyVerdicts(t *testing.T) {
	st := openStore(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.Record("f", "write", unitA, anchor.Result{Status: anchor.Error, Message: "boom"}))
	must(st.Record("f", "write", unitA, anchor.Result{Status: anchor.Skipped, Message: anchor.SkipRemote}))
	rows, err := st.Recent("f", 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("error and skipped were stored: %v %v", rows, err)
	}
}

func TestRecordPrunesToMaxPerAnchor(t *testing.T) {
	st := openStore(t)
	const total = MaxPerAnchor + 25
	for i := 0; i < total; i++ {
		// Every row carries its own index as the observed value, so the survivors
		// can be named exactly.
		if err := st.Record("f", "cli", unitA, pass(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// Another anchor on the same fact is bounded separately.
	other := anchor.Anchor{Type: anchor.PortListening, Args: map[string]string{"port": "8080"}}
	if err := st.Record("f", "cli", other, pass("p")); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Recent("f", 10000)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, r := range rows {
		if r.Anchor == KeyOf(unitA) {
			kept = append(kept, r.Observed)
		}
	}
	if len(kept) != MaxPerAnchor {
		t.Fatalf("kept %d rows for the first anchor, want %d", len(kept), MaxPerAnchor)
	}
	// Recent is newest first: the first survivor is the last write, and the
	// oldest 25 were the ones pruned.
	if kept[0] != fmt.Sprintf("v%d", total-1) {
		t.Fatalf("newest survivor = %q", kept[0])
	}
	if kept[len(kept)-1] != fmt.Sprintf("v%d", total-MaxPerAnchor) {
		t.Fatalf("oldest survivor = %q, want v%d", kept[len(kept)-1], total-MaxPerAnchor)
	}
	if len(rows) != MaxPerAnchor+1 {
		t.Fatalf("total rows = %d, want %d", len(rows), MaxPerAnchor+1)
	}
}

func TestTransitionsListFlipsAndDrift(t *testing.T) {
	rows := []Row{
		{FactID: "f", Anchor: "k", OK: true, Observed: "a", TS: 1, Origin: "write"},
		{FactID: "f", Anchor: "k", OK: true, Observed: "b", TS: 2, Origin: "recall"},
		{FactID: "f", Anchor: "k", OK: false, TS: 3, Origin: "recall"},
		{FactID: "f", Anchor: "k", OK: true, Observed: "c", TS: 4, Origin: "annotate"},
		{FactID: "g", Anchor: "k", OK: true, Observed: "x", TS: 5, Origin: "cli"},
	}
	// Input order must not matter.
	rev := make([]Row, len(rows))
	for i := range rows {
		rev[len(rows)-1-i] = rows[i]
	}
	got := Transitions(rev)
	want := []Transition{
		{FactID: "f", Anchor: "k", Kind: "drifted", TS: 2, Origin: "recall"},
		{FactID: "f", Anchor: "k", Kind: "pass->fail", TS: 3, Origin: "recall"},
		{FactID: "f", Anchor: "k", Kind: "fail->pass", TS: 4, Origin: "annotate"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions:\n got %+v\nwant %+v", got, want)
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	var st *Store
	if err := st.Record("f", "write", unitA, pass("a")); err != nil {
		t.Fatalf("nil Record: %v", err)
	}
	if s, err := st.LatestState("f", unitA); s != "" || err != nil {
		t.Fatalf("nil LatestState: %q %v", s, err)
	}
	if rows, err := st.Recent("", 5); rows != nil || err != nil {
		t.Fatalf("nil Recent: %v %v", rows, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

// A store that cannot write returns the error; it never panics, and the caller
// decides to log and continue.
func TestWriteErrorIsReturnedNotFatal(t *testing.T) {
	st := openStore(t)
	st.Close()
	if err := st.Record("f", "write", unitA, pass("a")); err == nil {
		t.Fatalf("write to a closed store returned no error")
	}
}
