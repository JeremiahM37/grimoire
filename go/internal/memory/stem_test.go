package memory

import "testing"

func TestStemMeetsCommonVariants(t *testing.T) {
	t.Parallel()
	same := [][2]string{
		{"linter", "lint"}, {"migrations", "migration"}, {"deploying", "deploy"},
		{"deployed", "deploy"}, {"configures", "configure"}, {"tests", "tests"},
		{"queries", "query"}, {"releases", "release"},
	}
	for _, p := range same {
		if Stem(p[0]) != Stem(p[1]) && p[0] != p[1] {
			t.Errorf("Stem(%q)=%q should meet Stem(%q)=%q", p[0], Stem(p[0]), p[1], Stem(p[1]))
		}
	}
}

func TestStemLeavesShortAndDistinctWordsAlone(t *testing.T) {
	t.Parallel()
	for _, w := range []string{"port", "bus", "dns", "go", "key", "api"} {
		if Stem(w) != w {
			t.Errorf("Stem(%q)=%q, short words must be unchanged", w, Stem(w))
		}
	}
	if Stem("deploy") == Stem("destroy") {
		t.Error("distinct words must not collapse")
	}
}
