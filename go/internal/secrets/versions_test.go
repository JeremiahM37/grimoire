package secrets

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The operation a person most needs to undo is a rotation, and it was the one
// that could not be undone. These are mostly about that.

// openVault returns an initialized, unlocked vault. testVault deliberately
// leaves it uninitialized so the lifecycle tests can drive that themselves.
func openVault(t *testing.T) *Vault {
	t.Helper()
	v, _ := testVault(t)
	if err := v.Initialize(testPassphrase); err != nil {
		t.Fatal(err)
	}
	return v
}

const testPassphrase = "correct horse battery"

// frozenVault opens a vault with the clock already stopped.
//
// The clock has to be frozen BEFORE the vault exists: unlocking stamps a
// last-activity time from Now, and a vault created at the real time and then
// shown a clock two hours later idle-locks itself out from under the test.
func frozenVault(t *testing.T, when time.Time) *Vault {
	t.Helper()
	old := Now
	Now = func() time.Time { return when }
	t.Cleanup(func() { Now = old })
	return openVault(t)
}

func TestRotationCanBeUndone(t *testing.T) {
	v := openVault(t)
	if err := v.PutVersioned("stripe", "sk_live_OLD", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := v.PutVersioned("stripe", "sk_live_NEW", nil, "quarterly rotation"); err != nil {
		t.Fatal(err)
	}
	got, _ := v.Get("stripe")
	if got != "sk_live_NEW" {
		t.Fatalf("current = %q, want the new value", got)
	}

	// The new key turns out not to work. Before this, the old one was gone.
	vers, err := v.Versions("stripe")
	if err != nil {
		t.Fatal(err)
	}
	if len(vers) != 1 {
		t.Fatalf("%d versions retained, want 1", len(vers))
	}
	if vers[0].Note != "quarterly rotation" {
		t.Errorf("note = %q; history without a reason is a list of blobs", vers[0].Note)
	}
	if err := v.Restore("stripe", 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := v.Get("stripe"); got != "sk_live_OLD" {
		t.Fatalf("after restore = %q, want the old value back", got)
	}
}

// Rolling back must not destroy what it rolled back from, or a mistaken
// rollback is unrecoverable in the other direction.
func TestARollbackIsItselfUndoable(t *testing.T) {
	v := openVault(t)
	v.PutVersioned("k", "v1", nil, "")
	v.PutVersioned("k", "v2", nil, "")
	if err := v.Restore("k", 0); err != nil { // back to v1
		t.Fatal(err)
	}
	vers, _ := v.Versions("k")
	if len(vers) != 2 {
		t.Fatalf("%d versions after a restore, want 2 — the rollback must be undoable too", len(vers))
	}
	if err := v.Restore("k", 0); err != nil { // back to v2
		t.Fatal(err)
	}
	if got, _ := v.Get("k"); got != "v2" {
		t.Errorf("undoing the rollback gave %q, want v2", got)
	}
}

// A listing is a browsing operation. Returning every old value to answer "how
// many are there" would hand out more than reading the current one does.
func TestHistoryNeverReturnsOldValues(t *testing.T) {
	v := openVault(t)
	v.PutVersioned("k", "the-old-secret", nil, "")
	v.PutVersioned("k", "the-new-secret", nil, "")
	vers, err := v.Versions("k")
	if err != nil {
		t.Fatal(err)
	}
	for _, ver := range vers {
		if ver.Value != "" {
			t.Fatalf("history exposed a value: %q", ver.Value)
		}
	}
	if vers[0].At == "" {
		t.Error("a version with no timestamp cannot be chosen between")
	}
}

func TestHistoryIsBounded(t *testing.T) {
	v := openVault(t)
	for i := 0; i < MaxVersions+8; i++ {
		if err := v.PutVersioned("k", string(rune('a'+i)), nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	vers, _ := v.Versions("k")
	if len(vers) != MaxVersions {
		t.Fatalf("%d versions retained, want the cap of %d — the payload is "+
			"decrypted whole on every unlock", len(vers), MaxVersions)
	}
}

// A sync or a retried request re-puts the same value. Counting that as history
// would push a genuinely older value out of a bounded list.
func TestRewritingTheSameValueIsNotAVersion(t *testing.T) {
	v := openVault(t)
	v.PutVersioned("k", "same", nil, "")
	for i := 0; i < 5; i++ {
		v.PutVersioned("k", "same", nil, "")
	}
	vers, _ := v.Versions("k")
	if len(vers) != 0 {
		t.Errorf("%d versions from rewriting an identical value, want 0", len(vers))
	}
}

// A rotation that supplies only the new value must not drop the expiry and the
// description somebody set months ago.
func TestMetadataSurvivesARotation(t *testing.T) {
	v := openVault(t)
	v.PutVersioned("k", "v1", map[string]any{
		MetaNote: "billing API", MetaExpires: "2027-01-01"}, "")
	v.PutVersioned("k", "v2", nil, "rotated")

	all, err := v.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 secret, got %d", len(all))
	}
	if all[0].Note != "billing API" || all[0].Expires != "2027-01-01" {
		t.Errorf("metadata lost on rotation: %+v", all[0])
	}
}

func TestMetadataCanBeExplicitlyCleared(t *testing.T) {
	v := openVault(t)
	v.PutVersioned("k", "v1", map[string]any{MetaExpires: "2027-01-01"}, "")
	v.PutVersioned("k", "v1", map[string]any{MetaExpires: nil}, "")
	all, _ := v.Describe()
	if all[0].Expires != "" {
		t.Errorf("expiry = %q, want cleared — carrying metadata forward must "+
			"not make it impossible to remove", all[0].Expires)
	}
}

// ------------------------------------------------------------- expiry

func TestExpiryIsClassifiedNotJustStored(t *testing.T) {
	v := frozenVault(t, time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))

	v.PutVersioned("dead", "x", map[string]any{MetaExpires: "2026-08-01"}, "")
	v.PutVersioned("soon", "x", map[string]any{MetaExpires: "2026-09-05"}, "")
	v.PutVersioned("fine", "x", map[string]any{MetaExpires: "2027-08-01"}, "")
	v.PutVersioned("none", "x", nil, "")

	byName := map[string]Info{}
	all, err := v.Describe()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range all {
		byName[i.Name] = i
	}
	for name, want := range map[string]string{
		"dead": StatusExpired, "soon": StatusExpiring,
		"fine": StatusOK, "none": StatusOK,
	} {
		if got := byName[name].Status; got != want {
			t.Errorf("%s status = %q, want %q", name, got, want)
		}
	}
	if d := byName["dead"].ExpiresInDays; d == nil || *d >= 0 {
		t.Errorf("expired secret reports %v days, want negative", d)
	}
}

// "2026-11-30" is what a provider dashboard shows. Refusing it would make the
// feature annoying enough to skip.
func TestABareDateIsAcceptedAsAnExpiry(t *testing.T) {
	for _, s := range []string{"2026-11-30", "2026-11-30T10:00:00Z", "2026-11-30T10:00:00"} {
		if _, err := parseDate(s); err != nil {
			t.Errorf("parseDate(%q) failed: %v", s, err)
		}
	}
	if _, err := parseDate("next tuesday"); err == nil {
		t.Error("an unparseable date must be reported, not silently treated as no expiry")
	}
}

func TestARotationReminderNeverHidesARealExpiry(t *testing.T) {
	v := frozenVault(t, time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))

	// Both stale AND expired. A dead credential is the worse fact.
	v.PutVersioned("k", "x", map[string]any{
		MetaExpires: "2026-08-01", MetaRotateDays: 1}, "")
	all, _ := v.Describe()
	if all[0].Status != StatusExpired {
		t.Errorf("status = %q, want expired — reporting the milder of two "+
			"problems hides the worse one", all[0].Status)
	}
}

func TestNeedsAttentionListsOnlyWhatNeedsIt(t *testing.T) {
	v := frozenVault(t, time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))

	v.PutVersioned("dead", "x", map[string]any{MetaExpires: "2026-01-01"}, "")
	v.PutVersioned("fine", "x", nil, "")
	need, err := v.NeedsAttention()
	if err != nil {
		t.Fatal(err)
	}
	if len(need) != 1 || need[0].Name != "dead" {
		t.Fatalf("needs attention = %+v, want only the expired one", need)
	}
}

