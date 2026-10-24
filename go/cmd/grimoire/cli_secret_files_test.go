package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFlattenYAMLReadsNestedCredentialFiles(t *testing.T) {
	got := flattenYAML(`# header
unifi:
  api_key: "abc:def"   # inline comment
  host: https://192.0.2.1
  nested:
    token: 'it''s'
plain: value
empty:
list:
  - skipped
block: |
  skipped too
`)
	want := map[string]string{
		"unifi.api_key":      "abc:def",
		"unifi.host":         "https://192.0.2.1",
		"unifi.nested.token": "it's",
		"plain":              "value",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestFlattenJSONAndDotenv(t *testing.T) {
	dir := t.TempDir()
	j := filepath.Join(dir, "k.json")
	os.WriteFile(j, []byte(`{"a":{"b":"x","n":3},"c":""}`), 0o600)
	got, err := flattenSecretFile(j)
	if err != nil || !reflect.DeepEqual(got, map[string]string{"a.b": "x", "a.n": "3"}) {
		t.Fatalf("json: %v %v", got, err)
	}
	e := filepath.Join(dir, "k.env")
	os.WriteFile(e, []byte("# c\nexport A=\"1\"\nB=two\nbad\n"), 0o600)
	got, _ = flattenSecretFile(e)
	if !reflect.DeepEqual(got, map[string]string{"A": "1", "B": "two"}) {
		t.Fatalf("env: %v", got)
	}
}

func TestFileSecretsComeFromTheIndex(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GRIMOIRE_VAULTED_FILES", filepath.Join(dir, "idx.json"))
	src := filepath.Join(dir, "api.yaml")
	os.WriteFile(src, []byte("svc:\n  key: v\n"), 0o600)
	if _, err := fileSecrets(src); err == nil {
		t.Fatal("a file never imported must be refused, with the import command")
	}
	if err := saveVaulted(map[string]vaultedFile{src: {Prefix: "files/api", Vars: map[string]string{"SVC_KEY": "files/api/svc.key"}}}); err != nil {
		t.Fatal(err)
	}
	got, err := fileSecrets(src)
	if err != nil || !reflect.DeepEqual(got, []string{"files/api/svc.key=SVC_KEY"}) {
		t.Fatalf("got %v %v", got, err)
	}
	link := filepath.Join(dir, "link.yaml")
	os.Symlink(src, link)
	if got, err := fileSecrets(link); err != nil || len(got) != 1 {
		t.Fatalf("a symlink to an imported file should resolve: %v %v", got, err)
	}
}
