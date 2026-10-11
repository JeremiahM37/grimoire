package memimage

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	// Decoders register themselves with image.Decode. Only the formats the
	// standard library can read are listed; WebP needs golang.org/x/image,
	// which is not vendored, so WebP has no signature (see ErrNoSignature).
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math/bits"
)

// A Signature is a cheap perceptual fingerprint of what a picture looks like.
//
// It has two parts. DHash is the difference hash: the picture is reduced to a
// 9x8 grid of mean luminances and each bit records whether a cell is brighter
// than its right-hand neighbour. Two pictures of the same scene, at a different
// size or after re-encoding, keep most of those bits; two unrelated pictures
// differ in about half of them. Hist is a 4x4x4 RGB colour histogram, which
// the hash ignores, so it can tell a recolour apart from a re-crop.
//
// This measures visual similarity of pixels. It does not recognise what is
// in the picture, and it is not invariant to rotation, flips, crops or large
// colour changes. docs/IMAGE-MEMORY.md states the measured limits.
type Signature struct {
	DHash uint64
	Hist  [HistBins]uint8
}

const (
	hashCols  = 9 // 8 comparisons per row
	hashRows  = 8
	histSide  = 4 // bins per RGB channel
	HistBins  = histSide * histSide * histSide
	histShift = 6 // 8-bit channel >> 6 gives 4 levels
	// maxPixels refuses a picture whose decoded size would be out of
	// proportion to its file, so a small compressed bomb cannot exhaust memory.
	maxPixels = 40_000_000
	// histSamples bounds the colour histogram's work on large pictures.
	histSamples = 256
)

// ErrNoSignature is returned for a picture the standard library cannot decode.
var ErrNoSignature = errors.New("no perceptual signature: this format cannot be decoded without a third-party package")

// ErrTooManyPixels is returned for a picture above maxPixels.
var ErrTooManyPixels = errors.New("picture is too large to fingerprint")

// ComputeSignature fingerprints the pixels in data. The bytes must already be
// the stored bytes (EXIF removed), so the signature describes what is kept.
func ComputeSignature(data []byte) (Signature, error) {
	f, err := Sniff(data)
	if err != nil {
		return Signature{}, err
	}
	if f == FormatWebP {
		return Signature{}, ErrNoSignature
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Signature{}, fmt.Errorf("decode picture: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return Signature{}, ErrTooManyPixels
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Signature{}, fmt.Errorf("decode picture: %w", err)
	}
	return signatureOf(img), nil
}

// signatureOf computes the signature of an already decoded image.
func signatureOf(img image.Image) Signature {
	b := img.Bounds()
	var sig Signature
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return sig
	}

	// Mean luminance per cell. Each cell is sampled on a small grid rather
	// than every pixel, which keeps a large picture cheap and is deterministic.
	var means [hashRows][hashCols]float64
	for gy := 0; gy < hashRows; gy++ {
		y0, y1 := span(b.Min.Y, b.Dy(), hashRows, gy)
		for gx := 0; gx < hashCols; gx++ {
			x0, x1 := span(b.Min.X, b.Dx(), hashCols, gx)
			means[gy][gx] = meanLuma(img, x0, x1, y0, y1)
		}
	}
	bit := 0
	for gy := 0; gy < hashRows; gy++ {
		for gx := 0; gx < hashCols-1; gx++ {
			if means[gy][gx] > means[gy][gx+1] {
				sig.DHash |= 1 << uint(bit)
			}
			bit++
		}
	}

	// Colour histogram over a bounded grid of the whole picture.
	stepX := max(1, b.Dx()/histSamples)
	stepY := max(1, b.Dy()/histSamples)
	var counts [HistBins]int
	total := 0
	for y := b.Min.Y; y < b.Max.Y; y += stepY {
		for x := b.Min.X; x < b.Max.X; x += stepX {
			r, g, bl := rgb8(img.At(x, y))
			counts[(r>>histShift)*histSide*histSide+(g>>histShift)*histSide+(bl>>histShift)]++
			total++
		}
	}
	for i, c := range counts {
		sig.Hist[i] = uint8(c * 255 / total)
	}
	return sig
}

// span returns the half-open range [start,end) of cell i when size pixels
// starting at lo are divided into n cells. A cell is never empty, so a picture
// smaller than the grid still produces every cell.
func span(lo, size, n, i int) (int, int) {
	start := lo + i*size/n
	end := lo + (i+1)*size/n
	if end <= start {
		end = start + 1
	}
	return start, end
}

// meanLuma averages the luminance of a sampled grid inside [x0,x1)x[y0,y1).
func meanLuma(img image.Image, x0, x1, y0, y1 int) float64 {
	b := img.Bounds()
	stepX := max(1, (x1-x0)/12)
	stepY := max(1, (y1-y0)/12)
	var sum float64
	n := 0
	for y := y0; y < y1 && y < b.Max.Y; y += stepY {
		for x := x0; x < x1 && x < b.Max.X; x += stepX {
			r, g, bl := rgb8(img.At(x, y))
			sum += (299*float64(r) + 587*float64(g) + 114*float64(bl)) / 1000
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// rgb8 reads a pixel as 8-bit RGB, composited over white. A transparent PNG
// otherwise hashes as black, and the same picture on a white page would not
// match it.
func rgb8(c color.Color) (uint8, uint8, uint8) {
	r, g, b, a := c.RGBA()
	white := uint32(0xffff) - a
	return uint8((r + white) >> 8), uint8((g + white) >> 8), uint8((b + white) >> 8)
}

// Hamming is the number of differing bits between two difference hashes.
func Hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// ColorDistance is the L1 distance between two colour histograms, scaled to
// [0,1]: 0 for identical colour distributions, 1 for disjoint ones.
func ColorDistance(a, b [HistBins]uint8) float64 {
	sum := 0
	for i := range a {
		d := int(a[i]) - int(b[i])
		if d < 0 {
			d = -d
		}
		sum += d
	}
	return float64(sum) / (2 * 255)
}

// Hist64 is the histogram as a slice, the form SQLite stores it in.
func (s Signature) Hist64() []byte { return s.Hist[:] }

// HashInt is the DHash as a signed integer, the form SQLite stores it in.
func (s Signature) HashInt() int64 { return int64(s.DHash) }

// SignatureFromStored rebuilds a Signature from its stored form. It refuses a
// histogram of the wrong length rather than comparing against a short slice.
func SignatureFromStored(dhash int64, hist []byte) (Signature, error) {
	var s Signature
	if len(hist) != HistBins {
		return s, fmt.Errorf("stored histogram has %d bins, want %d", len(hist), HistBins)
	}
	s.DHash = uint64(dhash)
	copy(s.Hist[:], hist)
	return s, nil
}
