package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/memimage"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// testPNG is a real, valid 4x4 PNG from the standard library. Each call with a
// different colour gives different bytes, so different content addresses.
func testPNG(t *testing.T, c uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := range img.Pix {
		img.Pix[i] = c
	}
	img.Set(0, 0, color.RGBA{R: c, G: 255, B: 0, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type imageResp struct {
	Path         string `json:"path"`
	ID           string `json:"id"`
	Text         string `json:"text"`
	CaptionBasis string `json:"caption_basis"`
	Searchable   bool   `json:"searchable"`
	Image        struct {
		SHA   string `json:"sha"`
		MIME  string `json:"mime"`
		Bytes int64  `json:"bytes"`
		URL   string `json:"url"`
	} `json:"image"`
}

func postImageJSON(t *testing.T, h http.Handler, data []byte, caption, topic string) *httptest.ResponseRecorder {
	t.Helper()
	return asKey(t, h, "", "POST", "/api/memory/image", map[string]any{
		"data_base64": base64.StdEncoding.EncodeToString(data),
		"caption":     caption, "topic": topic})
}

func storedImage(t *testing.T, h http.Handler, data []byte, caption, topic string) imageResp {
	t.Helper()
	w := postImageJSON(t, h, data, caption, topic)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/memory/image: %d %s", w.Code, w.Body)
	}
	var out imageResp
	decode(t, w, &out)
	return out
}

func recallEntries(t *testing.T, h http.Handler, q string) []entryOut {
	t.Helper()
	w := asKey(t, h, "", "GET", "/api/memory?q="+strings.ReplaceAll(q, " ", "+"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("recall %q: %d %s", q, w.Code, w.Body)
	}
	var out []entryOut
	decode(t, w, &out)
	return out
}

func TestMemoryImageCaptionedPictureIsRecalledByCaption(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	raw := testPNG(t, 10)
	got := storedImage(t, h, raw, "rack B wiring diagram", "infra")

	if got.Image.SHA != memimage.Sum(raw) || got.Image.MIME != "image/png" || !got.Searchable || got.CaptionBasis != memory.CaptionStated {
		t.Fatalf("response: %+v", got)
	}
	if _, p, ok := memimage.Find(s.Vault.Root, got.Image.SHA); !ok || filepath.Dir(p) != filepath.Join(s.Vault.Root, memimage.Dir) {
		t.Fatal("picture was not stored in the attachment directory")
	}

	hits := recallEntries(t, h, "wiring diagram")
	var found *entryOut
	for i := range hits {
		if hits[i].ID == got.ID {
			found = &hits[i]
		}
	}
	if found == nil {
		t.Fatalf("caption not recalled: %+v", hits)
	}
	if found.Image == nil || found.Image.SHA != got.Image.SHA || found.Image.MIME != "image/png" ||
		found.Image.Bytes != int64(len(raw)) || found.Image.URL != "/api/memory/image/"+got.Image.SHA {
		t.Fatalf("recall image ref: %+v", found.Image)
	}

	w := asKey(t, h, "", "GET", "/api/memory/image/"+got.Image.SHA, nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" ||
		!bytes.Equal(w.Body.Bytes(), raw) {
		t.Fatalf("GET image: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("image response lacks the sniffing and script defences")
	}
}

func TestMemoryImageWithoutCaptionIsStoredButNeverRecalled(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	raw := testPNG(t, 40)
	got := storedImage(t, h, raw, "", "")
	if got.Searchable || got.CaptionBasis != memory.CaptionNone || got.Text != uncaptionedText {
		t.Fatalf("uncaptioned picture claimed to be searchable: %+v", got)
	}
	for _, q := range []string{"uncaptioned", "image"} {
		for _, e := range recallEntries(t, h, q) {
			if e.Text == uncaptionedText {
				t.Fatalf("placeholder text recalled for %q", q)
			}
		}
	}
	// The bytes are still kept and still served to a caller who can see the entry.
	w := asKey(t, h, "", "GET", "/api/memory/image/"+got.Image.SHA, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("uncaptioned picture not served: %d", w.Code)
	}
}

func TestMemoryImageRejectsWhatIsNotAPicture(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	cases := map[string]map[string]any{
		"svg":        {"data_base64": base64.StdEncoding.EncodeToString([]byte("<svg xmlns='http://www.w3.org/2000/svg'/>")), "caption": "x"},
		"not base64": {"data_base64": "!!!not base64!!!", "caption": "x"},
		"empty":      {"data_base64": "", "caption": "x"},
		"long cap":   {"data_base64": base64.StdEncoding.EncodeToString(testPNG(t, 1)), "caption": strings.Repeat("c", maxCaptionChars+1)},
	}
	for name, body := range cases {
		w := asKey(t, h, "", "POST", "/api/memory/image", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 (%s)", name, w.Code, w.Body)
		}
	}
	if w := asKey(t, h, "", "GET", "/api/memory/image/"+strings.Repeat("0", 64), nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown hash: %d", w.Code)
	}
	if w := asKey(t, h, "", "GET", "/api/memory/image/..%2F..%2Fetc", nil); w.Code != http.StatusNotFound {
		t.Errorf("path-shaped hash: %d", w.Code)
	}
}

func TestMemoryImageSizeCapIsEnforced(t *testing.T) {
	t.Setenv(imageMaxEnv, "64")
	_, h := testServer(t)
	big := testPNG(t, 200) // comfortably over 64 bytes
	if len(big) <= 64 {
		t.Skip("fixture too small for the cap test")
	}
	if w := postImageJSON(t, h, big, "too big", ""); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap upload: %d", w.Code)
	}
}

func TestMemoryImageMultipartUpload(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	raw := testPNG(t, 77)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "photo.png")
	fw.Write(raw)
	mw.WriteField("caption", "the lab bench photo")
	mw.Close()
	req := httptest.NewRequest("POST", "/api/memory/image", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("multipart: %d %s", w.Code, w.Body)
	}
	var out imageResp
	decode(t, w, &out)
	if out.Image.SHA != memimage.Sum(raw) || out.Text != "the lab bench photo" {
		t.Fatalf("multipart response: %+v", out)
	}
}

// A picture is served only while a visible, non-private entry refers to it. A
// hash alone is not a capability.
func TestMemoryImageHiddenWhenNoVisibleEntryRefersToIt(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	got := storedImage(t, h, testPNG(t, 90), "private floor plan", "secret")
	get := func() int {
		return asKey(t, h, "", "GET", "/api/memory/image/"+got.Image.SHA, nil).Code
	}
	if c := get(); c != http.StatusOK {
		t.Fatalf("visible picture: %d", c)
	}

	// Mark the note that holds the reference as private.
	note, err := s.Vault.Read(got.Path)
	if err != nil {
		t.Fatal(err)
	}
	fm := note.Frontmatter.Clone()
	fm.Set("private", true)
	if _, err := s.Vault.Write(got.Path, note.Body, fm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Upsert(got.Path); err != nil {
		t.Fatal(err)
	}
	if c := get(); c != http.StatusNotFound {
		t.Fatalf("picture behind a private entry answered %d", c)
	}
	if c := asKey(t, h, "", "GET", "/api/memory/image/"+strings.Repeat("e", 64), nil).Code; c != http.StatusNotFound {
		t.Fatalf("absent picture answered %d", c)
	}
}

func TestMemoryImageIsCollectedWithItsLastReference(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	raw := testPNG(t, 120)
	first := storedImage(t, h, raw, "same photo, first note", "dup")
	second := storedImage(t, h, raw, "same photo, second note", "dup")
	if first.Image.SHA != second.Image.SHA {
		t.Fatal("identical bytes must share one address")
	}
	_, p, ok := memimage.Find(s.Vault.Root, first.Image.SHA)
	if !ok {
		t.Fatal("picture missing after write")
	}

	// A soft forget strikes the fact through and keeps it, so keeps the picture.
	if err := s.retractEntry(first.Path, first.ID, "human"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("soft forget must keep the picture its struck-through fact still refers to")
	}

	// Hard-forgetting one of two references keeps the picture for the other.
	if err := s.removeEntry(first.Path, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("picture removed while another fact still refers to it")
	}

	// The last reference goes, so the bytes go.
	if err := s.removeEntry(second.Path, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("picture survived its last reference being forgotten")
	}
}

func exportRecords(t *testing.T, h http.Handler, query string) []map[string]any {
	t.Helper()
	w := asKey(t, h, "", "GET", "/api/memory/export?format=jsonl"+query, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export: %d %s", w.Code, w.Body)
	}
	var out []map[string]any
	for i, line := range strings.Split(strings.TrimSpace(w.Body.String()), "\n") {
		if i == 0 {
			continue // header
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestMemoryImageExportsAndImportsItsBytes(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	raw := testPNG(t, 150)
	got := storedImage(t, h, raw, "whiteboard after the design review", "review")

	recs := exportRecords(t, h, "")
	var att map[string]any
	for _, r := range recs {
		if r["image"] == got.Image.SHA {
			att, _ = r["attachment"].(map[string]any)
		}
	}
	if att == nil || att["sha"] != got.Image.SHA || att["mime"] != "image/png" {
		t.Fatalf("export did not carry the picture: %+v", recs)
	}

	md := asKey(t, h, "", "GET", "/api/memory/export?format=markdown", nil).Body.String()
	if !strings.Contains(md, "![whiteboard after the design review](data:image/png;base64,") {
		t.Fatal("markdown export does not embed the picture")
	}

	// Lose the picture entirely, then restore it from the export.
	if err := s.removeEntry(got.Path, got.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := memimage.Find(s.Vault.Root, got.Image.SHA); ok {
		t.Fatal("picture should be gone before the import")
	}
	// The removed fact is no longer exported, so the record comes from the
	// export taken before the loss.
	var saved []byte
	for _, r := range recs {
		if r["image"] == got.Image.SHA {
			saved, _ = json.Marshal(r)
		}
	}
	jsonl := []byte(`{"format":"grimoire-memory","version":1,"exported":"2026-08-14T09:00:00Z","count":1}` + "\n" + string(saved) + "\n")
	res := importFile(t, h, "", "from=grimoire", jsonl)
	if res.Written != 1 || res.Failed != 0 {
		t.Fatalf("import: %+v", res)
	}
	if _, _, ok := memimage.Find(s.Vault.Root, got.Image.SHA); !ok {
		t.Fatal("import did not restore the picture")
	}
	if c := asKey(t, h, "", "GET", "/api/memory/image/"+got.Image.SHA, nil).Code; c != http.StatusOK {
		t.Fatalf("restored picture not served: %d", c)
	}
}

func TestMemoryImageImportRefusesForgedOrDanglingPictures(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	realPNG := testPNG(t, 160)
	otherPNG := testPNG(t, 161)
	realSHA := memimage.Sum(realPNG)
	header := `{"format":"grimoire-memory","version":1,"exported":"2026-08-14T09:00:00Z","count":2}`

	forged := map[string]any{"id": "0123456789ab", "text": "a forged picture", "stamp": "2026-08-14 09:00",
		"image": realSHA, "caption_basis": "stated",
		"attachment": map[string]any{"sha": realSHA, "mime": "image/png", "data": otherPNG}}
	dangling := map[string]any{"id": "fedcba987654", "text": "points nowhere", "stamp": "2026-08-14 09:00",
		"image": strings.Repeat("9", 64), "caption_basis": "stated"}
	b1, _ := json.Marshal(forged)
	b2, _ := json.Marshal(dangling)
	jsonl := []byte(header + "\n" + string(b1) + "\n" + string(b2) + "\n")

	res := importFile(t, h, "", "from=grimoire", jsonl)
	if res.Written != 0 || len(res.Skipped) != 2 {
		t.Fatalf("forged and dangling pictures were not refused: %+v", res)
	}
	if _, _, ok := memimage.Find(s.Vault.Root, realSHA); ok {
		t.Fatal("bytes that do not match their address were stored")
	}
}

// A fact tagged vis=private or vis=sensitive hides its picture too: the hash
// must not be a way around the visibility tag.
func TestMemoryImageHiddenByVisibilityTag(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	got := storedImage(t, h, testPNG(t, 77), "hidden by tag", "vis")
	get := func() int {
		return asKey(t, h, "", "GET", "/api/memory/image/"+got.Image.SHA, nil).Code
	}
	if c := get(); c != http.StatusOK {
		t.Fatalf("visible picture: %d", c)
	}
	if err := s.Index.DB.Exec("UPDATE memory_entries SET visibility='sensitive' WHERE image=?", got.Image.SHA); err != nil {
		t.Fatal(err)
	}
	if c := get(); c != http.StatusNotFound {
		t.Fatalf("picture behind a vis-tagged fact answered %d", c)
	}
}
