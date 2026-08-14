// Package crypto is the authenticated-encryption layer for the secret vault.
//
// This is a byte-compatible port of server/crypto.py: existing vaults and
// encrypted notes must open unchanged, so every constant and construction here
// is fixed by what Python already wrote to disk, not by preference.
//
// Key derivation: Argon2id from the passphrase + a per-vault random salt.
// Legacy vaults created with PBKDF2-HMAC-SHA256 still unlock (the KDF is
// recorded per-vault). Encryption: Fernet (AES-128-CBC + HMAC-SHA256).
//
// Fernet is implemented here rather than pulled in as a dependency: the spec is
// small and fully determined, and a third-party package is a supply-chain risk
// on the one code path that can lose a user's data.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/argon2"
)

// Legacy PBKDF2 — only used to unlock pre-existing vaults.
const Iterations = 240_000

// Argon2id parameters (OWASP-aligned: 64 MiB, t=3, p=4).
const (
	ArgonTime        uint32 = 3
	ArgonMemoryKiB   uint32 = 64 * 1024
	ArgonParallelism uint8  = 4
	keyLen           uint32 = 32
)

const DefaultKDF = "argon2id"

// fernetVersion is the single byte every Fernet token starts with.
const fernetVersion byte = 0x80

var ErrInvalidToken = errors.New("wrong passphrase or corrupted data")

// NewSalt returns a fresh 16-byte salt.
func NewSalt() ([]byte, error) {
	s := make([]byte, 16)
	if _, err := rand.Read(s); err != nil {
		return nil, err
	}
	return s, nil
}

// DeriveKey returns a 32-byte Fernet key, urlsafe-base64 encoded exactly as
// Python's derive_key does — callers pass this value straight to Seal/Unseal.
// kdf selects the algorithm so old vaults (pbkdf2) keep working while new ones
// use argon2id.
func DeriveKey(passphrase string, salt []byte, kdf string) ([]byte, error) {
	pw := []byte(passphrase)
	var raw []byte
	switch kdf {
	case "argon2id":
		raw = argon2.IDKey(pw, salt, ArgonTime, ArgonMemoryKiB, ArgonParallelism, keyLen)
	case "pbkdf2":
		var err error
		raw, err = pbkdf2.Key(sha256.New, string(pw), salt, Iterations, int(keyLen))
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown kdf: %q", kdf)
	}
	out := make([]byte, base64.URLEncoding.EncodedLen(len(raw)))
	base64.URLEncoding.Encode(out, raw)
	return out, nil
}

// splitKey turns a urlsafe-base64 Fernet key into its signing and encryption
// halves, in that order — the order Fernet defines, not an arbitrary one.
func splitKey(key []byte) (signing, encryption []byte, err error) {
	raw := make([]byte, base64.URLEncoding.DecodedLen(len(key)))
	n, err := base64.URLEncoding.Decode(raw, key)
	if err != nil {
		return nil, nil, fmt.Errorf("malformed fernet key: %w", err)
	}
	if n != 32 {
		return nil, nil, fmt.Errorf("fernet key must be 32 bytes, got %d", n)
	}
	return raw[:16], raw[16:32], nil
}

// Seal encrypts plaintext into a Fernet token.
func Seal(key, plaintext []byte) ([]byte, error) {
	return sealAt(key, plaintext, time.Now().Unix(), nil)
}

// sealAt exists so tests can pin the timestamp and IV; production always uses
// the current time and a random IV.
func sealAt(key, plaintext []byte, ts int64, iv []byte) ([]byte, error) {
	signing, encryption, err := splitKey(key)
	if err != nil {
		return nil, err
	}
	if iv == nil {
		iv = make([]byte, aes.BlockSize)
		if _, err := rand.Read(iv); err != nil {
			return nil, err
		}
	}
	block, err := aes.NewCipher(encryption)
	if err != nil {
		return nil, err
	}
	padded := pkcs7Pad(plaintext, aes.BlockSize)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)

	// version || timestamp || iv || ciphertext, then HMAC over all of it
	body := make([]byte, 0, 1+8+len(iv)+len(ct)+sha256.Size)
	body = append(body, fernetVersion)
	body = binary.BigEndian.AppendUint64(body, uint64(ts))
	body = append(body, iv...)
	body = append(body, ct...)

	mac := hmac.New(sha256.New, signing)
	mac.Write(body)
	body = mac.Sum(body)

	out := make([]byte, base64.URLEncoding.EncodedLen(len(body)))
	base64.URLEncoding.Encode(out, body)
	return out, nil
}

// Unseal decrypts a Fernet token. It does not enforce a TTL, matching the
// Python side (which calls Fernet.decrypt with no ttl).
func Unseal(key, token []byte) ([]byte, error) {
	signing, encryption, err := splitKey(key)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, base64.URLEncoding.DecodedLen(len(token)))
	n, err := base64.URLEncoding.Decode(raw, token)
	if err != nil {
		return nil, ErrInvalidToken
	}
	raw = raw[:n]

	// version(1) + timestamp(8) + iv(16) + at least one block + hmac(32)
	if len(raw) < 1+8+aes.BlockSize+aes.BlockSize+sha256.Size {
		return nil, ErrInvalidToken
	}
	if raw[0] != fernetVersion {
		return nil, ErrInvalidToken
	}
	macStart := len(raw) - sha256.Size
	mac := hmac.New(sha256.New, signing)
	mac.Write(raw[:macStart])
	// constant-time: never leak where a forged token first diverges
	if subtle.ConstantTimeCompare(mac.Sum(nil), raw[macStart:]) != 1 {
		return nil, ErrInvalidToken
	}

	iv := raw[9 : 9+aes.BlockSize]
	ct := raw[9+aes.BlockSize : macStart]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, ErrInvalidToken
	}
	block, err := aes.NewCipher(encryption)
	if err != nil {
		return nil, err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	return pkcs7Unpad(pt, aes.BlockSize)
}

func pkcs7Pad(b []byte, size int) []byte {
	n := size - len(b)%size
	out := make([]byte, len(b)+n)
	copy(out, b)
	for i := len(b); i < len(out); i++ {
		out[i] = byte(n)
	}
	return out
}

func pkcs7Unpad(b []byte, size int) ([]byte, error) {
	if len(b) == 0 || len(b)%size != 0 {
		return nil, ErrInvalidToken
	}
	n := int(b[len(b)-1])
	if n == 0 || n > size || n > len(b) {
		return nil, ErrInvalidToken
	}
	// check every pad byte, not just the last — a lenient unpad is a padding oracle
	bad := 0
	for _, c := range b[len(b)-n:] {
		bad |= int(c) ^ n
	}
	if bad != 0 {
		return nil, ErrInvalidToken
	}
	return b[:len(b)-n], nil
}
