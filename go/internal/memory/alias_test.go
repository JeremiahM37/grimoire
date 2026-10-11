package memory

import (
	"reflect"
	"testing"
)

func TestFullMentionsNamesPeopleWithAnInitial(t *testing.T) {
	ms := FullMentions("Priya N. Schmidt works in UTC, and Dana B Mori owns the backups")
	if len(ms) != 2 {
		t.Fatalf("mentions = %+v, want two", ms)
	}
	if ms[0].Canon != "priya n schmidt" || ms[0].Kind != "person" {
		t.Errorf("first = %+v", ms[0])
	}
	if want := []string{"priya", "priya schmidt"}; !reflect.DeepEqual(ms[0].Keys, want) {
		t.Errorf("keys = %v, want %v", ms[0].Keys, want)
	}
	if ms[1].Canon != "dana b mori" {
		t.Errorf("second = %+v", ms[1])
	}
}

func TestFullMentionsNeedAnInitialForAPerson(t *testing.T) {
	// No initial means no canonical name: "Priya Schmidt" is a short form to
	// be resolved, not a full entity. This is the negative case for the rule.
	if ms := FullMentions("Priya Schmidt owns the release checklist"); len(ms) != 0 {
		t.Errorf("mentions = %+v, want none", ms)
	}
}

func TestFullMentionsSkipsSentenceStartWords(t *testing.T) {
	if ms := FullMentions("The A. Foo rotation is weekly"); len(ms) != 0 {
		t.Errorf("mentions = %+v, want none", ms)
	}
}

