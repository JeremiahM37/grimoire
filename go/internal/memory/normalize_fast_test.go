package memory

import (
	"math/rand"
	"testing"
)

func TestNormalizeMatchesRegexpDefinition(t *testing.T) {
	cases := []string{"", "  ", "Hello, World!", "port 9111/tcp — v1.16.2", "ÉCOLE  naïve\tcafé\n",
		"a b", "x\vy", "日本語のテキスト。です", "éclair", "--a--", "MiXeD_case-words", "١٢٣ arabic digits"}
	alphabet := []rune("aZ09 \t\n\r\f\v.,;:!?-_/\\'\"()[]{}<>@#$%^&*+=~`|  éß日€́١")
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		n := r.Intn(40)
		b := make([]rune, n)
		for j := range b {
			b[j] = alphabet[r.Intn(len(alphabet))]
		}
		cases = append(cases, string(b))
	}
	for _, c := range cases {
		if got, want := Normalize(c), normalizeRegexp(c); got != want {
			t.Fatalf("Normalize(%q) = %q, want %q", c, got, want)
		}
	}
}
