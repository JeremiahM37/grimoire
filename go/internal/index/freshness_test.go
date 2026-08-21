package index

import (
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

var refNow = time.Date(2026, 8, 21, 12, 0, 0, 0, time.Local)

func TestAnExplicitVerifiedDateBeatsTheFileTime(t *testing.T) {
	// The whole reason the key exists: a note edited yesterday to fix a typo
	// has not been re-checked, and a note confirmed last week has, whatever
	// its modification time says.
	yesterday := float64(refNow.Add(-24 * time.Hour).Unix())
	f := Freshness(refNow.Add(-30*24*time.Hour).Format("2006-01-02"), yesterday)
	days, explicit := f.AgeDays(refNow)
	if !explicit {
		t.Fatal("a verified date was ignored in favour of the file time")
	}
	if days < 29 || days > 31 {
		t.Errorf("age = %d days, want ~30", days)
	}
}

func TestWithNoVerifiedDateTheFileTimeIsUsed(t *testing.T) {
	f := Freshness("", float64(refNow.Add(-10*24*time.Hour).Unix()))
	days, explicit := f.AgeDays(refNow)
	if explicit {
		t.Error("reported an explicit confirmation that nobody made")
	}
	if days != 10 {
		t.Errorf("age = %d, want 10", days)
	}
}

func TestAFutureVerifiedDateIsIgnored(t *testing.T) {
	// "2027-08-21" for 2026 is the classic typo. Honouring it would make the
	// one note somebody fat-fingered permanently the freshest in the vault —
	// and permanently absent from the review queue.
	f := Freshness("2027-08-21", float64(refNow.Add(-400*24*time.Hour).Unix()))
	days, explicit := f.AgeDays(refNow)
	if explicit {
		t.Error("a future date was accepted as a confirmation")
	}
	if days < 399 {
		t.Errorf("age = %d, want the real file age", days)
	}
}

func TestVerifiedDateFormatsAPersonActuallyTypes(t *testing.T) {
	for _, s := range []string{
		"2026-08-01", "2026-08-01 09:30", "2026-08-01T09:30:00Z", "2026-08",
	} {
		if !ValidVerifiedDate(s) {
			t.Errorf("ValidVerifiedDate(%q) = false", s)
		}
	}
	for _, s := range []string{"", "last tuesday", "01/08/2026", "yesterday"} {
		if ValidVerifiedDate(s) {
			t.Errorf("ValidVerifiedDate(%q) = true", s)
		}
	}
}

func TestStalenessScoreWeightsByDependenceWithoutLettingHubsDominate(t *testing.T) {
	// A note twelve others point at, a year old, should outrank a note nobody
	// links to that is slightly older…
	hub := StalenessScore(365, 12)
	orphan := StalenessScore(400, 0)
	if hub <= orphan {
		t.Errorf("hub %v did not outrank a slightly older orphan %v", hub, orphan)
	}
	// …but a 200-backlink index page that is barely overdue must not bury a
	// genuinely rotten note, which is what a linear weight would do.
	barelyOverdue := StalenessScore(181, 200)
	rotten := StalenessScore(900, 3)
	if barelyOverdue >= rotten {
		t.Errorf("a barely-overdue hub (%v) outranked a rotten note (%v) — the "+
			"link weight is drowning out age", barelyOverdue, rotten)
	}
	if StalenessScore(0, 100) != 0 {
		t.Error("a note confirmed today has a nonzero staleness score")
	}
}

func TestFreshnessTravelsWithARetrievalHit(t *testing.T) {
	ix := testIndex(t)
	write(t, ix, "runbook.md", "# Runbook\n\nthe kestrel deploy procedure")
	if _, err := ix.Upsert("runbook.md"); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.Retrieve("kestrel deploy", 5, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	// A note written this second is not stale, and reports an age rather than
	// leaving the caller to infer one.
	if hits[0].Stale {
		t.Error("a note written just now is reported stale")
	}
	if hits[0].AgeDays != 0 {
		t.Errorf("age = %d for a note written now", hits[0].AgeDays)
	}
}

func TestAnOldNoteIsFlaggedStaleInRetrieval(t *testing.T) {
	ix := testIndex(t)
	writeOrigin(t, ix, "old.md", "", "", "# Old\n\nthe kestrel deploy procedure")
	// Backdate through the frontmatter key, which is the supported way to say
	// when a note was last confirmed — reaching into the file's mtime would
	// test the filesystem rather than the feature.
	writeVerified(t, ix, "old.md", "2020-01-01", "# Old\n\nthe kestrel deploy procedure")

	hits, err := ix.Retrieve("kestrel deploy", 5, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if !hits[0].Stale {
		t.Errorf("a note last confirmed in 2020 is not flagged stale: %+v", hits[0])
	}
	if !hits[0].Verified {
		t.Error("an explicit verified date was not reported as explicit")
	}
	if hits[0].AgeDays < 365 {
		t.Errorf("age = %d days", hits[0].AgeDays)
	}
}

func TestStalenessIsReportedNotRanked(t *testing.T) {
	// Down-ranking old notes would bury a vault's most considered writing
	// under its most recent. The signal is exposed; the ordering is not
	// touched.
	ix := testIndex(t)
	writeVerified(t, ix, "ancient.md", "2019-01-01",
		"# Ancient\n\nkestrel kestrel kestrel deployment deployment")
	writeVerified(t, ix, "fresh.md", "2026-08-20", "# Fresh\n\nkestrel mentioned once")

	hits, err := ix.Retrieve("kestrel deployment", 5, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("got %v", paths(hits))
	}
	if hits[0].Path != "ancient.md" {
		t.Errorf("ranking = %v — the better lexical match was demoted for being "+
			"old, which this feature deliberately does not do", paths(hits))
	}
}

// writeVerified writes a note carrying a `verified:` date.
func writeVerified(t *testing.T, ix *Index, rel, date, body string) {
	t.Helper()
	fm := markdown.NewFrontmatter()
	fm.Set("verified", date)
	if _, err := ix.Vault.Write(rel, body, fm); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Upsert(rel); err != nil {
		t.Fatal(err)
	}
}

func TestConfirmingANoteIsVisibleWithoutARebuild(t *testing.T) {
	// Found by the e2e suite, not by the unit tests above: they wrote both
	// notes before the first retrieval, so the cache was BUILT with the
	// frontmatter already in place. A running server has a warm cache and
	// takes the incremental patch path instead, where every per-note field has
	// to be refreshed — and `verified` was not, so a note confirmed through
	// the console kept reporting the age it had before.
	ix := testIndex(t)
	write(t, ix, "runbook.md", "# Runbook\n\nthe kestrel deploy procedure")
	if _, err := ix.Upsert("runbook.md"); err != nil {
		t.Fatal(err)
	}
	// Warm the cache, which is what makes the next write a patch.
	if _, err := ix.Retrieve("kestrel deploy", 5, true); err != nil {
		t.Fatal(err)
	}

	writeVerified(t, ix, "runbook.md", "2019-01-01", "# Runbook\n\nthe kestrel deploy procedure")
	hits, err := ix.Retrieve("kestrel deploy", 5, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if !hits[0].Stale || !hits[0].Verified {
		t.Errorf("a note back-dated on a warm cache reports %+v — the patch path "+
			"is not refreshing freshness", hits[0])
	}
}

func TestPromotingANoteIsVisibleWithoutARebuild(t *testing.T) {
	// The same hazard for the other per-note field.
	ix := testIndex(t)
	writeOrigin(t, ix, "pulled.md", "connector:slack:C1", "", "# Pulled\n\nkestrel notes")
	if _, err := ix.Retrieve("kestrel", 5, true); err != nil {
		t.Fatal(err)
	}
	writeOrigin(t, ix, "pulled.md", "connector:slack:C1", "trusted", "# Pulled\n\nkestrel notes")

	hits, err := ix.RetrieveFor("kestrel", 5, Filter{IncludePrivate: true, TrustedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("a promoted note is invisible to a trusted-only caller on a warm cache: %v",
			paths(hits))
	}
}
