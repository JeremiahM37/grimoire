package memimage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func jpegWith(exif bool) []byte {
	var b bytes.Buffer
	b.Write([]byte{0xFF, 0xD8})
	b.Write([]byte{0xFF, 0xE0, 0x00, 0x10}) // APP0 JFIF
	b.Write([]byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00"))
	if exif {
		payload := append([]byte("Exif\x00\x00"), []byte("GPS-LAT-51.5007")...)
		b.Write([]byte{0xFF, 0xE1})
		binary.Write(&b, binary.BigEndian, uint16(len(payload)+2))
		b.Write(payload)
	}
	b.Write([]byte{0xFF, 0xDA, 0x00, 0x04, 0x01, 0x02}) // SOS header
	b.Write([]byte{0x11, 0x22, 0x33})                   // scan data
	b.Write([]byte{0xFF, 0xD9})
	return b.Bytes()
}

func chunk(typ string, data []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(len(data)))
	b.WriteString(typ)
	b.Write(data)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	return b.Bytes()
}

func pngWith(exif bool) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	b.Write(chunk("IHDR", make([]byte, 13)))
	if exif {
		b.Write(chunk("eXIf", []byte("GPS-LAT-51.5007")))
	}
	b.Write(chunk("IDAT", []byte{0x78, 0x9c, 0x63, 0x00}))
	b.Write(chunk("IEND", nil))
	return b.Bytes()
}

func TestSniffAcceptsOnlyTheFourFormats(t *testing.T) {
	ok := map[string]Format{
		"\x89PNG\r\n\x1a\nrest":          FormatPNG,
		"\xFF\xD8\xFF\xE0junk":           FormatJPEG,
		"GIF89a........":                 FormatGIF,
		"RIFF\x10\x00\x00\x00WEBPVP8 ..": FormatWebP,
	}
	for in, want := range ok {
		got, err := Sniff([]byte(in))
		if err != nil || got != want {
			t.Errorf("Sniff(%q) = %v, %v; want %v", in[:8], got, err, want)
		}
	}
	for _, in := range []string{"BM\x00\x00bmp", "<svg xmlns=", "<html>", "RIFF\x00\x00\x00\x00WAVE", ""} {
		if _, err := Sniff([]byte(in)); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Sniff(%q) accepted an unsupported format: %v", in, err)
		}
	}
}

func TestPrepareEnforcesCapAndEmpty(t *testing.T) {
	png := pngWith(false)
	if _, err := Prepare(png, int64(len(png))-1); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("over-cap image accepted: %v", err)
	}
	if _, err := Prepare(png, int64(len(png))); err != nil {
		t.Fatalf("image at the cap refused: %v", err)
	}
	if _, err := Prepare(nil, 0); err == nil {
		t.Fatal("empty image accepted")
	}
	if _, err := Prepare([]byte("not an image"), 0); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("non-image accepted: %v", err)
	}
}

func TestStripEXIFRemovesOnlyTheExifBlock(t *testing.T) {
	with := jpegWith(true)
	out, st, err := StripEXIF(with, FormatJPEG)
	if err != nil || !st.EXIF {
		t.Fatalf("jpeg with EXIF: stripped=%v err=%v", st, err)
	}
	if bytes.Contains(out, []byte("GPS-LAT")) || bytes.Contains(out, []byte("Exif\x00\x00")) {
		t.Fatal("EXIF payload survived the strip")
	}
	if !bytes.Contains(out, []byte("JFIF")) || !bytes.Contains(out, []byte{0x11, 0x22, 0x33}) {
		t.Fatal("JFIF header or scan data was damaged")
	}
	if !bytes.HasSuffix(out, []byte{0xFF, 0xD9}) {
		t.Fatal("EOI missing after strip")
	}

	plain := jpegWith(false)
	same, st, err := StripEXIF(plain, FormatJPEG)
	if err != nil || st.EXIF || !bytes.Equal(same, plain) {
		t.Fatalf("jpeg without EXIF must pass through unchanged: %v %v", st, err)
	}
}

func TestStripEXIFRemovesPNGeXIfChunk(t *testing.T) {
	out, st, err := StripEXIF(pngWith(true), FormatPNG)
	if err != nil || !st.EXIF {
		t.Fatalf("png with eXIf: stripped=%v err=%v", st, err)
	}
	if bytes.Contains(out, []byte("eXIf")) || bytes.Contains(out, []byte("GPS-LAT")) {
		t.Fatal("eXIf chunk survived")
	}
	if !bytes.Contains(out, []byte("IDAT")) || !bytes.HasSuffix(out, chunk("IEND", nil)) {
		t.Fatal("image chunks were damaged")
	}
}

func TestStripEXIFLeavesGIFAlone(t *testing.T) {
	gif := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")
	out, st, err := StripEXIF(gif, FormatGIF)
	if err != nil || st.EXIF || !bytes.Equal(out, gif) {
		t.Fatalf("gif must not be rewritten: %v %v", st, err)
	}
}

func TestPreparedAddressIsOfTheStoredBytes(t *testing.T) {
	with := jpegWith(true)
	p, err := Prepare(with, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.SHA != Sum(p.Data) || p.SHA == Sum(with) {
		t.Fatal("address must be the hash of the bytes actually stored, after EXIF removal")
	}
	again, err := Prepare(p.Data, 0)
	if err != nil || again.SHA != p.SHA {
		t.Fatalf("re-preparing stored bytes must be a no-op: %v", err)
	}
}

func TestStoreFindRemoveAreContentAddressed(t *testing.T) {
	root := t.TempDir()
	p, err := Prepare(pngWith(false), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := Store(root, p); err != nil {
		t.Fatal(err)
	}
	if err := Store(root, p); err != nil { // second write is a no-op
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, Dir))
	if len(entries) != 1 {
		t.Fatalf("content-addressed store kept %d files for one picture", len(entries))
	}
	f, path, ok := Find(root, p.SHA)
	if !ok || f != FormatPNG || filepath.Base(path) != p.SHA+".png" {
		t.Fatalf("Find: %v %q %v", f, path, ok)
	}
	if _, _, ok := Find(root, strings.Repeat("0", 64)); ok {
		t.Fatal("Find invented a picture")
	}
	if _, _, ok := Find(root, "../../etc/passwd"); ok || ValidSHA("../../etc/passwd") {
		t.Fatal("a non-hash was accepted as an address")
	}
	if err := Remove(root, p.SHA); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := Find(root, p.SHA); ok {
		t.Fatal("Remove left the picture behind")
	}
	if err := Remove(root, p.SHA); err != nil {
		t.Fatalf("removing a missing picture must not fail: %v", err)
	}
}
