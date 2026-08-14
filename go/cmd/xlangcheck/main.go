// Command xlangcheck emits artifacts produced by the GO implementation, for
// Python to verify.
//
// The fixtures in compat/fixtures/ prove one direction: Go reproduces what
// Python froze. This proves the other: Python accepts what Go produces. Both
// directions matter for a drop-in replacement — a vault written by the Go build
// has to stay readable if the user rolls back, and vice versa.
//
// Usage:  go run ./cmd/xlangcheck | .venv/bin/python compat/verify_go.py
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/JeremiahM37/grimoire/go/internal/crdt"
	"github.com/JeremiahM37/grimoire/go/internal/crypto"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

type output struct {
	Crypto []cryptoCase `json:"crypto"`
	CRDT   []crdtCase   `json:"crdt"`
	Notes  []noteCase   `json:"notes"`
}

type cryptoCase struct {
	Passphrase string `json:"passphrase"`
	KDF        string `json:"kdf"`
	SaltB64    string `json:"salt_b64"`
	Plaintext  string `json:"plaintext"`
	Token      string `json:"token"`
}

type crdtCase struct {
	Site  string   `json:"site"`
	Edits []string `json:"edits"`
	Text  string   `json:"text"`
	JSON  string   `json:"json"`
}

type noteCase struct {
	Path  string `json:"path"`
	Text  string `json:"text"`
	Title string `json:"title"`
	Hash  string `json:"hash"`
}

func main() {
	var out output

	salt := []byte("0123456789abcdef")
	for _, tc := range []struct{ pass, kdf, pt string }{
		{"correct horse battery staple", "argon2id", "sealed by Go"},
		{"ünïcode-påss-🔐", "argon2id", "ünïcode from Go 🔐\nmultiline\n"},
		{"legacy vault", "pbkdf2", "legacy kdf path"},
		{"empty plaintext", "argon2id", ""},
	} {
		key, err := crypto.DeriveKey(tc.pass, salt, tc.kdf)
		if err != nil {
			fail(err)
		}
		tok, err := crypto.Seal(key, []byte(tc.pt))
		if err != nil {
			fail(err)
		}
		out.Crypto = append(out.Crypto, cryptoCase{
			Passphrase: tc.pass, KDF: tc.kdf, Plaintext: tc.pt,
			SaltB64: base64.StdEncoding.EncodeToString(salt), Token: string(tok),
		})
	}

	for _, s := range []struct {
		site  string
		edits []string
	}{
		{"local", []string{"", "a", "ab", "abc"}},
		{"site-1", []string{"hello world", "hello brave world", "hello brave"}},
		{"s3", []string{"", "Ünïcodé 🔐", "Ünïcodé 🔐 日本語"}},
		{"s4", []string{"the quick brown fox", "the quick red fox jumps"}},
		{"s5", []string{"aaa bbb ccc", "ccc bbb aaa"}},
		{"html", []string{"", "<b>&</b> tags"}},
	} {
		d := crdt.New(s.site)
		for _, e := range s.edits {
			d.LocalEdit(e)
		}
		out.CRDT = append(out.CRDT, crdtCase{
			Site: s.site, Edits: s.edits, Text: d.Text(), JSON: d.ToJSON(),
		})
	}

	for _, tc := range []struct{ path, text string }{
		{"a.md", "# Title From Heading\n\nbody with #tag and [[Link]]\n"},
		{"b.md", "---\ntitle: FM Title\ntags: [x, y]\n---\nbody\n"},
		{"ü.md", "# Ünïcodé — 日本語\n\n[[Ünïcodé]] 🔐\n"},
	} {
		n := vault.NoteFromText(tc.path, tc.text, 1700000000.0)
		out.Notes = append(out.Notes, noteCase{
			Path: tc.path, Text: tc.text, Title: n.Title, Hash: n.Hash,
		})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "xlangcheck:", err)
	os.Exit(1)
}
