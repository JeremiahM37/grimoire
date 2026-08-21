package secrets

import (
	"errors"
	"strings"
	"testing"
)

type stubProv struct {
	note        string
	alsoTrusted bool
	err         error
	asked       []string
}

func (s *stubProv) UntrustedMention(t string) (string, bool, error) {
	s.asked = append(s.asked, t)
	return s.note, s.alsoTrusted, s.err
}

func TestGatedCoversWritesAndNotReads(t *testing.T) {
	for _, m := range []string{"POST", "put", "PATCH", "delete"} {
		if !Gated(m) {
			t.Errorf("%s should be gated", m)
		}
	}
	// "" is what the broker turns into GET; gating it would gate every
	// default call, which is the opposite of the intent.
	for _, m := range []string{"GET", "head", ""} {
		if Gated(m) {
			t.Errorf("%s should not be gated", m)
		}
	}
}

func TestNormalizeTargetDropsQueryAndCredentials(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"https://api.example.com/v1/items?token=abc#frag", "https://api.example.com/v1/items"},
		{"https://u:p@api.example.com/v1", "https://api.example.com/v1"},
		{"https://api.example.com/v1/", "https://api.example.com/v1"},
		{"not a url", "not a url"},
	} {
		if got := normalizeTarget(c.in); got != c.want {
			t.Errorf("normalizeTarget(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The gate must not fire on the paths that were working before it existed.
func TestGateIsSilentWhenItShouldBe(t *testing.T) {
	tainted := &stubProv{note: "clipped/evil.md"}
	for _, c := range []struct {
		name   string
		broker *Broker
		method string
	}{
		{"no checker configured", &Broker{}, "POST"},
		{"read method", &Broker{Provenance: tainted}, "GET"},
		{"defaulted method", &Broker{Provenance: tainted}, ""},
		{"checker errors", &Broker{Provenance: &stubProv{err: errors.New("db gone")}}, "POST"},
		{"nothing untrusted mentions it", &Broker{Provenance: &stubProv{}}, "POST"},
		{"a trusted note names it too", &Broker{Provenance: &stubProv{
			note: "clipped/evil.md", alsoTrusted: true}}, "POST"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.broker.checkProvenance(Grant{Secret: "gh"}, c.method,
				"https://api.example.com/v1/items"); err != nil {
				t.Fatalf("gate fired when it should not have: %v", err)
			}
		})
	}
}

func TestGateRefusesAWriteToAPlantedURL(t *testing.T) {
	p := &stubProv{note: "clipped/attacker-page.md"}
	b := &Broker{Provenance: p}
	err := b.checkProvenance(Grant{Secret: "gh"}, "POST",
		"https://api.example.com/v1/exfil?data=secret")
	if !errors.Is(err, ErrUntrustedTarget) {
		t.Fatalf("want ErrUntrustedTarget, got %v", err)
	}
	// The refusal has to name the note, or a person cannot act on it.
	if !strings.Contains(err.Error(), "clipped/attacker-page.md") {
		t.Errorf("refusal does not name the note: %v", err)
	}
	// It is asked about the normalized URL, not the raw one: the query string
	// carries the payload and would never match prose.
	if len(p.asked) != 1 || p.asked[0] != "https://api.example.com/v1/exfil" {
		t.Errorf("checker asked about %q", p.asked)
	}
}
