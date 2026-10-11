package api

import "testing"

// A path that climbs out of its space must be checked, and read, under the one
// name the vault will open, never under the raw string.
func TestNormPathCleansAndRejectsTraversal(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/notes/a":            "notes/a.md",
		"notes/a.md":          "notes/a.md",
		"public/../private/x": "private/x.md",
		"./notes//a":          "notes/a.md",
		"../etc/passwd":       "",
		"a/../../b":           "",
		"..":                  "",
		"   ":                 "",
	}
	for in, want := range cases {
		if got := normPath(in); got != want {
			t.Errorf("normPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// A fetched URL is evidence for that URL, not for a shorter one it begins with.
func TestContainsURLMatchesWholeURLs(t *testing.T) {
	t.Parallel()
	raw := "pulled https://x.com/a/b, and (https://y.org/docs). Also https://z.net."
	for _, u := range []string{"https://x.com/a/b", "https://y.org/docs", "https://z.net"} {
		if !containsURL(raw, u) {
			t.Errorf("containsURL(%q) = false, want true", u)
		}
	}
	for _, u := range []string{"https://x.com", "https://x.com/a", "https://y.org/doc", "https://z.ne", ""} {
		if containsURL(raw, u) {
			t.Errorf("containsURL(%q) = true, want false", u)
		}
	}
}
