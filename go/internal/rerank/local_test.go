package rerank

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/embed"
)

// ---------------------------------------------------------------- helpers

func TestOrderIsStableAndPutsNaNLast(t *testing.T) {
	got := Order([]float32{1, 3, float32(math.NaN()), 3, -2})
	want := []int{1, 3, 0, 4, 2}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Order = %v, want %v", got, want)
	}
}

func TestScoresFromOrderKeepsUnrankedDocuments(t *testing.T) {
	s := ScoresFromOrder([]int{2, 0, 2, 9, -1}, 4)
	if got := fmt.Sprint(Order(s)); got != "[2 0 1 3]" {
		t.Fatalf("order from scores %v = %s", s, got)
	}
}

func TestAdapt(t *testing.T) {
	r := Adapt("llm", func(_ context.Context, q string, docs []string) ([]float32, error) {
		return ScoresFromOrder([]int{1}, len(docs)), nil
	})
	s, err := r.Score(context.Background(), "q", []string{"a", "b"})
	if err != nil || r.Name() != "llm" || s[1] <= s[0] {
		t.Fatalf("adapter: %v %v %s", s, err, r.Name())
	}
}

// ---------------------------------------------------------------- truncation

func TestTruncateLongestFirst(t *testing.T) {
	for _, c := range []struct{ a, b, target, wa, wb int }{
		{5, 5, 20, 5, 5},        // fits
		{10, 500, 253, 10, 243}, // short side kept whole
		{500, 10, 253, 243, 10},
		{300, 400, 253, 126, 127}, // both long: even split, odd token to the second
		{400, 300, 253, 127, 126},
		{300, 300, 61, 30, 31},
		{0, 100, 10, 0, 10},
		{100, 0, 10, 10, 0},
	} {
		a, b := truncateLongestFirst(c.a, c.b, c.target)
		if a != c.wa || b != c.wb {
			t.Errorf("truncate(%d,%d,%d) = %d,%d want %d,%d", c.a, c.b, c.target, a, b, c.wa, c.wb)
		}
	}
}

// ---------------------------------------------------------------- safetensors

