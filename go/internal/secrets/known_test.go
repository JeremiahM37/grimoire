package secrets

import (
	"strings"
	"testing"
)

func TestRedactKnownReplacesStoredValuesInAnyPosition(t *testing.T) {
	v := New(t.TempDir())
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if err := v.Put("TAILSCALE_OAUTH", "oauth-k9Qz3abcdEFGH-value", nil); err != nil {
		t.Fatal(err)
	}
	if err := v.Put("ROUTER_PASSWORD", "hunter2hunter2", nil); err != nil {
		t.Fatal(err)
	}
	if err := v.Put("PORT", "8080", nil); err != nil {
		t.Fatal(err)
	}
	text := "curl -d \"client_secret=oauth-k9Qz3abcdEFGH-value\" and ssh admin:hunter2hunter2@router, port 8080."
	got, n := v.RedactKnown(text)
	if n != 2 || strings.Contains(got, "k9Qz3") || strings.Contains(got, "hunter2hunter2") {
		t.Fatalf("got %d %q", n, got)
	}
	if !strings.Contains(got, "[vault secret TAILSCALE_OAUTH]") || !strings.Contains(got, "[vault secret ROUTER_PASSWORD]") {
		t.Fatalf("markers missing: %q", got)
	}
	if !strings.Contains(got, "port 8080") {
		t.Fatalf("short values must not be redacted: %q", got)
	}
}

func TestRedactKnownCoversOldValuesAndSurvivesLock(t *testing.T) {
	dir := t.TempDir()
	v := New(dir)
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if err := v.PutVersioned("API", "first-value-123456", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := v.PutVersioned("API", "second-value-123456", nil, "rotated"); err != nil {
		t.Fatal(err)
	}
	v.Lock()
	got, n := v.RedactKnown("old first-value-123456 new second-value-123456")
	if n != 2 {
		t.Fatalf("after lock: %d %q", n, got)
	}
	// A new process knows nothing until it is unlocked.
	fresh := New(dir)
	if _, n := fresh.RedactKnown("second-value-123456"); n != 0 {
		t.Fatal("a locked fresh vault cannot know values")
	}
	if err := fresh.Unlock("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if _, n := fresh.RedactKnown("second-value-123456"); n != 1 {
		t.Fatal("unlock must load the fingerprints")
	}
	if err := fresh.Delete("API"); err != nil {
		t.Fatal(err)
	}
	if _, n := fresh.RedactKnown("second-value-123456"); n != 0 {
		t.Fatal("a deleted secret is forgotten")
	}
}

// A host, user name or URL imported from a secret file is config: redacting
// it turned a machine's name into a vault marker in every note that named the
// machine. The file's credentials are still redacted.
func TestRedactKnownSkipsConfigValues(t *testing.T) {
	v := New(t.TempDir())
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"files/homelab-access.yaml.lxc_104.host":     "buildhost01",
		"files/homelab-access.yaml.lxc_104.password": "s3cretpassw0rd",
		"files/unifi.yaml.base_url":                  "https://unifi.example.internal",
	} {
		if err := v.Put(name, value, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Put("files/router.yaml.admin_name", "routeradmin", map[string]any{MetaNote: ConfigNotePrefix + "imported"}); err != nil {
		t.Fatal(err)
	}
	got, _ := v.RedactKnown("ssh buildhost01 as routeradmin with s3cretpassw0rd via https://unifi.example.internal")
	if strings.Contains(got, "s3cretpassw0rd") {
		t.Fatalf("the credential leaked: %q", got)
	}
	for _, keep := range []string{"buildhost01", "routeradmin", "https://unifi.example.internal"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("config value %q was redacted: %q", keep, got)
		}
	}
}
