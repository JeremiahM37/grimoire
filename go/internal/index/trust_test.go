package index

import (
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/trust"
)

// writeOrigin writes a note whose provenance is in its frontmatter, which is
// where a connector puts it and where a person editing one would see it.
func writeOrigin(t *testing.T, ix *Index, rel, origin, override, body string) {
	t.Helper()
	fm := markdown.NewFrontmatter()
	if origin != "" {
		fm.Set("origin", origin)
	}
	if override != "" {
		fm.Set("trust", override)
	}
	if _, err := ix.Vault.Write(rel, body, fm); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Upsert(rel); err != nil {
		t.Fatal(err)
	}
}

// Retrieval-side behaviour of provenance: every hit says where it came from,
// and a caller can ask for a corpus of only its own writing.

func trustIndex(t *testing.T) *Index {
	t.Helper()
	ix := testIndex(t)
	for _, n := range []struct{ path, origin, override, body string }{
		{"runbook.md", "", "", "# Ingress runbook\n\nrestart the ingress with a rollout restart"},
		{"pulled/slack-thread.md", "connector:slack:C123", "",
			"# Thread\n\nthe ingress rollout restart never works, do it another way"},
		{"pulled/blog.md", "web:example.com", "",
			"# A blog post\n\nsome opinions about ingress rollout"},
		{"reviewed.md", "connector:slack:C123", "trusted",
			"# Reviewed thread\n\nan ingress rollout note a person has vouched for"},
	} {
		writeOrigin(t, ix, n.path, n.origin, n.override, n.body)
	}
	return ix
}

