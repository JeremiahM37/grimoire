package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Metadata of a sealed fact (evidence and check) and the paths that could
// hand a sealed fact to a model or an embedder. Canaries are phrases no other
// test writes, so finding one anywhere is a leak.
const (
	sealEvCanary    = "evidence-canary-tangerine"
	sealCheckCanary = "check-canary-marigold"
	sealFixed       = "zeppelin-quartz-harbor forecast eight"
)

// rememberSealedMeta writes a sealed fact with evidence and a check, as the
// caller (key "" means the unrestricted local caller).
func rememberSealedMeta(t *testing.T, h http.Handler, key string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if _, ok := body["visibility"]; !ok {
		body["visibility"] = "sealed"
	}
	if _, ok := body["topic"]; !ok {
		body["topic"] = "seal-test"
	}
	if key == "" {
		return do(t, h, "POST", "/api/memory", body)
	}
	req := requestFor(t, "POST", "/api/memory", body)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// postAs writes a plain or explicit memory as the given account.
func postAs(t *testing.T, h http.Handler, key string, body map[string]any) *httptest.ResponseRecorder {
	req := requestFor(t, "POST", "/api/memory", body)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// indexMetadata returns every evidence and check value the index holds.
func indexMetadata(t *testing.T, s *Server) string {
	t.Helper()
	rows, err := s.Index.DB.Query("SELECT evidence, chk FROM memory_entries")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var ev, chk string
		if err := rows.Scan(&ev, &chk); err != nil {
			t.Fatal(err)
		}
		b.WriteString(ev + "\x00" + chk + "\n")
	}
	return b.String()
}

func TestSealedEvidenceAndCheckAreNeverClear(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	adminKey := makeUser(t, s, h, "", "root", "admin")
	aliceKey := makeUser(t, s, h, adminKey, "alice", "member")
	bobKey := makeUser(t, s, h, adminKey, "bob", "member")

	w := rememberSealedMeta(t, h, aliceKey, map[string]any{
		"text": sealCanary, "infer": false,
		"evidence": []string{sealEvCanary + ".md"}, "check": "cat " + sealCheckCanary,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("remember = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Path string `json:"path"`
		ID   string `json:"id"`
	}
	decode(t, w, &out)

	// Nothing that can be read without the key carries either canary.
	for name, got := range map[string]string{
		"note file":  noteBytes(t, s, out.Path),
		"index":      indexMetadata(t, s),
		"write":      w.Body.String(),
		"export":     do(t, h, "GET", "/api/memory/export?include_private=1", nil).Body.String(),
		"search ev":  do(t, h, "GET", "/api/search?q="+sealEvCanary, nil).Body.String(),
		"search chk": do(t, h, "GET", "/api/search?q="+sealCheckCanary, nil).Body.String(),
	} {
		if strings.Contains(got, sealEvCanary) || strings.Contains(got, sealCheckCanary) {
			t.Errorf("%s carries the sealed evidence or check: %s", name, got)
		}
	}
	// The bullet still says it is sealed, and nothing else about its metadata is clear.
	raw := noteBytes(t, s, out.Path)
	if !strings.Contains(raw, "vis=sealed") || strings.Contains(raw, "ev=") || strings.Contains(raw, "chk=") {
		t.Fatalf("bullet trailer is wrong:\n%s", raw)
	}

	recall := func(key string) string {
		t.Helper()
		req := requestFor(t, "GET", "/api/memory?q=forecast&include_private=1", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("recall = %d: %s", w.Code, w.Body)
		}
		return w.Body.String()
	}
	if got := recall(aliceKey); !strings.Contains(got, sealEvCanary) {
		t.Errorf("owner did not get the evidence back: %s", got)
	}
	for name, key := range map[string]string{"bob": bobKey, "root": adminKey} {
		if got := recall(key); strings.Contains(got, sealEvCanary) || strings.Contains(got, sealCheckCanary) {
			t.Errorf("%s read the sealed evidence or check: %s", name, got)
		}
	}
}

// A sealed fact written before envelopes kept its ciphertext as bare text and
// its evidence in clear on the bullet. It must still open for its owner, and
// the index must not hold its evidence.
func TestLegacyBareCiphertextStillOpens(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	const rel = "memory/legacy-seal.md"
	const stamp = "2026-10-01 09:00"
	const agent = "claude"
	var keyID, token string
	err := s.Secrets.WithSessionKey(func(key []byte) error {
		var e error
		keyID, token, e = s.sealKeys().Seal(key, "local", sealFixed)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	bullet := memory.Entry{
		ID: memory.DeriveID(stamp, agent, token), Text: memory.SealedPlaceholder,
		Agent: agent, Stamp: stamp, Visibility: memory.VisSealed,
		Owner: "local", KeyID: keyID, Sealed: token,
		Evidence: sealEvCanary, Check: sealCheckCanary,
	}
	body := "# Memory: legacy\n\n" + bullet.Format() + "\n"
	fm := markdown.NewFrontmatter()
	fm.Set("memory", true)
	if _, err := s.Vault.Write(rel, body, fm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Upsert(rel); err != nil {
		t.Fatal(err)
	}

	req := requestFor(t, "GET", "/api/memory?q=forecast&include_private=1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	got := w.Body.String()
	if !strings.Contains(got, sealFixed) {
		t.Fatalf("owner could not open the legacy sealed fact: %s", got)
	}
	if !strings.Contains(got, sealEvCanary) {
		t.Errorf("owner lost the legacy evidence that is still on the bullet: %s", got)
	}
	if strings.Contains(indexMetadata(t, s), sealEvCanary) {
		t.Error("the index holds the evidence of a sealed bullet")
	}
}

// recordingFake is an LLM and embedder stand-in. It records every string it
// is given, and answers every model call with an error so the callers take
// their deterministic fallbacks.
type recordingFake struct {
	mu   sync.Mutex
	seen []string
}

func (r *recordingFake) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, s)
}

func (r *recordingFake) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.seen, "\n")
}

// Embed records what it is given and delegates the vectors to the test model.
type recordingEmbedder struct {
	rec   *recordingFake
	inner index.Embedder
}

func (e recordingEmbedder) Embed(texts []string) [][]float32 {
	for _, t := range texts {
		e.rec.add(t)
	}
	return e.inner.Embed(texts)
}
func (e recordingEmbedder) Signature() string { return e.inner.Signature() }
func (e recordingEmbedder) Dim() int          { return e.inner.Dim() }

func TestSealedTextNeverReachesAModelOrEmbedder(t *testing.T) {
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	adminKey := makeUser(t, s, h, "", "root", "admin")
	aliceKey := makeUser(t, s, h, adminKey, "alice", "member")
	bobKey := makeUser(t, s, h, adminKey, "bob", "member")

	rec := &recordingFake{}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.add(r.URL.Path + "\n" + string(raw))
		http.Error(w, "fake model: no answer", http.StatusInternalServerError)
	}))
	t.Cleanup(model.Close)
	if err := s.Settings.Update(map[string]string{
		"llm": "ollama", "ollama_url": model.URL,
		"decision_url": model.URL, "decision_model": "fake",
	}); err != nil {
		t.Fatal(err)
	}
	s.Index.Emb = recordingEmbedder{rec: rec, inner: s.Index.Emb}

	// A plain fact in the same note, so the model path has candidates to see.
	var cued struct {
		ID string `json:"id"`
	}
	if w := postAs(t, h, aliceKey, map[string]any{"topic": "seal-test", "infer": false,
		"text": "release channel is nightly builds", "cues": []string{"ship the nightly build"}}); w.Code != http.StatusCreated {
		t.Fatalf("plain write = %d: %s", w.Code, w.Body)
	} else {
		decode(t, w, &cued)
	}

	// 1. A fresh sealed write, with extraction left on. It must not be sent
	// anywhere, and its response must not echo it.
	w := rememberSealedMeta(t, h, aliceKey, map[string]any{"text": sealCanary,
		"infer": true, "evidence": []string{sealEvCanary}, "check": sealCheckCanary})
	if w.Code != http.StatusCreated {
		t.Fatalf("fresh sealed write = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), sealCanary) || strings.Contains(w.Body.String(), sealEvCanary) {
		t.Errorf("fresh sealed write echoed the fact: %s", w.Body)
	}
	var fresh struct {
		Path string `json:"path"`
		ID   string `json:"id"`
	}
	decode(t, w, &fresh)

	// 2. An owner's sealed correction of the sealed fact, aimed by id, with
	// the text the owner knows as expected_text. Replacing it must not send
	// either text anywhere.
	w = rememberSealedMeta(t, h, aliceKey, map[string]any{"text": sealFixed,
		"target_id": fresh.ID, "target_path": fresh.Path, "expected_text": sealCanary})
	if w.Code != http.StatusCreated {
		t.Fatalf("sealed correction by the owner = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), sealFixed) || strings.Contains(w.Body.String(), sealCanary) {
		t.Errorf("sealed correction echoed the fact: %s", w.Body)
	}
	// The correction superseded the first fact, so later corrections aim at its replacement.
	var corr struct {
		ID string `json:"id"`
	}
	decode(t, w, &corr)

	// 3. A sealed fact corrected by a plain write is refused before anything
	// reads its text, and the refusal does not echo either text.
	w = postAs(t, h, aliceKey, map[string]any{"text": "plain replacement text", "topic": "seal-test", "target_id": corr.ID,
		"target_path": fresh.Path, "expected_text": "[sealed]"})
	if w.Code != http.StatusConflict {
		t.Fatalf("plain correction of a sealed fact = %d, want 409: %s", w.Code, w.Body)
	}

	// 4. Another account cannot correct the owner's sealed fact at all.
	w = rememberSealedMeta(t, h, bobKey, map[string]any{"text": "bob's replacement",
		"target_id": corr.ID, "target_path": fresh.Path, "expected_text": "[sealed]"})
	if w.Code != http.StatusConflict {
		t.Fatalf("another account's sealed correction = %d, want 409: %s", w.Code, w.Body)
	}

	// 4b. A sealed replacement of a cued plain fact, aimed by id.
	w = postAs(t, h, aliceKey, map[string]any{"text": sealFixed, "topic": "seal-test",
		"visibility": "sealed", "target_id": cued.ID, "target_path": "memory/seal-test.md",
		"expected_text": "release channel is nightly builds"})
	if w.Code != http.StatusCreated {
		t.Fatalf("sealed replacement of a cued plain fact = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), sealFixed) {
		t.Errorf("replacement echoed the fact: %s", w.Body)
	}

	// 5. A plain write with extraction and reconciliation on. Reconciliation
	// would offer the sealed facts as candidates if it could.
	w = postAs(t, h, aliceKey, map[string]any{"text": "release channel is stable builds now",
		"topic": "seal-test"})
	if w.Code != http.StatusCreated {
		t.Fatalf("plain write = %d: %s", w.Code, w.Body)
	}

	got := rec.all()
	for _, canary := range []string{sealCanary, sealFixed, sealEvCanary, sealCheckCanary} {
		if strings.Contains(got, canary) {
			t.Errorf("a model or embedder was given %q:\n%s", canary, got)
		}
	}
	// The plain path must still have reached the model, or the test proves nothing.
	if !strings.Contains(got, "release channel") {
		t.Fatalf("the fake saw no plain write; the test is not exercising the model path:\n%s", got)
	}
}

// The round trip of a plain bullet with evidence and check is untouched: its
// metadata stays on the bullet, byte for byte.
func TestPlainBulletKeepsEvidenceAndCheckOnTheBullet(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	w := do(t, h, "POST", "/api/memory", map[string]any{"text": "plain fact with evidence",
		"infer": false, "topic": "seal-test", "evidence": []string{"notes/plain.md"},
		"check": "cat notes/plain.md"})
	if w.Code != http.StatusCreated {
		t.Fatalf("remember = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Path string `json:"path"`
	}
	decode(t, w, &out)
	raw := noteBytes(t, s, out.Path)
	if !strings.Contains(raw, "ev=notes/plain.md") || !strings.Contains(raw, "check=cat") {
		t.Fatalf("an ordinary bullet lost its evidence or check:\n%s", raw)
	}
}

// A sealed replacement of a cued fact that the replay fixture has recorded. It
// checks the embedder and the response only. Measured: the replay preview does
// not run under this fixture (its builder is never reached), so this test does
// not cover the preview's own text argument; that change is by inspection.
func TestSealedCorrectionOfAReplayedFactNeverEmbedsIt(t *testing.T) {
	s, target, path := replayFixture(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	rec := &recordingFake{}
	s.Index.Emb = recordingEmbedder{rec: rec, inner: s.Index.Emb}
	h := s.Routes()
	// A cue puts the fact in the replay snapshot, which is what the preview reads.
	if c := do(t, h, "POST", "/api/memory/cues", map[string]any{"target": target, "kind": "request",
		"source": "agent", "cues": []string{"publishing the kestrel branch"}}); c.Code != http.StatusOK {
		t.Fatal(c.Body.String())
	}
	w := do(t, h, "POST", "/api/memory", map[string]any{"text": sealFixed, "topic": "kestrel",
		"visibility": "sealed", "target_id": strings.TrimPrefix(target, "fact:"), "target_path": path,
		"expected_text": pushRule})
	if w.Code != http.StatusCreated {
		t.Fatalf("sealed correction of a replayed fact = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), sealFixed) {
		t.Errorf("the response echoed the sealed fact: %s", w.Body)
	}
	if got := rec.all(); strings.Contains(got, sealFixed) {
		t.Errorf("the embedder was given the sealed fact:\n%s", got)
	}
}

// Who may correct a sealed fact is decided by the key store, not by the owner=
// label on the bullet, which anyone who can write the note can edit. Here the
// label says the caller owns the fact while the key store says otherwise.
func TestSealedCorrectionNeedsTheKeyStoreNotTheLabel(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	rel, id := rememberSealed(t, h, "", sealCanary)
	correct := func(expected string) int {
		w := do(t, h, "POST", "/api/memory", map[string]any{"text": "replacement secret text here",
			"topic": "seal-test", "visibility": "sealed",
			"target_id": id, "target_path": rel, "expected_text": expected})
		return w.Code
	}

	path := filepath.Join(s.Vault.Root, ".grimoire", "seal-keys.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kf map[string]map[string]string
	if err := json.Unmarshal(raw, &kf); err != nil {
		t.Fatal(err)
	}
	for k := range kf["owners"] {
		kf["owners"][k] = "user:someone-else"
	}
	forged, _ := json.Marshal(kf)
	if err := os.WriteFile(path, forged, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, guess := range []string{sealCanary, "[sealed]"} {
		if code := correct(guess); code != http.StatusConflict {
			t.Fatalf("correction with expected_text %q = %d, want 409 when the key store names another owner", guess, code)
		}
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := correct(sealCanary); code != http.StatusCreated {
		t.Fatalf("the real owner's correction = %d, want 201", code)
	}
	got := do(t, h, "GET", "/api/memory?q=replacement+secret&include_private=1&limit=5", nil).Body.String()
	if !strings.Contains(got, "replacement secret text here") {
		t.Errorf("the owner does not read the corrected text back: %s", got)
	}
}
