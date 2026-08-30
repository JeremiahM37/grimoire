package secrets

import (
	"strings"
	"testing"
)

// Two properties decide whether this is worth shipping: it finds real keys,
// and it stays quiet on notes. The second is the harder one — a scanner people
// turn off is worse than none, because afterwards its silence means "not run".

func kinds(fs []Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Kind)
	}
	return strings.Join(out, ",")
}

// Fixtures are ASSEMBLED rather than written as literals.
//
// A credential-shaped string in a source file is a credential-shaped string:
// GitHub's push protection blocks it, every other scanner flags it, and a
// project whose own feature is finding pasted keys should not be the one
// committing them. Joining the parts at runtime keeps the test exercising the
// real issuer shape — the value the scanner sees is byte-identical — while the
// repository contains no line anybody has to allowlist.
func fixture(parts ...string) string { return strings.Join(parts, "") }

func TestIssuerShapedKeysAreFound(t *testing.T) {
	// Every fixture is a syntactically valid key of the issuer's real shape and
	// length; a fixture that is merely key-ish would let a pattern that never
	// matches anything real pass.
	alnum := "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	cases := map[string]string{
		"AWS access key": "aws key " + fixture("AKIA", "IOSFODNN7EXAMPLE") + " in the runbook",
		"GitHub token":   "token " + fixture("ghp", "_", alnum) + " works",
		"Slack token":    fixture("xoxb", "-", "123456789012", "-", "1234567890123", "-", alnum[:24]),
		"Stripe key":     "STRIPE=" + fixture("sk", "_live_", alnum[:24]),
		"Anthropic key":  fixture("sk", "-ant-", "api03-", alnum),
		"Google API key": fixture("AIza", "SyA1B2c3D4e5F6g7H8i9J0k1L2m3N4o5P6q"),
		"private key":    "-----BEGIN RSA PRIVATE KEY-----",
		"JSON Web Token": fixture("eyJ", "hbGciOiJIUzI1NiJ9", ".", "eyJ", "zdWIiOiIxMjM0NTY3ODkwIn0",
			".", "dBjftJeZ4CVPmB92K27uhbUJU1p1r"),
		"npm token":    fixture("npm", "_", alnum),
		"SendGrid key": fixture("SG", ".", alnum[:22], ".", alnum[:30]),
		"Twilio SID":   fixture("AC", "0123456789abcdef0123456789abcdef"),
	}
	for want, text := range cases {
		got := ScanText("n.md", text)
		if len(got) == 0 {
			t.Errorf("%s: nothing found in %q", want, text)
			continue
		}
		if got[0].Kind != want {
			t.Errorf("found %q, want %q (in %q)", kinds(got), want, text)
		}
		if got[0].Confidence != ConfidenceHigh {
			t.Errorf("%s reported as %s; an issuer-defined shape is not a guess",
				want, got[0].Confidence)
		}
	}
}

// The report must not reproduce the leak in a terminal, a log and a ticket.
func TestAFindingNeverContainsTheCredential(t *testing.T) {
	secret := fixture("ghp", "_", "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789")
	got := ScanText("n.md", "my token is "+secret)
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %d", len(got))
	}
	if strings.Contains(got[0].Masked, secret) {
		t.Fatal("the finding contains the whole credential")
	}
	// Enough to identify which key, not enough to use it.
	if !strings.HasPrefix(got[0].Masked, "ghp_") || !strings.Contains(got[0].Masked, "•") {
		t.Errorf("masked = %q, want an identifiable but unusable fragment", got[0].Masked)
	}
	if len(got[0].Masked) >= len(secret) {
		t.Errorf("masked value is not shorter than the secret")
	}
}

// A short value gives too much away if any of it is shown.
func TestShortValuesAreMaskedEntirely(t *testing.T) {
	if m := Mask("abc123"); strings.ContainsAny(m, "abc123") {
		t.Errorf("Mask of a short value = %q, want it fully hidden", m)
	}
}