// ------------------------------------------------------------- use tracking

func TestUseIsCountedSoAnUnusedCredentialIsVisible(t *testing.T) {
	v := openVault(t)
	v.PutVersioned("used", "x", nil, "")
	v.PutVersioned("never", "x", nil, "")
	v.MarkUsed("used")
	v.MarkUsed("used")

	byName := map[string]Info{}
	all, _ := v.Describe()
	for _, i := range all {
		byName[i.Name] = i
	}
	if byName["used"].Uses != 2 {
		t.Errorf("uses = %d, want 2", byName["used"].Uses)
	}
	if byName["used"].LastUsed == "" {
		t.Error("a used credential has no last-used time")
	}
	// The distinction that matters: never used is not the same as unused.
	if byName["never"].Uses != 0 || byName["never"].LastUsed != "" {
		t.Errorf("an unused secret reports use: %+v", byName["never"])
	}
}

func TestMarkUsedOnAMissingOrLockedSecretIsSilent(t *testing.T) {
	v := openVault(t)
	v.MarkUsed("nope") // must not panic
	v.Lock()
	v.MarkUsed("nope") // locked: also must not panic
}

// ------------------------------------------------------------- safety

func TestVersionsAndRestoreRefuseWhileLocked(t *testing.T) {
	v := openVault(t)
	v.PutVersioned("k", "v1", nil, "")
	v.PutVersioned("k", "v2", nil, "")
	v.Lock()
	if _, err := v.Versions("k"); err != ErrLocked {
		t.Errorf("Versions while locked = %v, want ErrLocked", err)
	}
	if err := v.Restore("k", 0); err != ErrLocked {
		t.Errorf("Restore while locked = %v, want ErrLocked", err)
	}
	if _, err := v.Describe(); err != ErrLocked {
		t.Errorf("Describe while locked = %v, want ErrLocked", err)
	}
}