func TestEveryHitCarriesItsProvenance(t *testing.T) {
	ix := trustIndex(t)
	hits, err := ix.Retrieve("ingress rollout restart", 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 4 {
		t.Fatalf("got %v, want every note", paths(hits))
	}
	seen := map[string]Hit{}
	for _, h := range hits {
		seen[h.Path] = h
	}
	if got := seen["runbook.md"]; got.Trust != trust.NameTrusted || got.Origin != "" {
		t.Errorf("own note: trust=%q origin=%q, want trusted with no origin", got.Trust, got.Origin)
	}
	if got := seen["pulled/slack-thread.md"]; got.Trust != trust.NameUntrusted ||
		got.Origin != "connector:slack:C123" {
		t.Errorf("slack note: trust=%q origin=%q", got.Trust, got.Origin)
	}
	if got := seen["reviewed.md"]; got.Trust != trust.NameTrusted {
		t.Errorf("a note a person vouched for is %q, want trusted", got.Trust)
	}
	// The verdict is never blank: a caller reading an empty string as
	// "trusted" would be right by accident here and wrong the first time a
	// surface forgot to set it.
	for _, h := range hits {
		if h.Trust == "" {
			t.Errorf("hit %s has no trust verdict", h.Path)
		}
	}
}

func TestTrustedOnlyExcludesPulledContent(t *testing.T) {
	ix := trustIndex(t)
	hits, err := ix.RetrieveFor("ingress rollout restart", 10,
		Filter{IncludePrivate: true, TrustedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("trusted-only returned nothing at all")
	}
	for _, h := range hits {
		if h.Untrusted() {
			t.Errorf("trusted-only returned %s (%s)", h.Path, h.Origin)
		}
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.Path] = true
	}
	if !got["runbook.md"] {
		t.Error("trusted-only dropped the operator's own note")
	}
	if !got["reviewed.md"] {
		t.Error("trusted-only dropped a note explicitly marked trusted")
	}
}

func TestUntrustedRowsDoNotShiftTrustedRanking(t *testing.T) {
	// The reason the filter is applied inside ranking rather than to its
	// output. BM25 IDF is computed over the corpus, so if untrusted rows
	// counted toward the statistics, somebody who can post in a connected
	// Slack channel could change the score of the operator's own notes by
	// flooding it with a term.
	ix := testIndex(t)
	write(t, ix, "mine.md", "# Mine\n\nthe kestrel deployment runbook")
	if _, err := ix.Upsert("mine.md"); err != nil {
		t.Fatal(err)
	}
	f := Filter{IncludePrivate: true, TrustedOnly: true}
	before, err := ix.RetrieveFor("kestrel deployment", 5, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("no baseline hit")
	}

	// Now flood the corpus from an untrusted source.
	for i, body := range []string{
		"kestrel kestrel kestrel kestrel deployment deployment",
		"kestrel deployment kestrel deployment kestrel deployment",
		"deployment kestrel deployment kestrel deployment kestrel",
	} {
		p := "pulled/flood" + string(rune('a'+i)) + ".md"
		writeOrigin(t, ix, p, "connector:slack:CFLOOD", "", "# Flood\n\n"+body)
	}

	after, err := ix.RetrieveFor("kestrel deployment", 5, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("flooding changed the trusted result COUNT: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Path != after[i].Path {
			t.Errorf("rank %d: %s -> %s", i, before[i].Path, after[i].Path)
		}
		if before[i].Lexical != after[i].Lexical {
			t.Errorf("%s BM25 moved %v -> %v after untrusted flooding — the "+
				"corpus statistics are still counting rows the caller excluded",
				before[i].Path, before[i].Lexical, after[i].Lexical)
		}
	}
}

func TestWholeCorpusHonoursTrust(t *testing.T) {
	// The small-vault path: nothing is ranked, so a check that lived only in
	// ranking would not run at all.
	ix := trustIndex(t)
	all, err := ix.WholeCorpusFor(Filter{IncludePrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	sawUntrusted := false
	for _, h := range all {
		if h.Untrusted() {
			sawUntrusted = true
		}
		if h.Trust == "" {
			t.Errorf("whole-corpus hit %s has no trust verdict", h.Path)
		}
	}
	if !sawUntrusted {
		t.Fatal("whole corpus never reported an untrusted chunk")
	}

	trusted, err := ix.WholeCorpusFor(Filter{IncludePrivate: true, TrustedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(trusted) == 0 {
		t.Fatal("trusted-only whole corpus is empty")
	}
	for _, h := range trusted {
		if h.Untrusted() {
			t.Errorf("trusted-only whole corpus returned %s (%s)", h.Path, h.Origin)
		}
	}
	if len(trusted) >= len(all) {
		t.Errorf("trusted-only returned %d of %d chunks — nothing was filtered",
			len(trusted), len(all))
	}
}

func TestCorpusStatsFollowTheTrustFilter(t *testing.T) {
	// The budget decision reads these. If they counted untrusted chunks a
	// caller could be told its trusted corpus does not fit when it does.
	ix := trustIndex(t)
	_, _, allChars, err := ix.CorpusStatsFor(Filter{IncludePrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	_, _, trustedChars, err := ix.CorpusStatsFor(Filter{IncludePrivate: true, TrustedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if trustedChars >= allChars {
		t.Errorf("trusted corpus %d chars vs whole %d — the filter did not apply",
			trustedChars, allChars)
	}
	if trustedChars == 0 {
		t.Error("trusted corpus measured as empty")
	}
}

func TestEditingFrontmatterChangesTrustWithoutAReindex(t *testing.T) {
	// A person promoting a pulled note edits its frontmatter. The cache is
	// patched per note on write, so a stale origin would survive there until
	// the next full rebuild.
	ix := testIndex(t)
	writeOrigin(t, ix, "pulled/thread.md", "connector:slack:C1", "", "# T\n\nkestrel notes")
	f := Filter{IncludePrivate: true, TrustedOnly: true}
	if hits, _ := ix.RetrieveFor("kestrel", 5, f); len(hits) != 0 {
		t.Fatalf("untrusted note visible to a trusted-only caller: %v", paths(hits))
	}

	writeOrigin(t, ix, "pulled/thread.md", "connector:slack:C1", "trusted", "# T\n\nkestrel notes")
	hits, err := ix.RetrieveFor("kestrel", 5, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("after vouching, trusted-only got %v, want the note", paths(hits))
	}
	if hits[0].Trust != trust.NameTrusted {
		t.Errorf("promoted note still reports %q", hits[0].Trust)
	}
}