func writeSafetensors(t *testing.T, path string, tensors map[string]tensorInfo, data []byte) {
	t.Helper()
	hdr, err := json.Marshal(tensors)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint64(len(hdr)))
	buf.Write(hdr)
	buf.Write(data)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSafetensorsDTypes(t *testing.T) {
	var data bytes.Buffer
	f32 := []float32{1.5, -2, 0, 65504}
	binary.Write(&data, binary.LittleEndian, f32)
	// fp16: 1.5, -2, smallest subnormal, 65504 (max), +Inf
	binary.Write(&data, binary.LittleEndian, []uint16{0x3e00, 0xc000, 0x0001, 0x7bff, 0x7c00})
	// bf16: 1.5, -2
	binary.Write(&data, binary.LittleEndian, []uint16{0x3fc0, 0xc000})
	path := filepath.Join(t.TempDir(), "m.safetensors")
	writeSafetensors(t, path, map[string]tensorInfo{
		"a": {DType: "F32", Shape: []int{2, 2}, Offsets: []int64{0, 16}},
		"h": {DType: "F16", Shape: []int{5}, Offsets: []int64{16, 26}},
		"b": {DType: "BF16", Shape: []int{2}, Offsets: []int64{26, 30}},
		"i": {DType: "I64", Shape: []int{0}, Offsets: []int64{30, 30}},
	}, data.Bytes())
	st, err := openSafetensors(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if a, err := st.float32s("a", 2, 2); err != nil || fmt.Sprint(a) != fmt.Sprint(f32) {
		t.Fatalf("F32: %v %v", a, err)
	}
	h, err := st.float32s("h", 5)
	if err != nil {
		t.Fatal(err)
	}
	if h[0] != 1.5 || h[1] != -2 || h[2] != float32(math.Ldexp(1, -24)) || h[3] != 65504 || !math.IsInf(float64(h[4]), 1) {
		t.Fatalf("F16: %v", h)
	}
	if b, err := st.float32s("b", 2); err != nil || b[0] != 1.5 || b[1] != -2 {
		t.Fatalf("BF16: %v %v", b, err)
	}
	if _, err := st.float32s("a", 4); err == nil {
		t.Error("shape mismatch accepted")
	}
	if _, err := st.float32s("i", 0); err == nil {
		t.Error("integer tensor accepted")
	}
	if _, err := st.float32s("missing", 1); err == nil {
		t.Error("missing tensor accepted")
	}
}

func TestSafetensorsRejectsCorruptHeaders(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short")
	os.WriteFile(short, []byte{1, 2}, 0o644)
	huge := filepath.Join(dir, "huge")
	os.WriteFile(huge, binary.LittleEndian.AppendUint64(nil, 1<<40), 0o644)
	over := filepath.Join(dir, "over")
	writeSafetensors(t, over, map[string]tensorInfo{
		"a": {DType: "F32", Shape: []int{4}, Offsets: []int64{0, 1 << 20}},
	}, make([]byte, 16))
	for _, p := range []string{short, huge, over} {
		if _, err := openSafetensors(p); err == nil {
			t.Errorf("%s: accepted", filepath.Base(p))
		}
	}
}

// ---------------------------------------------------------------- fetch

// hub serves model files the way the HF hub does: a redirect that carries
// the digest, then the bytes without it.
func hub(t *testing.T, files map[string][]byte, digest func(name string, b []byte) string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name, ok := strings.CutPrefix(r.URL.Path, "/cdn/"); ok {
			w.Write(files[name])
			return
		}
		name := filepath.Base(r.URL.Path)
		b, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Linked-Etag", `"`+digest(name, b)+`"`)
		http.Redirect(w, r, "/cdn/"+name, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	old := embed.HubBase
	embed.HubBase = srv.URL
	t.Cleanup(func() { embed.HubBase = old })
	return srv
}

func fakeModelFiles() map[string][]byte {
	return map[string][]byte{
		"config.json":       []byte(`{"model_type":"bert"}`),
		"tokenizer.json":    []byte(`{"model":{"type":"WordPiece"}}`),
		"model.safetensors": bytes.Repeat([]byte{7}, 4096),
	}
}

func sha256Hex(_ string, b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func gitBlobHex(_ string, b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func TestFetchVerifiesHubDigests(t *testing.T) {
	files := fakeModelFiles()
	hub(t, files, func(name string, b []byte) string {
		if name == "model.safetensors" {
			return sha256Hex(name, b) // large files: sha256
		}
		return gitBlobHex(name, b) // small files: git blob id
	})
	cache := t.TempDir()
	dir, err := ensureModel("org/tiny", cache, true)
	if err != nil {
		t.Fatal(err)
	}
	if dir != ModelDir(cache, "org/tiny") || FindModel("org/tiny", cache) != dir {
		t.Fatalf("model stored at %s", dir)
	}
	for name, want := range files {
		got, _ := os.ReadFile(filepath.Join(dir, name))
		if !bytes.Equal(got, want) {
			t.Errorf("%s corrupted", name)
		}
	}
}

func TestFetchRejectsAChecksumMismatchAndLeavesNothing(t *testing.T) {
	hub(t, fakeModelFiles(), func(string, []byte) string { return strings.Repeat("ab", 32) })
	cache := t.TempDir()
	if _, err := ensureModel("org/tiny", cache, true); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(ModelDir(cache, "org/tiny"), "*"))
	if len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}
}

func TestMissingModelIsOfflineSafe(t *testing.T) {
	srv := hub(t, fakeModelFiles(), sha256Hex)
	hits := 0
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ })
	cache := t.TempDir()
	if _, err := ensureModel("org/tiny", cache, false); !errors.Is(err, ErrModelMissing) {
		t.Fatalf("download disallowed: err = %v", err)
	}
	t.Setenv("HF_HUB_OFFLINE", "1")
	if _, err := ensureModel("org/tiny", cache, true); !errors.Is(err, ErrModelMissing) {
		t.Fatalf("offline: err = %v", err)
	}
	if hits != 0 {
		t.Fatalf("made %d requests while forbidden to", hits)
	}
	// and the local reranker turns that into an error, not a hang or panic
	l := NewLocal(LocalConfig{Model: "org/tiny", CacheDir: cache, AllowDownload: true})
	if _, err := l.Score(context.Background(), "q", []string{"d"}); err == nil {
		t.Fatal("scored without a model")
	}
}

func TestLocalPathModels(t *testing.T) {
	dir := t.TempDir()
	if FindModel(dir, "") != "" {
		t.Fatal("empty dir accepted")
	}
	for name, b := range fakeModelFiles() {
		os.WriteFile(filepath.Join(dir, name), b, 0o644)
	}
	if FindModel(dir, "") != dir || Downloadable(dir, t.TempDir()) {
		t.Fatal("local directory not used as-is")
	}
}
