package cloudsync

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Format identifies a cloud-folder backup; Version is the on-disk layout.
// A device refuses a newer Version rather than guessing at it.
const (
	Format  = "grimoire-cloud-sync"
	Version = 1
	Cipher  = "xchacha20-poly1305"
)

// KDFParams are the passphrase-stretching parameters, stored in the header so
// a future version can raise them without stranding an existing backup.
type KDFParams struct {
	Name      string `json:"name"` // "argon2id"
	Salt      string `json:"salt"` // base64, 16 random bytes
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
}

// DefaultKDF is what a new backup gets: Argon2id at the RFC 9106 "second
// recommended" memory-constrained setting. A variable so tests can make
// derivation cheap; production never changes it.
var DefaultKDF = KDFParams{Name: "argon2id", Time: 3, MemoryKiB: 64 * 1024, Threads: 4}

// Header is the one unencrypted file in the folder. It holds nothing secret:
// how to stretch a passphrase, and a check value that tells a device whether
// the passphrase it was given is the right one, so "wrong passphrase" is a
// clear message rather than a stream of decryption failures.
type Header struct {
	Format  string    `json:"format"`
	Version int       `json:"version"`
	ID      string    `json:"id"` // random identity of this backup
	Cipher  string    `json:"cipher"`
	KDF     KDFParams `json:"kdf"`
	Check   string    `json:"check"`
	Created int64     `json:"created"`
}

// keys are the subkeys derived from the passphrase-stretched master key. They
// are separate so the key that names blobs can never be used to decrypt one.
type keys struct {
	master []byte
	enc    []byte // XChaCha20-Poly1305
	mac    []byte // keyed content hash: blob names, so names do not reveal content
	check  []byte // the header's check value
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS RNG failing is not a condition to continue under
	}
	return b
}

func randHex(n int) string { return hex.EncodeToString(randBytes(n)) }

// newHeader makes the header for a brand-new backup.
func newHeader(passphrase string, now int64) (*Header, *keys, error) {
	p := DefaultKDF
	p.Salt = base64.StdEncoding.EncodeToString(randBytes(16))
	h := &Header{Format: Format, Version: Version, ID: randHex(16),
		Cipher: Cipher, KDF: p, Created: now}
	k, err := deriveKeys(passphrase, p)
	if err != nil {
		return nil, nil, err
	}
	h.Check = k.checkValue(h.ID)
	return h, k, nil
}

func deriveKeys(passphrase string, p KDFParams) (*keys, error) {
	if p.Name != "argon2id" {
		return nil, fmt.Errorf("unsupported key derivation %q", p.Name)
	}
	salt, err := base64.StdEncoding.DecodeString(p.Salt)
	if err != nil || len(salt) < 16 {
		return nil, errors.New("header salt is malformed")
	}
	if p.Time == 0 || p.MemoryKiB == 0 || p.Threads == 0 || p.MemoryKiB > 4<<20 || p.Time > 64 {
		return nil, errors.New("header key-derivation parameters are out of range")
	}
	master := argon2.IDKey([]byte(passphrase), salt, p.Time, p.MemoryKiB, p.Threads, 32)
	return keysFromMaster(master)
}

func keysFromMaster(master []byte) (*keys, error) {
	if len(master) != 32 {
		return nil, errors.New("stored key has the wrong length")
	}
	sub := func(info string) []byte {
		out := make([]byte, 32)
		if _, err := io.ReadFull(hkdf.New(sha256.New, master, nil, []byte(info)), out); err != nil {
			panic(err)
		}
		return out
	}
	return &keys{master: master,
		enc:   sub("grimoire cloud sync v1 encryption"),
		mac:   sub("grimoire cloud sync v1 content names"),
		check: sub("grimoire cloud sync v1 key check")}, nil
}

func (k *keys) checkValue(backupID string) string {
	m := hmac.New(sha256.New, k.check)
	m.Write([]byte("grimoire cloud sync key check\x00" + backupID))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// verify reports whether these keys open the backup the header describes.
func (k *keys) verify(h *Header) bool {
	return hmac.Equal([]byte(k.checkValue(h.ID)), []byte(h.Check))
}

// name is the keyed hash of content: the blob's file name, and the content
// identity every manifest compares. Keyed, so a name says nothing about the
// content to anyone without the passphrase, not even "these two devices hold
// the same well-known file".
func (k *keys) name(content []byte) string {
	m := hmac.New(sha256.New, k.mac)
	m.Write(content)
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// sealMagic starts every encrypted object, so a truncated or foreign file is
// recognised before any decryption is attempted.
var sealMagic = []byte("GRS1")

// seal compresses then encrypts. The associated data binds the object to its
// role and name: a blob cannot be passed off as a manifest, and one device's
// manifest cannot be swapped into another device's directory.
func (k *keys) seal(aad string, plain []byte) ([]byte, error) {
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	if _, err := zw.Write(plain); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(k.enc)
	if err != nil {
		return nil, err
	}
	nonce := randBytes(chacha20poly1305.NonceSizeX)
	out := make([]byte, 0, len(sealMagic)+len(nonce)+len(plain)/2+64)
	out = append(out, sealMagic...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, z.Bytes(), []byte(aad)), nil
}

// errIncomplete means an object exists but is not (yet) a whole, authentic
// object: a cloud drive mid-download, a truncated copy, or tampering. Callers
// skip it and retry; they never read it as "this was deleted".
var errIncomplete = errors.New("incomplete or damaged file")

func (k *keys) open(aad string, sealed []byte) ([]byte, error) {
	hdr := len(sealMagic) + chacha20poly1305.NonceSizeX
	if len(sealed) < hdr+chacha20poly1305.Overhead || !bytes.Equal(sealed[:len(sealMagic)], sealMagic) {
		return nil, errIncomplete
	}
	aead, err := chacha20poly1305.NewX(k.enc)
	if err != nil {
		return nil, err
	}
	z, err := aead.Open(nil, sealed[len(sealMagic):hdr], sealed[hdr:], []byte(aad))
	if err != nil {
		return nil, errIncomplete
	}
	zr, err := gzip.NewReader(bytes.NewReader(z))
	if err != nil {
		return nil, errIncomplete
	}
	plain, err := io.ReadAll(io.LimitReader(zr, maxObjectBytes+1))
	if err != nil || len(plain) > maxObjectBytes {
		return nil, errIncomplete
	}
	return plain, nil
}
