// Package memimage stores images that agent memory refers to.
//
// An image is content-addressed: the file is named by the SHA-256 of the bytes
// actually stored, so a reference (`img=<sha>` in a memory bullet) can never
// point at different pixels than the ones that were written, and identical
// uploads collapse to one file. The format is deliberately narrow — PNG, JPEG,
// GIF and WebP, identified by their magic bytes, never by the file name or the
// client's Content-Type — and everything here is standard library.
//
// Retrieval is by caption only. A memory image has a text caption, the caption
// is the memory's text, and the existing full-text and vector arms index that
// text. Nothing in this package embeds pixels.
package memimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Dir is the vault directory attachments live in, relative to the vault root.
const Dir = "Memory Attachments"

// DefaultMaxBytes is the size cap when GRIMOIRE_MEMORY_IMAGE_MAX_BYTES is unset.
// It is chosen so a base64 JSON upload of a cap-sized image still fits the
// default 8 MiB JSON body limit.
const DefaultMaxBytes = 4 << 20

// ErrUnsupported is returned for any content that is not one of the four formats.
var ErrUnsupported = errors.New("unsupported image: only png, jpeg, gif and webp are accepted")

// Format describes one accepted image format.
type Format struct {
	MIME string
	Ext  string
}

var (
	FormatPNG  = Format{"image/png", "png"}
	FormatJPEG = Format{"image/jpeg", "jpg"}
	FormatGIF  = Format{"image/gif", "gif"}
	FormatWebP = Format{"image/webp", "webp"}
)

// Formats lists every accepted format, for the search order of Find.
var Formats = []Format{FormatPNG, FormatJPEG, FormatGIF, FormatWebP}

var shaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA reports whether s is a well-formed content address.
func ValidSHA(s string) bool { return shaRE.MatchString(s) }

// Sniff identifies the format from the leading bytes. It never trusts a name.
func Sniff(data []byte) (Format, error) {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return FormatPNG, nil
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return FormatJPEG, nil
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return FormatGIF, nil
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return FormatWebP, nil
	}
	return Format{}, ErrUnsupported
}

// Sum is the content address of data.
func Sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// Stripped names what StripEXIF removed, so the caller can report it. Nothing
// is removed silently.
type Stripped struct {
	EXIF bool `json:"exif,omitempty"`
}

// StripEXIF removes EXIF metadata (which carries GPS coordinates, the camera
// serial and capture times) from JPEG and PNG files, and reports whether it
// did. It drops the whole EXIF block, not only the GPS tags: a partial rewrite
// of a TIFF structure is exactly where a stdlib-only parser would corrupt a
// photo. XMP, GIF comments and WebP EXIF chunks are left as they are; the
// reported Stripped value says what was removed, and docs/IMAGE-MEMORY.md says
// what is not.
func StripEXIF(data []byte, f Format) ([]byte, Stripped, error) {
	switch f {
	case FormatJPEG:
		out, ok, err := stripJPEG(data)
		return out, Stripped{EXIF: ok}, err
	case FormatPNG:
		out, ok, err := stripPNG(data)
		return out, Stripped{EXIF: ok}, err
	}
	return data, Stripped{}, nil
}

func stripJPEG(data []byte) ([]byte, bool, error) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, false, ErrUnsupported
	}
	var out bytes.Buffer
	out.Write(data[:2])
	removed := false
	i := 2
	for i < len(data) {
		if data[i] != 0xFF {
			return nil, false, errors.New("malformed jpeg: expected a marker")
		}
		// Skip fill bytes before a marker.
		for i < len(data) && data[i] == 0xFF {
			i++
		}
		if i >= len(data) {
			break
		}
		marker := data[i]
		i++
		// Start of scan: the entropy-coded image data runs to the end of the
		// file (EOI); copy everything that remains untouched.
		if marker == 0xDA {
			out.WriteByte(0xFF)
			out.WriteByte(marker)
			out.Write(data[i-1+1:])
			return out.Bytes(), removed, nil
		}
		if marker == 0xD9 { // EOI with no scan
			out.Write([]byte{0xFF, marker})
			return out.Bytes(), removed, nil
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) { // standalone markers
			out.Write([]byte{0xFF, marker})
			continue
		}
		if i+2 > len(data) {
			return nil, false, errors.New("malformed jpeg: truncated segment")
		}
		n := int(binary.BigEndian.Uint16(data[i : i+2]))
		if n < 2 || i+n > len(data) {
			return nil, false, errors.New("malformed jpeg: bad segment length")
		}
		seg := data[i : i+n]
		body := seg[2:]
		if marker == 0xE1 && bytes.HasPrefix(body, []byte("Exif\x00\x00")) {
			removed = true
		} else {
			out.Write([]byte{0xFF, marker})
			out.Write(seg)
		}
		i += n
	}
	return out.Bytes(), removed, nil
}

func stripPNG(data []byte) ([]byte, bool, error) {
	if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return nil, false, ErrUnsupported
	}
	var out bytes.Buffer
	out.Write(data[:8])
	removed := false
	i := 8
	for i+8 <= len(data) {
		n := int(binary.BigEndian.Uint32(data[i : i+4]))
		typ := string(data[i+4 : i+8])
		end := i + 12 + n // length + type + data + crc
		if n < 0 || end > len(data) {
			return nil, false, errors.New("malformed png: chunk runs past end of file")
		}
		if typ == "eXIf" {
			removed = true
		} else {
			out.Write(data[i:end])
		}
		i = end
		if typ == "IEND" {
			return out.Bytes(), removed, nil
		}
	}
	return nil, false, errors.New("malformed png: no IEND chunk")
}

// Path returns the vault-relative path for a stored image.
func Path(sha, ext string) string { return Dir + "/" + sha + "." + ext }

// Find locates a stored image by content address, returning its format and
// absolute path. The store is a flat directory, so the extension is probed
// rather than recorded.
func Find(vaultRoot, sha string) (Format, string, bool) {
	if !ValidSHA(sha) {
		return Format{}, "", false
	}
	for _, f := range Formats {
		p := filepath.Join(vaultRoot, Dir, sha+"."+f.Ext)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return f, p, true
		}
	}
	return Format{}, "", false
}

// Remove deletes every stored copy of sha. A missing file is not an error.
func Remove(vaultRoot, sha string) error {
	for _, f := range Formats {
		p := filepath.Join(vaultRoot, Dir, sha+"."+f.Ext)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Prepared is an image ready to store.
type Prepared struct {
	SHA      string
	Format   Format
	Data     []byte
	Stripped Stripped
}

// Prepare validates raw bytes against the size cap, sniffs the format, strips
// EXIF and computes the content address of what will actually be stored.
func Prepare(raw []byte, maxBytes int64) (Prepared, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if int64(len(raw)) > maxBytes {
		return Prepared{}, fmt.Errorf("image too large: %d bytes, the limit is %d", len(raw), maxBytes)
	}
	if len(raw) == 0 {
		return Prepared{}, errors.New("image is empty")
	}
	f, err := Sniff(raw)
	if err != nil {
		return Prepared{}, err
	}
	data, st, err := StripEXIF(raw, f)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{SHA: Sum(data), Format: f, Data: data, Stripped: st}, nil
}

// Store writes the prepared image into the vault directory. Writing the same
// content twice is a no-op, which is what content addressing buys.
func Store(vaultRoot string, p Prepared) error {
	dir := filepath.Join(vaultRoot, Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	final := filepath.Join(dir, p.SHA+"."+p.Format.Ext)
	if _, err := os.Stat(final); err == nil {
		return nil
	}
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, p.Data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}
