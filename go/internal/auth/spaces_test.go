package auth

import "testing"

func TestSpaceOfMatchesTheLongestPrefix(t *testing.T) {
	s := testStore(t)
	if _, err := s.CreateSpace("Engineering", "team/eng", KindShared, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSpace("Team", "team", KindShared, ""); err != nil {
		t.Fatal(err)
	}
	spaces, err := s.Spaces()
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := s.SpaceByPrefix("team/eng/")
	team, _ := s.SpaceByPrefix("team/")

	for path, want := range map[string]string{
		"team/eng/runbook.md": eng.ID,
		"team/handbook.md":    team.ID,
		"notes/personal.md":   CommonsID,
		"team-adjacent.md":    CommonsID, // a prefix is a path segment, not a string
	} {
		if got := SpaceOf(path, spaces); got != want {
			t.Errorf("SpaceOf(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestPrefixesAreNormalizedAndUnique(t *testing.T) {
	s := testStore(t)
	if _, err := s.CreateSpace("A", "/team/eng/", KindShared, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSpace("B", "team/eng", KindShared, ""); err != ErrPrefixTaken {
		t.Fatalf("a duplicate prefix in another form was accepted: %v", err)
	}
	for _, bad := range []string{"", "/", "../escape", "team/../..", "."} {
		if _, err := s.CreateSpace("bad", bad, KindShared, ""); err != ErrBadPrefix {
			t.Errorf("prefix %q accepted: %v", bad, err)
		}
	}
}

// The property the whole model exists for: one member cannot read another's
// personal space, and can read what is shared with them.
func TestPrincipalSeesOnlyItsOwnSpaces(t *testing.T) {
	s := testStore(t)
	admin, _ := s.Create("admin", "", "correct horse battery", RoleAdmin)
	alice, _ := s.Create("alice", "Alice", "correct horse battery", RoleMember)
	bob, _ := s.Create("bob", "Bob", "correct horse battery", RoleMember)

	aliceSpace, err := s.EnsurePersonalSpace(alice)
	if err != nil {
		t.Fatal(err)
	}
	bobSpace, err := s.EnsurePersonalSpace(bob)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := s.CreateSpace("Engineering", "team/eng", KindShared, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(shared.ID, alice.ID, SpaceReader); err != nil {
		t.Fatal(err)
	}

	ap, err := s.PrincipalFor(alice)
	if err != nil {
		t.Fatal(err)
	}
	bp, err := s.PrincipalFor(bob)
	if err != nil {
		t.Fatal(err)
	}

	if !ap.CanRead(aliceSpace.ID) || !ap.CanWrite(aliceSpace.ID) {
		t.Error("alice cannot use her own space")
	}
	if ap.CanRead(bobSpace.ID) {
		t.Error("alice can read bob's personal space")
	}
	if bp.CanRead(aliceSpace.ID) {
		t.Error("bob can read alice's personal space")
	}
	if !ap.CanRead(shared.ID) {
		t.Error("alice cannot read the space she was added to")
	}
	if ap.CanWrite(shared.ID) {
		t.Error("a reader can write")
	}
	if bp.CanRead(shared.ID) {
		t.Error("bob can read a space he is not a member of")
	}
	// the commons is the shared root everyone works in
	if !ap.CanRead(CommonsID) || !bp.CanWrite(CommonsID) {
		t.Error("the commons is not shared")
	}

	// An administrator reads everything, deliberately.
	adp, err := s.PrincipalFor(admin)
	if err != nil {
		t.Fatal(err)
	}
	if !adp.CanRead(aliceSpace.ID) || !adp.CanRead(bobSpace.ID) || !adp.IsAdmin() {
		t.Error("an administrator cannot see the spaces it administers")
	}

	// And an unauthenticated caller reads nothing at all.
	an := Anonymous()
	if an.CanRead(CommonsID) || an.CanRead(shared.ID) || an.IsAdmin() {
		t.Error("an anonymous caller has access")
	}
}

func TestPersonalSpaceIsIdempotent(t *testing.T) {
	s := testStore(t)
	u, _ := s.Create("alice", "Alice", "correct horse battery", RoleAdmin)
	a, err := s.EnsurePersonalSpace(u)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.EnsurePersonalSpace(u)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatal("a second personal space was created for the same account")
	}
	if a.Prefix != PersonalPrefix+"alice/" {
		t.Fatalf("personal prefix = %q", a.Prefix)
	}
}
