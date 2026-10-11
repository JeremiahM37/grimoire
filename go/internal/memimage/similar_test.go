package memimage

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// scene draws a procedural picture at any size. The same scene at two sizes
// is the same picture resized, because every feature is positioned as a
// fraction of the frame. palette shifts the colours without changing where
// the light and dark areas fall.
func scene(w, h int, seed uint32, tint color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// A diagonal gradient with two large blocks at fixed fractions of
			// the frame. Brightness changes with position, not with size.
			v := uint8((x*255/w + y*255/h) / 2)
			c := color.RGBA{R: uint8(int(v) * int(tint.R) / 255), G: uint8(int(v) * int(tint.G) / 255), B: uint8(int(v) * int(tint.B) / 255), A: 255}
			if x > w/5 && x < w*2/5 && y > h/4 && y < h*3/4 {
				c = color.RGBA{R: 20, G: 20, B: 20, A: 255}
			}
			if x > w*3/5 && y < h/2 {
				c = color.RGBA{R: 240, G: 240, B: 230, A: 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	_ = seed
	return img
}

// noise is an unrelated picture: a pseudo-random field with no shared layout.
func noise(w, h int, seed uint32) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	s := seed
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			s = s*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{R: uint8(s >> 24), G: uint8(s >> 16), B: uint8(s >> 8), A: 255})
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sigOf(t *testing.T, data []byte) Signature {
	t.Helper()
	s, err := ComputeSignature(data)
	if err != nil {
		t.Fatalf("ComputeSignature: %v", err)
	}
	return s
}

// The ordering the feature depends on: a picture is closest to itself, then to
// a resized or re-encoded copy, and an unrelated picture is far away. The
// recoloured copy is measured and reported; it is asserted only to be nearer
// than the unrelated picture, because a hash over luminance cannot promise
// more.
func TestSimilarityOrderingOnGeneratedPictures(t *testing.T) {
	base := scene(160, 120, 0, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	orig := encodePNG(t, base)
	sigOrig := sigOf(t, orig)

	identical := sigOf(t, orig)
	resized := sigOf(t, encodePNG(t, scene(80, 60, 0, color.RGBA{R: 255, G: 255, B: 255, A: 255})))
	reencoded := sigOf(t, encodeJPEG(t, base))
	recolored := sigOf(t, encodePNG(t, scene(160, 120, 0, color.RGBA{R: 255, G: 180, B: 90, A: 255})))
	unrelated := sigOf(t, encodePNG(t, noise(160, 120, 7)))

	dIdentical := Hamming(sigOrig.DHash, identical.DHash)
	dResized := Hamming(sigOrig.DHash, resized.DHash)
	dJPEG := Hamming(sigOrig.DHash, reencoded.DHash)
	dRecolored := Hamming(sigOrig.DHash, recolored.DHash)
	dUnrelated := Hamming(sigOrig.DHash, unrelated.DHash)
	t.Logf("dHash distance (of 64): identical=%d resized=%d jpeg=%d recolored=%d unrelated=%d",
		dIdentical, dResized, dJPEG, dRecolored, dUnrelated)

	if dIdentical != 0 {
		t.Fatalf("identical picture differs by %d bits", dIdentical)
	}
	if dResized >= dUnrelated || dJPEG >= dUnrelated {
		t.Fatalf("resized (%d) or re-encoded (%d) picture is not nearer than an unrelated one (%d)", dResized, dJPEG, dUnrelated)
	}
	if dRecolored >= dUnrelated {
		t.Fatalf("recoloured picture (%d) is not nearer than an unrelated one (%d)", dRecolored, dUnrelated)
	}
	if dUnrelated < 16 {
		t.Fatalf("unrelated picture is only %d bits away; the threshold would admit it", dUnrelated)
	}

	// The hash ignores colour on purpose; the histogram is what sees it.
	cdRecolored := ColorDistance(sigOrig.Hist, recolored.Hist)
	cdResized := ColorDistance(sigOrig.Hist, resized.Hist)
	t.Logf("colour distance (0-1): recolored=%.3f resized=%.3f", cdRecolored, cdResized)
	if cdRecolored < 0.05 {
		t.Fatalf("a recoloured picture is %.3f apart in colour; the histogram did not see it", cdRecolored)
	}
	if cdResized > 0.05 {
		t.Fatalf("a resized picture is %.3f apart in colour; the histogram should not depend on size", cdResized)
	}
}

// A textured picture resized by averaging 2x2 blocks: the case a real photo
// presents, where the grid cells carry noise. Reported, and asserted only to
// stay nearer than an unrelated picture.
func TestResizedTexturedPictureStaysNear(t *testing.T) {
	big := scene(320, 240, 0, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	s := uint32(99)
	for i := range big.Pix {
		if i%4 == 3 {
			continue
		}
		s = s*1664525 + 1013904223
		j := int(s>>28) - 8 // +-8 jitter
		v := int(big.Pix[i]) + j
		if v < 0 {
			v = 0
		} else if v > 255 {
			v = 255
		}
		big.Pix[i] = uint8(v)
	}
	half := image.NewRGBA(image.Rect(0, 0, 160, 120))
	for y := 0; y < 120; y++ {
		for x := 0; x < 160; x++ {
			var r, g, b int
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					c := big.RGBAAt(2*x+dx, 2*y+dy)
					r += int(c.R)
					g += int(c.G)
					b += int(c.B)
				}
			}
			half.SetRGBA(x, y, color.RGBA{R: uint8(r / 4), G: uint8(g / 4), B: uint8(b / 4), A: 255})
		}
	}
	a := sigOf(t, encodePNG(t, big))
	near := sigOf(t, encodeJPEG(t, half))
	far := sigOf(t, encodePNG(t, noise(320, 240, 3)))
	dNear := Hamming(a.DHash, near.DHash)
	dFar := Hamming(a.DHash, far.DHash)
	t.Logf("textured: resized+jpeg=%d bits, unrelated=%d bits (of 64)", dNear, dFar)
	if dNear >= dFar {
		t.Fatalf("resized textured picture (%d) is not nearer than an unrelated one (%d)", dNear, dFar)
	}
}

// Rank orders by hash distance, returns only what is within the threshold, and
// is deterministic when distances tie.
func TestRankIsOrderedThresholdedAndStable(t *testing.T) {
	q := Signature{DHash: 0}
	cands := map[string]Signature{
		"b": {DHash: 0x3},            // 2 bits
		"a": {DHash: 0x3},            // 2 bits, ties with b
		"c": {DHash: 0xFFFFFFFF},     // 32 bits
		"d": {DHash: 0x1},            // 1 bit
		"e": {DHash: 0xFFFFFFFFFFFF}, // 48 bits
	}
	got := Rank(q, cands, 8)
	want := []string{"d", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %d matches, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].SHA != want[i] {
			t.Fatalf("position %d: got %s want %s (%+v)", i, got[i].SHA, want[i], got)
		}
	}
	if again := Rank(q, cands, 8); again[1].SHA != "a" {
		t.Fatalf("tie order changed between calls: %+v", again)
	}
}