func TestFullMentionsNamesServicesAndHosts(t *testing.T) {
	ms := FullMentions("auth-gateway-03 listens on port 8443; db01.prod runs postgres; pgbouncer-523 is pinned")
	got := map[string][]string{}
	for _, m := range ms {
		got[m.Canon] = m.Keys
	}
	want := map[string][]string{
		"auth-gateway-03": {"auth-gateway", "auth"},
		"db01.prod":       {"db01"},
		"pgbouncer-523":   {"pgbouncer"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("services = %v, want %v", got, want)
	}
}

func TestAliasCandidatesIncludeWordsPartsAndPairs(t *testing.T) {
	got := AliasCandidates("Priya Schmidt is on auth-gateway")
	for _, k := range []string{"priya", "schmidt", "priya schmidt", "auth-gateway", "auth", "gateway"} {
		if !hasKey(got, k) {
			t.Errorf("candidates %v missing %q", got, k)
		}
	}
	if hasKey(got, "is") || hasKey(got, "on") {
		t.Errorf("stopwords leaked into candidates: %v", got)
	}
}

func TestAliasCandidatesAreCapped(t *testing.T) {
	var text string
	for i := 0; i < 3*MaxAliasKeysPerFact; i++ {
		text += " word" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
	}
	if n := len(AliasCandidates(text)); n > MaxAliasKeysPerFact {
		t.Errorf("candidates = %d, want at most %d", n, MaxAliasKeysPerFact)
	}
}

func hasKey(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func fact(note, id, session, stamp string, canons ...string) FactRef {
	t, _ := StampTime(stamp)
	return FactRef{Note: note, ID: id, Session: session, Stamp: t, Canons: canons}
}

func ev(alias, canon, note, id, session, stamp string, canons ...string) Evidence {
	t, _ := StampTime(stamp)
	return Evidence{Alias: alias, Canon: canon, Note: note, ID: id, Session: session, Stamp: t, Canons: canons}
}

func TestSessionScopeLinksTheSameConversation(t *testing.T) {
	r := NewAliasResolver("priya", []Evidence{
		ev("priya", "priya n schmidt", "memory/a.md", "e1", "s1", "2026-08-14 09:00", "priya n schmidt"),
		ev("priya", "priya a sharma", "memory/b.md", "e2", "s2", "2026-08-14 09:00", "priya a sharma"),
	})
	link, out := r.Resolve(fact("memory/c.md", "x", "s1", "2026-09-01 09:00"))
	if out != Linked || link.Entity != "priya n schmidt" || link.Scope != ScopeSession {
		t.Fatalf("got %+v %v, want session link to priya n schmidt", link, out)
	}
	if link.Conf != 1.0 {
		t.Errorf("conf = %v, want 1.0", link.Conf)
	}
}

func TestSessionScopeAllowsLaterEvidenceInTheSameSession(t *testing.T) {
	// Within one conversation the order of the mentions is not evidence.
	r := NewAliasResolver("priya", []Evidence{
		ev("priya", "priya n schmidt", "memory/a.md", "e1", "s1", "2026-09-05 09:00", "priya n schmidt"),
	})
	if _, out := r.Resolve(fact("memory/a.md", "x", "s1", "2026-09-01 09:00")); out != Linked {
		t.Fatalf("outcome = %v, want Linked", out)
	}
}

func TestAmbiguousSessionIsNotGuessed(t *testing.T) {
	// Two people with the same first name in one conversation: the session
	// names two candidates, so nothing is linked even though the note around
	// it names only one.
	r := NewAliasResolver("priya", []Evidence{
		ev("priya", "priya n schmidt", "memory/a.md", "e1", "s1", "2026-08-14 09:00", "priya n schmidt"),
		ev("priya", "priya a sharma", "memory/a.md", "e2", "s1", "2026-08-14 09:01", "priya a sharma"),
		ev("priya", "priya n schmidt", "memory/a.md", "e3", "s9", "2026-08-14 10:00", "priya n schmidt"),
	})
	link, out := r.Resolve(fact("memory/a.md", "x", "s1", "2026-08-14 11:00"))
	if out != Ambiguous || link != (Link{}) {
		t.Fatalf("got %+v %v, want Ambiguous with no link", link, out)
	}
}

func TestNoteScopeNeedsTheTimeWindow(t *testing.T) {
	r := NewAliasResolver("dana", []Evidence{
		ev("dana", "dana b mori", "memory/notes.md", "e1", "", "2026-08-01 09:00", "dana b mori"),
	})
	near := fact("memory/notes.md", "x", "", "2026-08-10 09:00")
	if link, out := r.Resolve(near); out != Linked || link.Scope != ScopeNote || link.Conf != 0.8 {
		t.Fatalf("near: got %+v %v, want note link at 0.8", link, out)
	}
	// Four months later the note evidence no longer counts. The unique scope
	// may still link, because it is the only evidence there is; what must not
	// happen is a note-scope link.
	far := fact("memory/notes.md", "y", "", "2026-12-01 09:00")
	if link, _ := r.Resolve(far); link.Scope == ScopeNote {
		t.Fatalf("far: note scope linked across %v: %+v", NoteWindow, link)
	}
}

func TestNoteScopeBeatsComentionAndUnique(t *testing.T) {
	r := NewAliasResolver("dana", []Evidence{
		ev("dana", "dana b mori", "memory/notes.md", "e1", "", "2026-08-01 09:00", "dana b mori"),
		ev("dana", "dana k lee", "memory/other.md", "e2", "", "2026-07-01 09:00", "dana k lee", "ops rotation"),
	})
	f := fact("memory/notes.md", "x", "", "2026-08-02 09:00", "ops rotation")
	link, out := r.Resolve(f)
	if out != Linked || link.Entity != "dana b mori" || link.Scope != ScopeNote {
		t.Fatalf("got %+v %v, want note scope to decide", link, out)
	}
}

func TestComentionScopeUsesAnotherFullNameInTheFact(t *testing.T) {
	// The fact names "ops rotation" in full; another fact names it alongside
	// dana k lee, so the alias points there rather than at dana b mori.
	r := NewAliasResolver("dana", []Evidence{
		ev("dana", "dana b mori", "memory/a.md", "e1", "", "2026-08-01 09:00", "dana b mori"),
		ev("dana", "dana k lee", "memory/b.md", "e2", "", "2026-08-02 09:00", "dana k lee", "ops rotation"),
	})
	f := fact("memory/c.md", "x", "", "2026-08-20 09:00", "ops rotation")
	link, out := r.Resolve(f)
	if out != Linked || link.Entity != "dana k lee" || link.Scope != ScopeComention || link.Conf != 0.7 {
		t.Fatalf("got %+v %v, want comention link to dana k lee", link, out)
	}
}

func TestUniqueScopeNeedsOnlyOneCandidateEver(t *testing.T) {
	r := NewAliasResolver("ilya", []Evidence{
		ev("ilya", "ilya k novak", "memory/a.md", "e1", "s1", "2026-08-01 09:00", "ilya k novak"),
	})
	link, out := r.Resolve(fact("memory/z.md", "x", "", "2026-08-20 09:00"))
	if out != Linked || link.Scope != ScopeUnique || link.Conf != 0.5 {
		t.Fatalf("got %+v %v, want unique link at 0.5", link, out)
	}
}

func TestLaterFullNameInAnotherSessionDoesNotRelinkAnEarlierFact(t *testing.T) {
	// The guard. The alias fact is in s1 on Aug 1. A full name for the same
	// alias appears in s2 on Aug 20. The earlier fact must not change meaning:
	// nothing in s1 names the entity and the only evidence is later.
	r := NewAliasResolver("ilya", []Evidence{
		ev("ilya", "ilya k novak", "memory/later.md", "e1", "s2", "2026-08-20 09:00", "ilya k novak"),
	})
	earlier := fact("memory/early.md", "x", "s1", "2026-08-01 09:00")
	if link, out := r.Resolve(earlier); out != NoEvidence || link != (Link{}) {
		t.Fatalf("got %+v %v, want no link for the earlier fact", link, out)
	}
	// And the same fact, written after that evidence, does link.
	later := fact("memory/late.md", "y", "s3", "2026-08-25 09:00")
	if _, out := r.Resolve(later); out != Linked {
		t.Fatalf("later fact outcome = %v, want Linked", out)
	}
}

func TestComentionEvidenceAlsoRespectsTime(t *testing.T) {
	r := NewAliasResolver("dana", []Evidence{
		ev("dana", "dana b mori", "memory/a.md", "e1", "", "2026-08-01 09:00", "dana b mori"),
		ev("dana", "dana k lee", "memory/b.md", "e2", "", "2026-08-30 09:00", "dana k lee", "ops rotation"),
	})
	f := fact("memory/c.md", "x", "", "2026-08-20 09:00", "ops rotation")
	link, out := r.Resolve(f)
	// The co-mention that would pick dana k lee is after f, so it cannot
	// count. dana k lee's first evidence is also after f, so at the time of f
	// the only known "dana" is dana b mori, and unique scope links to it.
	if out != Linked || link.Entity != "dana b mori" || link.Scope != ScopeUnique {
		t.Fatalf("got %+v %v, want unique link to dana b mori", link, out)
	}
}

func TestResolverIgnoresOtherAliasesEvidence(t *testing.T) {
	r := NewAliasResolver("priya", []Evidence{
		ev("dana", "dana b mori", "memory/a.md", "e1", "s1", "2026-08-01 09:00", "dana b mori"),
	})
	if _, out := r.Resolve(fact("memory/a.md", "x", "s1", "2026-08-02 09:00")); out != NoEvidence {
		t.Fatalf("outcome = %v, want NoEvidence", out)
	}
}

func TestLinkScoreStaysBelowAFullNameMatch(t *testing.T) {
	for _, conf := range scopeConf {
		if s := LinkScore(conf); s >= 1 || s <= 0 {
			t.Errorf("LinkScore(%v) = %v, want strictly between 0 and 1", conf, s)
		}
	}
}
