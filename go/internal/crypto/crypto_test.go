package crypto

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/compat"
)

type cryptoFixture struct {
	Params struct {
		Argon2id struct {
			Time        uint32 `json:"time"`
			MemoryKiB   uint32 `json:"memory_kib"`
			Parallelism uint8  `json:"parallelism"`
			HashLen     uint32 `json:"hash_len"`
		} `json:"argon2id"`
		PBKDF2 struct {
			Iterations int    `json:"iterations"`
			Hash       string `json:"hash"`
			Length     int    `json:"length"`
		} `json:"pbkdf2"`
		DefaultKDF string `json:"default_kdf"`
	} `json:"params"`
	Cases []struct {
		Passphrase string `json:"passphrase"`
		SaltB64    string `json:"salt_b64"`
		KDF        string `json:"kdf"`
		DerivedKey string `json:"derived_key"`
	} `json:"cases"`
	SealCases []struct {
		Passphrase string `json:"passphrase"`
		SaltB64    string `json:"salt_b64"`
		KDF        string `json:"kdf"`
		Plaintext  string `json:"plaintext"`
		Token      string `json:"token"`
	} `json:"seal_cases"`
}

func load(t *testing.T) cryptoFixture {
	t.Helper()
	var fx cryptoFixture
	compat.Load(t, "crypto.json", &fx)
	return fx
}

// The parameters themselves are part of the contract: derive with different
// costs and every existing vault stops opening.
func TestParametersMatchPython(t *testing.T) {
	fx := load(t)
	if fx.Params.Argon2id.Time != ArgonTime ||
		fx.Params.Argon2id.MemoryKiB != ArgonMemoryKiB ||
		fx.Params.Argon2id.Parallelism != ArgonParallelism ||
		fx.Params.Argon2id.HashLen != keyLen {
		t.Errorf("argon2id params drifted: fixture=%+v go=(t=%d,m=%d,p=%d,len=%d)",
			fx.Params.Argon2id, ArgonTime, ArgonMemoryKiB, ArgonParallelism, keyLen)
	}
	if fx.Params.PBKDF2.Iterations != Iterations {
		t.Errorf("pbkdf2 iterations: fixture=%d go=%d", fx.Params.PBKDF2.Iterations, Iterations)
	}
	if fx.Params.DefaultKDF != DefaultKDF {
		t.Errorf("default kdf: fixture=%q go=%q", fx.Params.DefaultKDF, DefaultKDF)
	}
}

func TestDeriveKeyMatchesPython(t *testing.T) {
	for _, c := range load(t).Cases {
		salt, err := base64.StdEncoding.DecodeString(c.SaltB64)
		if err != nil {
			t.Fatalf("bad fixture salt: %v", err)
		}
		got, err := DeriveKey(c.Passphrase, salt, c.KDF)
		if err != nil {
			t.Fatalf("DeriveKey(%q, %s): %v", c.Passphrase, c.KDF, err)
		}
		if string(got) != c.DerivedKey {
			t.Errorf("%s key mismatch for passphrase %q\n got: %s\nwant: %s",
				c.KDF, c.Passphrase, got, c.DerivedKey)
		}
	}
}

// The real test of a drop-in replacement: tokens Python wrote must open here.
func TestUnsealPythonTokens(t *testing.T) {
	for _, c := range load(t).SealCases {
		salt, _ := base64.StdEncoding.DecodeString(c.SaltB64)
		key, err := DeriveKey(c.Passphrase, salt, c.KDF)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Unseal(key, []byte(c.Token))
		if err != nil {
			t.Errorf("Unseal python token (passphrase %q): %v", c.Passphrase, err)
			continue
		}
		if string(got) != c.Plaintext {
			t.Errorf("plaintext mismatch: got %d bytes, want %d", len(got), len(c.Plaintext))
		}
	}
}

func TestSealUnsealRoundTrip(t *testing.T) {
	key, err := DeriveKey("passphrase", bytes.Repeat([]byte{7}, 16), "argon2id")
	if err != nil {
		t.Fatal(err)
	}
	for _, pt := range []string{"", "short", "ünïcode 🔐", string(bytes.Repeat([]byte("x"), 10000))} {
		tok, err := Seal(key, []byte(pt))
		if err != nil {
			t.Fatal(err)
		}
		got, err := Unseal(key, tok)
		if err != nil {
			t.Fatalf("round trip failed for %d bytes: %v", len(pt), err)
		}
		if string(got) != pt {
			t.Errorf("round trip corrupted %d-byte plaintext", len(pt))
		}
	}
}

func TestUnsealRejectsTampering(t *testing.T) {
	key, _ := DeriveKey("passphrase", bytes.Repeat([]byte{7}, 16), "argon2id")
	tok, _ := Seal(key, []byte("authentic message"))

	raw, _ := base64.URLEncoding.DecodeString(string(tok))
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"flipped ciphertext bit", func(b []byte) []byte { c := append([]byte{}, b...); c[30] ^= 1; return c }},
		{"flipped hmac bit", func(b []byte) []byte { c := append([]byte{}, b...); c[len(c)-1] ^= 1; return c }},
		{"wrong version", func(b []byte) []byte { c := append([]byte{}, b...); c[0] = 0x81; return c }},
		{"truncated", func(b []byte) []byte { return b[:len(b)-1] }},
		{"empty", func(b []byte) []byte { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := base64.URLEncoding.EncodeToString(tc.mutate(raw))
			if _, err := Unseal(key, []byte(bad)); err == nil {
				t.Error("tampered token was accepted")
			}
		})
	}

	other, _ := DeriveKey("different passphrase", bytes.Repeat([]byte{7}, 16), "argon2id")
	if _, err := Unseal(other, tok); err == nil {
		t.Error("token opened with the wrong key")
	}
}

// Padding must be validated in full — a lenient unpad is a padding oracle.
func TestPKCS7RejectsBadPadding(t *testing.T) {
	if _, err := pkcs7Unpad([]byte{1, 2, 3, 4, 5, 6, 7, 0}, 8); err == nil {
		t.Error("accepted zero-length padding")
	}
	if _, err := pkcs7Unpad([]byte{1, 2, 3, 4, 5, 3, 3, 2}, 8); err == nil {
		t.Error("accepted inconsistent padding bytes")
	}
	if _, err := pkcs7Unpad([]byte{1, 2, 3, 4, 5, 6, 7, 9}, 8); err == nil {
		t.Error("accepted oversized pad length")
	}
}