// A picture that cannot be decoded without a third-party package has no
// signature. It must say so rather than return a zero signature that would
// match a blank picture.
func TestSignatureRefusesWebPAndGarbage(t *testing.T) {
	webp := []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")
	if _, err := ComputeSignature(webp); err != ErrNoSignature {
		t.Fatalf("webp: got %v, want ErrNoSignature", err)
	}
	if _, err := ComputeSignature([]byte("not an image at all")); err == nil {
		t.Fatal("garbage produced a signature")
	}
}

// A blank white picture and a blank black picture are different pictures.
func TestSignatureSeparatesBlankPictures(t *testing.T) {
	white := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range white.Pix {
		white.Pix[i] = 255
	}
	black := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := 3; i < len(black.Pix); i += 4 {
		black.Pix[i] = 255
	}
	w := sigOf(t, encodePNG(t, white))
	b := sigOf(t, encodePNG(t, black))
	if ColorDistance(w.Hist, b.Hist) < 0.9 {
		t.Fatalf("white and black histograms too close: %v", ColorDistance(w.Hist, b.Hist))
	}
}

// The stored form round-trips, and a histogram of the wrong length is refused.
func TestSignatureStorageRoundTrip(t *testing.T) {
	s := sigOf(t, encodePNG(t, scene(64, 48, 0, color.RGBA{R: 200, G: 100, B: 50, A: 255})))
	back, err := SignatureFromStored(s.HashInt(), s.Hist64())
	if err != nil {
		t.Fatal(err)
	}
	if back != s {
		t.Fatal("signature changed through storage")
	}
	if _, err := SignatureFromStored(0, make([]byte, 3)); err == nil {
		t.Fatal("short histogram accepted")
	}
}