func TestRestoringAVersionThatIsNotThereSaysSo(t *testing.T) {
	v := openVault(t)
	v.PutVersioned("k", "v1", nil, "")
	err := v.Restore("k", 4)
	if err == nil || !strings.Contains(err.Error(), "no version") {
		t.Errorf("restore of a missing version = %v, want a clear error", err)
	}
	if err := v.Restore("nope", 0); err == nil {
		t.Error("restoring a secret that does not exist must fail")
	}
}

func TestHistorySurvivesLockAndUnlock(t *testing.T) {
	v := openVault(t)
	v.PutVersioned("k", "v1", nil, "first")
	v.PutVersioned("k", "v2", nil, "second")
	v.Lock()
	if err := v.Unlock(testPassphrase); err != nil {
		t.Fatal(err)
	}
	vers, err := v.Versions("k")
	if err != nil {
		t.Fatal(err)
	}
	if len(vers) != 1 || vers[0].Note != "second" {
		t.Fatalf("history did not survive a lock cycle: %+v", vers)
	}
}

// The count has to come from the broker, not from a test calling MarkUsed.
// "Which of these credentials is anything actually using" is only answerable
// if the real path records it.
func TestABrokeredCallRecordsTheUse(t *testing.T) {
	v, b := testVault(t)
	if err := v.Initialize(testPassphrase); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := v.PutVersioned("api", "the-value", nil, ""); err != nil {
		t.Fatal(err)
	}
	token, err := b.Grant(GrantSpec{Secret: "api", Grantee: "agent", Scope: srv.URL, TTLSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Use(token, "GET", srv.URL, "Authorization", ""); err != nil {
		t.Fatal(err)
	}

	all, err := v.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if all[0].Uses != 1 {
		t.Errorf("uses = %d after a brokered call, want 1", all[0].Uses)
	}
	if all[0].LastUsed == "" {
		t.Error("last-used was not recorded by the broker path")
	}
}

// A far-side failure still put the credential on the wire.
func TestAFailedCallStillCountsAsUse(t *testing.T) {
	v, b := testVault(t)
	if err := v.Initialize(testPassphrase); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	v.PutVersioned("api", "the-value", nil, "")
	token, _ := b.Grant(GrantSpec{Secret: "api", Grantee: "agent", Scope: srv.URL, TTLSeconds: 300})
	b.Use(token, "GET", srv.URL, "Authorization", "")

	all, _ := v.Describe()
	if all[0].Uses != 1 {
		t.Errorf("uses = %d; a 500 from the far side still used the key", all[0].Uses)
	}
}

// ------------------------------------------------------------- prefixes

func TestPrefixIsTheNamePartBeforeTheLastSlash(t *testing.T) {
	for name, want := range map[string]string{
		"stripe":         "",
		"prod/stripe":    "prod",
		"prod/eu/stripe": "prod/eu",
		"/leading":       "",
		"trailing/":      "trailing",
	} {
		if got := Prefix(name); got != want {
			t.Errorf("Prefix(%q) = %q, want %q", name, got, want)
		}
	}
}

// The bug this exists to prevent: a command scoped to one namespace reaching
// into another whose name merely starts with the same letters.
func TestAPrefixMatchesWholeSegmentsOnly(t *testing.T) {
	if HasPrefix("production/key", "prod") {
		t.Error(`"prod" matched "production/key" — a prefix compared as a string ` +
			`hands one namespace's secrets to a command scoped to another`)
	}
	if !HasPrefix("prod/key", "prod") {
		t.Error(`"prod" did not match "prod/key"`)
	}
	if !HasPrefix("prod/key", "prod/") {
		t.Error("a trailing slash on the prefix should not change the answer")
	}
	if !HasPrefix("prod", "prod") {
		t.Error("a secret named exactly the prefix is under it")
	}
	if !HasPrefix("anything", "") {
		t.Error("an empty prefix means everything")
	}
	if !HasPrefix("prod/eu/key", "prod") {
		t.Error("a nested name is under its ancestor")
	}
}

func TestUnderSelectsANamespace(t *testing.T) {
	v := openVault(t)
	for _, n := range []string{"prod/stripe", "prod/github", "dev/stripe", "toplevel"} {
		if err := v.PutVersioned(n, "x", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := v.Under("prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d secrets under prod, want 2: %+v", len(got), got)
	}
	all, _ := v.Under("")
	if len(all) != 4 {
		t.Errorf("%d secrets for an empty prefix, want all 4", len(all))
	}
}

// Derived from names, so there is no empty folder to clean up and no way for
// the list to disagree with reality.
func TestPrefixesAreDerivedFromNames(t *testing.T) {
	v := openVault(t)
	for _, n := range []string{"prod/a", "prod/b", "dev/a", "flat"} {
		v.PutVersioned(n, "x", nil, "")
	}
	got, err := v.Prefixes()
	if err != nil {
		t.Fatal(err)
	}
	if got["prod"] != 2 || got["dev"] != 1 {
		t.Errorf("prefixes = %v, want prod:2 dev:1", got)
	}
	if _, ok := got[""]; ok {
		t.Error("a top-level secret invented an empty-named namespace")
	}

	// Deleting the last secret in a namespace removes the namespace, because
	// there was never a folder object to leave behind.
	v.Delete("dev/a")
	got, _ = v.Prefixes()
	if _, ok := got["dev"]; ok {
		t.Error("an empty namespace survived its last secret")
	}
}
