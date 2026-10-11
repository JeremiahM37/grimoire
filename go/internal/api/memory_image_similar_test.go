package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// gradientPNG is a picture with a large dark block and a bright corner, drawn
// as fractions of the frame so the same scene at another size is the same
// picture. bright shifts the whole scene, which is a recolour.
func gradientPNG(t *testing.T, w, h int, bright uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8((x*255/w + y*255/h) / 2)
			c := color.RGBA{R: v / 2, G: uint8(int(v) * int(bright) / 255), B: v, A: 255}
			if x > w/5 && x < w*2/5 && y > h/4 && y < h*3/4 {
				c = color.RGBA{R: 20, G: 20, B: 20, A: 255}
			}
			if x > w*3/5 && y < h/2 {
				c = color.RGBA{R: 240, G: 240, B: 230, A: 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// noisePNG is an unrelated picture: no layout in common with gradientPNG.
func noisePNG(t *testing.T, w, h int, seed uint32) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	s := seed
	for i := range img.Pix {
		if i%4 == 3 {
			img.Pix[i] = 255
			continue
		}
		s = s*1664525 + 1013904223
		img.Pix[i] = uint8(s >> 24)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegOf(t *testing.T, data []byte) []byte {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type similarResp struct {
	Method      string `json:"method"`
	MaxDistance int    `json:"max_distance"`
	Matches     []struct {
		SHA          string  `json:"sha"`
		URL          string  `json:"url"`
		HashDistance int     `json:"hash_distance"`
		Color        float64 `json:"color_distance"`
		Caption      string  `json:"caption"`
		Basis        string  `json:"caption_basis"`
	} `json:"matches"`
}

func similarTo(t *testing.T, h http.Handler, sha string) similarResp {
	t.Helper()
	w := asKey(t, h, "", "GET", "/api/memory/image?similar_to="+sha, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("similar_to: %d %s", w.Code, w.Body)
	}
	var out similarResp
	decode(t, w, &out)
	return out
}

func probeSimilar(t *testing.T, h http.Handler, data []byte) similarResp {
	t.Helper()
	w := asKey(t, h, "", "POST", "/api/memory/image/similar", map[string]any{
		"data_base64": base64.StdEncoding.EncodeToString(data)})
	if w.Code != http.StatusOK {
		t.Fatalf("probe: %d %s", w.Code, w.Body)
	}
	var out similarResp
	decode(t, w, &out)
	return out
}

func containsSHA(out similarResp, sha string) (int, bool) {
	for i, m := range out.Matches {
		if m.SHA == sha {
			return i, true
		}
	}
	return -1, false
}

// The acceptance case: a near-duplicate is found by pixels, with no caption on
// the stored picture and no caption on the probe.
func TestMemoryImageSimilarFindsUncaptionedNearDuplicate(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	orig := storedImage(t, h, gradientPNG(t, 160, 120, 255), "", "photos")
	if orig.CaptionBasis != "none" || orig.Searchable {
		t.Fatalf("uncaptioned picture reported as %q searchable=%v", orig.CaptionBasis, orig.Searchable)
	}
	resized := storedImage(t, h, gradientPNG(t, 80, 60, 255), "resized copy", "photos")
	storedImage(t, h, noisePNG(t, 160, 120, 5), "unrelated", "photos")

	out := similarTo(t, h, orig.Image.SHA)
	if _, ok := containsSHA(out, orig.Image.SHA); ok {
		t.Fatal("a picture was listed as similar to itself")
	}
	i, ok := containsSHA(out, resized.Image.SHA)
	if !ok {
		t.Fatalf("resized copy not found: %+v", out)
	}
	if out.Matches[i].Caption != "resized copy" || out.Matches[i].Basis != "stated" {
		t.Fatalf("caption not carried: %+v", out.Matches[i])
	}
	if out.Matches[0].SHA != resized.Image.SHA {
		t.Fatalf("resized copy is not the nearest match: %+v", out.Matches)
	}

	// A JPEG re-encode of the uncaptioned original, probed without storing.
	before := asKey(t, h, "", "GET", "/api/memory?q=photos", nil).Body.String()
	p := probeSimilar(t, h, jpegOf(t, gradientPNG(t, 160, 120, 255)))
	if _, ok := containsSHA(p, orig.Image.SHA); !ok {
		t.Fatalf("probe did not find the uncaptioned original: %+v", p)
	}
	if after := asKey(t, h, "", "GET", "/api/memory?q=photos", nil).Body.String(); after != before {
		t.Fatal("the similarity probe wrote to memory")
	}
	if p.Method == "" || p.MaxDistance != defaultSimilarMax {
		t.Fatalf("response header wrong: %+v", p)
	}
}

// A picture behind a private note is neither compared nor returned, and a
// probe for it answers as for an absent picture.
func TestMemoryImageSimilarHidesPrivateAndVisibilityTagged(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	hidden := storedImage(t, h, gradientPNG(t, 160, 120, 200), "private floor plan", "secret")
	tagged := storedImage(t, h, gradientPNG(t, 96, 72, 90), "tagged sensitive", "vis")
	visible := storedImage(t, h, gradientPNG(t, 120, 90, 230), "visible plan", "open")

	note, err := s.Vault.Read(hidden.Path)
	if err != nil {
		t.Fatal(err)
	}
	fm := note.Frontmatter.Clone()
	fm.Set("private", true)
	if _, err := s.Vault.Write(hidden.Path, note.Body, fm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Upsert(hidden.Path); err != nil {
		t.Fatal(err)
	}
	if err := s.Index.DB.Exec("UPDATE memory_entries SET visibility='sensitive' WHERE image=?", tagged.Image.SHA); err != nil {
		t.Fatal(err)
	}

	probe := probeSimilar(t, h, gradientPNG(t, 160, 120, 200))
	if _, ok := containsSHA(probe, hidden.Image.SHA); ok {
		t.Fatal("a private picture was returned by the upload probe")
	}
	if _, ok := containsSHA(probe, tagged.Image.SHA); ok {
		t.Fatal("a sensitive-tagged picture was returned by the upload probe")
	}
	if c := asKey(t, h, "", "GET", "/api/memory/image?similar_to="+hidden.Image.SHA, nil).Code; c != http.StatusNotFound {
		t.Fatalf("similar_to a hidden picture answered %d", c)
	}
	if c := asKey(t, h, "", "GET", "/api/memory/image?similar_to="+strings.Repeat("e", 64), nil).Code; c != http.StatusNotFound {
		t.Fatalf("similar_to an absent picture answered %d", c)
	}
	out := similarTo(t, h, visible.Image.SHA)
	for _, m := range out.Matches {
		if m.SHA == hidden.Image.SHA || m.SHA == tagged.Image.SHA {
			t.Fatalf("hidden picture leaked into similar_to results: %+v", m)
		}
	}
}

func TestMemoryImageSimilarRejectsBadThreshold(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	got := storedImage(t, h, gradientPNG(t, 64, 48, 255), "x", "t")
	for _, q := range []string{"max=-1", "max=65", "max=ten"} {
		c := asKey(t, h, "", "GET", "/api/memory/image?similar_to="+got.Image.SHA+"&"+q, nil).Code
		if c != http.StatusBadRequest {
			t.Fatalf("%s answered %d, want 400", q, c)
		}
	}
	if c := asKey(t, h, "", "GET", "/api/memory/image", nil).Code; c != http.StatusBadRequest {
		t.Fatalf("missing similar_to answered %d", c)
	}
}

// --- automatic captioning ---

// fakeCaptioner answers like Ollama's /api/chat and counts the requests it gets.
func fakeCaptioner(t *testing.T, reply string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"content": reply}})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func configureCaptioner(t *testing.T, s interface{ Update(map[string]string) error }, url string, allowPrivate bool) {
	t.Helper()
	patch := map[string]string{"memory_caption_url": url, "memory_caption_model": "vision-test"}
	if allowPrivate {
		patch["memory_caption_allow_private"] = "1"
	}
	if err := s.Update(patch); err != nil {
		t.Fatal(err)
	}
}

// Off by default: no captioner configured, so an uncaptioned upload stays
// uncaptioned and nothing is sent anywhere.
func TestMemoryImageAutoCaptionOffByDefault(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	got := storedImage(t, h, gradientPNG(t, 64, 48, 255), "", "t")
	if got.CaptionBasis != "none" || got.Searchable {
		t.Fatalf("default upload: basis=%q searchable=%v", got.CaptionBasis, got.Searchable)
	}
}

// Opt-in: the configured captioner writes the caption, marked inferred, and
// recall finds the picture by the words the model chose.
func TestMemoryImageAutoCaptionOptIn(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	srv, hits := fakeCaptioner(t, "A rack of network switches with blue cables.")
	configureCaptioner(t, s.Settings, srv.URL, true)

	got := storedImage(t, h, gradientPNG(t, 64, 48, 255), "", "racks")
	if hits.Load() != 1 {
		t.Fatalf("captioner called %d times, want 1", hits.Load())
	}
	if got.CaptionBasis != "inferred" || !got.Searchable || got.Text != "A rack of network switches with blue cables." {
		t.Fatalf("inferred caption not stored: %+v", got)
	}
	found := recallEntries(t, h, "network switches")
	if len(found) == 0 {
		t.Fatal("picture with an inferred caption is not recalled by that caption")
	}
}

// A human caption always wins: the captioner is never called with the picture,
// and the caption and basis are the caller's own.
func TestMemoryImageHumanCaptionWinsOverCaptioner(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	srv, hits := fakeCaptioner(t, "model guess")
	configureCaptioner(t, s.Settings, srv.URL, true)

	got := storedImage(t, h, gradientPNG(t, 64, 48, 255), "my own words", "t")
	if hits.Load() != 0 {
		t.Fatalf("captioner was called %d times for a captioned upload", hits.Load())
	}
	if got.Text != "my own words" || got.CaptionBasis != "stated" {
		t.Fatalf("human caption replaced: %+v", got)
	}
}

// A captioner that fails, or that cannot be reached, leaves the upload
// succeeding and uncaptioned, exactly as if none were configured.
func TestMemoryImageCaptionerFailureIsNonFatal(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	// Loopback without memory_caption_allow_private: refused at connect time.
	srv, hits := fakeCaptioner(t, "unreachable")
	configureCaptioner(t, s.Settings, srv.URL, false)

	got := storedImage(t, h, gradientPNG(t, 64, 48, 255), "", "t")
	if hits.Load() != 0 {
		t.Fatalf("refused captioner still received %d request(s)", hits.Load())
	}
	if got.CaptionBasis != "none" || got.Searchable || got.Text != "image (uncaptioned)" {
		t.Fatalf("failed caption not stored as uncaptioned: %+v", got)
	}
}