// The whole reason the entropy gate exists.
func TestOrdinaryNotesProduceNothing(t *testing.T) {
	clean := []string{
		"# Deploy runbook\n\nThe API key lives in the vault, not here.",
		"password: changeme",
		"api_key = TODO",
		"token: your_api_key_here",
		"Set AWS_SECRET_ACCESS_KEY from the environment before running.",
		"The secret to good pastry is cold butter and not overworking it.",
		"auth: none",
		"See https://example.com/docs/authentication for the token flow.",
		"| password | the one from 1Password |",
		"secret = \"\"",
		"My access_key is stored in the credential vault under `aws-prod`.",
		"- [ ] rotate the stripe key\n- [ ] update the runbook",
		"git commit 4f2b8c1e9a0d3f5b7c2e8a1d4f6b9c0e3a5d7f2b",
	}
	for _, text := range clean {
		if got := ScanText("n.md", text); len(got) > 0 {
			t.Errorf("false positive %q on: %q", kinds(got), text)
		}
	}
}

// A hex commit hash is one character class and very common in notes.
func TestHexHashesAreNotReportedAsSecrets(t *testing.T) {
	text := "deployed at commit a3f5b8c2d1e4f7a9b0c3d6e8f1a4b7c0d3e6f9a2"
	if got := ScanText("n.md", text); len(got) > 0 {
		t.Errorf("a commit hash was reported as %q", kinds(got))
	}
}

// The generic rule has to fire when the value really is a key.
func TestAHighEntropyAssignmentIsReported(t *testing.T) {
	text := `api_key = "Zx9Qw3Rt7Yu1Iop5Asd8Fgh2Jkl4Zxc6Vbn0"`
	got := ScanText("n.md", text)
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %d (%s)", len(got), kinds(got))
	}
	if got[0].Confidence != ConfidenceMedium {
		t.Errorf("confidence = %s; without an issuer shape this is a guess and "+
			"must say so", got[0].Confidence)
	}
}

// Grimoire seals note bodies itself. Ciphertext is maximally high-entropy, so
// reporting it would be both wrong and extremely loud.
func TestTheVaultsOwnCiphertextIsNotAFinding(t *testing.T) {
	text := EncPrefix + "Zx9Qw3Rt7Yu1Iop5Asd8Fgh2Jkl4Zxc6Vbn0AbCdEfGhIjKlMnOpQrStUvWx"
	if got := ScanText("n.md", text); len(got) > 0 {
		t.Errorf("the vault's own encrypted body was reported as %q", kinds(got))
	}
}

func TestFindingsCarryWhereAndWhatToDo(t *testing.T) {
	text := "line one\nline two\nAKIAIOSFODNN7EXAMPLE\n"
	got := ScanText("notes/ops.md", text)
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %d", len(got))
	}
	if got[0].Line != 3 {
		t.Errorf("line = %d, want 3", got[0].Line)
	}
	if got[0].Path != "notes/ops.md" {
		t.Errorf("path = %q", got[0].Path)
	}
	if got[0].Advice == "" {
		t.Error("a finding nobody can act on is noise")
	}
}

// The same key on the same line matched by two rules is one problem.
func TestTheSameValueIsNotReportedTwice(t *testing.T) {
	text := `api_key = "` + fixture("sk", "-ant-", "api03-", "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789") + `"`
	got := ScanText("n.md", text)
	if len(got) != 1 {
		t.Fatalf("got %d findings (%s), want 1 — one value is one problem", len(got), kinds(got))
	}
	if got[0].Confidence != ConfidenceHigh {
		t.Errorf("the issuer-shaped match must win over the generic one, got %s", got[0].Confidence)
	}
}

func TestEntropyRanksRandomAboveProse(t *testing.T) {
	if entropy("aaaaaaaaaaaaaaaa") > 1 {
		t.Error("a repeated character should have near-zero entropy")
	}
	if entropy("Zx9Qw3Rt7Yu1Iop5") < minEntropy {
		t.Error("a random-looking key fell below the bar")
	}
	if !looksRandom("Zx9Qw3Rt7Yu1Iop5Asd8") {
		t.Error("a mixed-class random string was not treated as random")
	}
	if looksRandom("thequickbrownfoxjumpsover") {
		t.Error("lowercase prose was treated as random")
	}
}
