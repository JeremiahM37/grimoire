package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

func providerTestVault(t *testing.T) *secrets.Vault {
	t.Helper()
	v := secrets.New(t.TempDir())
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := `#!/bin/sh
[ "$BW_SESSION" = "SESS-OK" ] || { echo "Vault is locked." >&2; exit 1; }
case "$1 $2" in
"list folders") echo '[{"id":"f1","name":"agents"}]' ;;
"list items") echo '[{"id":"i1","name":"GitHub Token","folderId":"f1"},{"id":"i2","name":"Stripe","folderId":"f1"}]' ;;
"get password") printf 'pw-%s' "$3" ;;
esac`
	if err := os.WriteFile(filepath.Join(dir, "bw"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GRIMOIRE_TEST_BW_SESSION", "SESS-OK")
	return v
}

func TestProviderAddLinkImportFromTheCLI(t *testing.T) {
	v := providerTestVault(t)
	if rc := cmdSecretProvider(v, []string{"add", "bw", "--kind", "bitwarden",
		"--secret-env", "session=GRIMOIRE_TEST_BW_SESSION"}); rc != 0 {
		t.Fatalf("add rc=%d", rc)
	}
	if rc := cmdSecretProvider(v, []string{"test", "bw"}); rc != 0 {
		t.Fatalf("test rc=%d", rc)
	}
	if rc := cmdSecretLink(v, []string{"gh", "bitwarden://i1/password"}); rc != 0 {
		t.Fatalf("link rc=%d", rc)
	}
	if got, err := v.Get("gh"); err != nil || got != "pw-i1" {
		t.Fatalf("%q %v", got, err)
	}
	if rc := importFromProvider(v, []string{"--from", "bitwarden", "--folder", "agents", "--item", "i2", "--yes"}); rc != 0 {
		t.Fatalf("import rc=%d", rc)
	}
	if got, err := v.Get("bw/stripe"); err != nil || got != "pw-i2" {
		t.Fatalf("imported value %q %v", got, err)
	}
	if v.LinkOf("bw/stripe") != nil {
		t.Fatal("an imported item is a stored secret, not a link")
	}
	// the other item was not selected
	if _, err := v.Get("bw/github-token"); err == nil {
		t.Fatal("unselected item was imported")
	}
	if rc := cmdSecretUnlink(v, []string{"gh"}); rc != 0 {
		t.Fatalf("unlink rc=%d", rc)
	}
}

func TestProviderAddRejectsBadInput(t *testing.T) {
	v := providerTestVault(t)
	for name, args := range map[string][]string{
		"no kind":      {"add", "x"},
		"unknown kind": {"add", "x", "--kind", "lastpass"},
		"typo setting": {"add", "x", "--kind", "pass", "--set", "storedir=/x"},
		"unset env":    {"add", "x", "--kind", "bitwarden", "--secret-env", "session=GRIMOIRE_NOPE_UNSET"},
	} {
		if rc := cmdSecretProvider(v, args); rc == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	if !strings.Contains(importSlug("My Key (prod)"), "my-key--prod") {
		t.Errorf("slug = %q", importSlug("My Key (prod)"))
	}
}
