package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Tests here turn the clone flag on with exp.Set and must not run in parallel.

func cloneTexts(cs []cloneCand) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.text
	}
	return out
}

func containsText(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestCloneTemplatesRestateTheSlot(t *testing.T) {
	st := newCloneNoteState()
	got := cloneTexts(st.candidates("Dana K. Berg owns the release checklist"))
	for _, want := range []string{
		"Dana K. Berg holding: the release checklist",
		"Which holding does Dana K. Berg have?",
	} {
		if !containsText(got, want) {
			t.Errorf("missing alias %q in %q", want, got)
		}
	}
}

func TestCloneUnknownPredicateGetsNoAliases(t *testing.T) {
	st := newCloneNoteState()
	if got := st.candidates("Dana K. Berg enjoys long walks"); len(got) != 0 {
		t.Errorf("fact outside the lexicon produced aliases: %q", cloneTexts(got))
	}
}

func TestCloneCorefResolvesToLastFullSubject(t *testing.T) {
	st := newCloneNoteState()
	st.candidates("pgbouncer-512 listens on port 6432")
	got := cloneTexts(st.candidates("It runs on db01.prod"))
	if !containsText(got, "pgbouncer-512 runs on db01.prod") {
		t.Errorf("pronoun not resolved to the last subject: %q", got)
	}
	if st.abstain != 0 {
		t.Errorf("unexpected abstention: %d", st.abstain)
	}
}

// A short form that two full names share in one note is ambiguous, so the
// pronoun after it must abstain rather than pick one of them.
func TestCloneCorefAbstainsOnAmbiguousShortForm(t *testing.T) {
	st := newCloneNoteState()
	st.candidates("Dana K. Berg owns the release checklist")
	st.candidates("Dana M. Okafor owns the indexer list")
	st.candidates("Dana prefers dark mode")
	got := st.candidates("It works in UTC")
	if len(got) != 0 {
		t.Errorf("ambiguous antecedent produced aliases: %q", cloneTexts(got))
	}
	if st.abstain != 1 {
		t.Errorf("abstentions = %d, want 1", st.abstain)
	}
}

// Coreference fixture: each case is a note whose pronoun fact has a known
// antecedent alias, or none. want is the alias that must appear, "" means the
// pronoun must abstain. The antecedent error count must be zero.
func TestCloneCorefFixtureAntecedentErrors(t *testing.T) {
	cases := []struct {
		name    string
		facts   []string
		pronoun string
		want    string
	}{
		{"service then pronoun", []string{"grafana-210 listens on port 3000"}, "It runs on db02.prod", "grafana-210 runs on db02.prod"},
		{"person then pronoun", []string{"Priya N. Schmidt prefers dark mode"}, "They prefer tabs", "Priya N. Schmidt prefer tabs"},
		{"short service name", []string{"pgbouncer-512 runs on db01.prod", "pgbouncer-512 listens on port 6432"}, "It is pinned to version 2.1.0", "pgbouncer-512 is pinned to version 2.1.0"},
		{"ambiguous short form", []string{"Ines A. Novak works in UTC", "Ines B. Novak works in CET", "Ines prefers tabs"}, "It owns the invoice archive", ""},
		{"no antecedent", nil, "It runs on db09.prod", ""},
	}
	errs := 0
	for _, c := range cases {
		st := newCloneNoteState()
		for _, f := range c.facts {
			st.candidates(f)
		}
		got := cloneTexts(st.candidates(c.pronoun))
		ok := (c.want == "" && len(got) == 0) || (c.want != "" && containsText(got, c.want))
		if !ok {
			errs++
			t.Logf("%s: want %q, got %q", c.name, c.want, got)
		}
	}
	t.Logf("coreference fixture: %d cases, %d antecedent errors", len(cases), errs)
	if errs != 0 {
		t.Errorf("%d coreference fixture errors", errs)
	}
}

func TestCloneGroundingRefusesIntroducedEntityOrNumber(t *testing.T) {
	fact := "Dana K. Berg owns the release checklist"
	cases := map[string]bool{
		"Which holding does Dana K. Berg have?":                   true,
		"Dana K. Berg owns the release checklist for Marco Silva": false,
		"Dana K. Berg owns the release checklist, version 4":      false,
		"":   false,
		fact: false,
	}
	for para, want := range cases {
		if got := cloneGrounded(fact, para); got != want {
			t.Errorf("cloneGrounded(%q) = %v, want %v", para, got, want)
		}
	}
	port := "pgbouncer-512 listens on port 6432"
	if !cloneGrounded(port, "What port number does pgbouncer-512 have? 6432") {
		t.Error("a number present in the fact was refused")
	}
	if cloneGrounded(port, "What port number does pgbouncer-512 have? 8080") {
		t.Error("a number absent from the fact was accepted")
	}
}

func cloneCount(t *testing.T, ix *Index, where string) int {
	t.Helper()
	n, err := ix.DB.Count("SELECT COUNT(*) FROM memory_clone_alias WHERE " + where)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCloneFlagOffWritesNothing(t *testing.T) {
	restore := exp.Set()
	defer restore()
	ix := testIndex(t)
	memNote(t, ix, "memory/facts.md",
		entry("a", "2026-08-14 09:00", "Dana K. Berg owns the release checklist"))
	n, err := ix.DB.Count("SELECT COUNT(*) FROM sqlite_master WHERE name='memory_clone_alias'")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("flag off created the clone table")
	}
}

// The alias is found by the ranking, is never shown, and is attached only to
// the fact it came from: a same-first-name fact gains nothing from it.
func TestCloneAliasRaisesOwnFactOnly(t *testing.T) {
	const query = "Which holding does Dana M. Okafor have?"
	facts := []memory.Entry{
		entry("berg", "2026-08-14 09:00", "Dana K. Berg owns the release checklist"),
		entry("okafor", "2026-08-14 10:00", "Dana M. Okafor owns the indexer list"),
		entry("filler", "2026-08-14 11:00", "the cat is named marmalade"),
	}

	restoreOff := exp.Set()
	off := testIndex(t)
	memNote(t, off, "memory/facts.md", facts...)
	offHits, err := off.MemoryEntries(MemoryQuery{Query: query, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	restoreOff()

	restore := exp.Set(cloneFlag)
	defer restore()
	on := testIndex(t)
	memNote(t, on, "memory/facts.md", facts...)
	onHits, err := on.MemoryEntries(MemoryQuery{Query: query, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if n := cloneCount(t, on, "kept=1"); n == 0 {
		t.Fatal("flag on kept no aliases")
	}
	for _, h := range onHits {
		if h.Text != "Dana K. Berg owns the release checklist" && h.Text != "Dana M. Okafor owns the indexer list" && h.Text != "the cat is named marmalade" {
			t.Errorf("an alias leaked into a hit text: %q", h.Text)
		}
	}
	// The Okafor fact may only move through its own alias.
	offO, onO := aliasHit(t, offHits, "okafor"), aliasHit(t, onHits, "okafor")
	if onO.Semantic < offO.Semantic || onO.Keyword < offO.Keyword {
		t.Error("alias made a component score lower; it must only raise")
	}
	if rows := cloneCount(t, on, "id='okafor' AND alias LIKE '%Berg%'"); rows != 0 {
		t.Errorf("Berg's alias attached to Okafor's fact: %d rows", rows)
	}
	if rows := cloneCount(t, on, "id='berg' AND alias LIKE '%Berg%'"); rows == 0 {
		t.Error("Berg's own alias is missing")
	}
	// The markdown on disk is the same with the flag on.
	b, err := os.ReadFile(filepath.Join(on.Vault.Root, "memory", "facts.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "holding:") || strings.Contains(string(b), "Which holding") {
		t.Error("an alias was written into the markdown")
	}
}

// rankOfGold is the 1-based rank of a query's gold fact in the real ranking,
// or 0 when it is not in the answer.
func rankOfGold(t *testing.T, ix *Index, q cloneSelQuery) int {
	t.Helper()
	hits, err := ix.MemoryEntries(MemoryQuery{Query: q.Q, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for i, h := range hits {
		if h.ID == q.ID {
			return i + 1
		}
	}
	return 0
}

// The selection keeps an alias only when it helps its own selection query and
// lowers no other selection query's gold rank. The check is end to end: after
// the selection, the real ranking must not lower any gold rank, and the kept
// alias for the activated fact must improve that fact's own rank.
func TestCloneSelectionKeepsOnlyHelpfulAliases(t *testing.T) {
	sel := []cloneSelQuery{
		{Note: "memory/facts.md", ID: "berg", Q: "Which holding does Dana K. Berg have?"},
		{Note: "memory/facts.md", ID: "rival", Q: "Which holding does the release team have on file"},
	}
	facts := []memory.Entry{
		entry("berg", "2026-08-14 09:00", "Dana K. Berg owns the release checklist"),
		entry("rival", "2026-08-14 10:00", "Which holding does the release team have on file"),
		entry("filler", "2026-08-14 11:00", "the cat is named marmalade"),
	}

	restoreOff := exp.Set()
	off := testIndex(t)
	memNote(t, off, "memory/facts.md", facts...)
	base := make([]int, len(sel))
	for i, q := range sel {
		base[i] = rankOfGold(t, off, q)
	}
	restoreOff()

	restore := exp.Set(cloneFlag, cloneN7Flag)
	defer restore()
	dir := t.TempDir()
	path := filepath.Join(dir, "selection.json")
	raw, err := json.Marshal(sel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(cloneSelEnv, path)
	ix := testIndex(t)
	ix.cloneActive = map[string]bool{"berg": true, "rival": true}
	memNote(t, ix, "memory/facts.md", facts...)
	if n := cloneCount(t, ix, "kept=0"); n == 0 {
		t.Fatal("no candidates were generated for the activated facts")
	}
	if err := ix.cloneSelect(); err != nil {
		t.Fatal(err)
	}
	if n := cloneCount(t, ix, "kept=0"); n != 0 {
		t.Errorf("%d rejected candidates left in the table", n)
	}
	if n := cloneCount(t, ix, "id='filler'"); n != 0 {
		t.Error("a fact outside the activation set got aliases")
	}
	for i, q := range sel {
		got := rankOfGold(t, ix, q)
		if base[i] != 0 && got > base[i] {
			t.Errorf("selection query for %s lowered its gold rank: %d -> %d", q.ID, base[i], got)
		}
	}
	kept := cloneCount(t, ix, "kept=1 AND id='berg'")
	if kept > 0 {
		if got := rankOfGold(t, ix, sel[0]); got >= base[0] {
			t.Errorf("kept berg alias did not improve its own query: %d -> %d", base[0], got)
		}
	}
	if kept > cloneCapFact {
		t.Errorf("kept %d aliases for one fact, cap is %d", kept, cloneCapFact)
	}
}
