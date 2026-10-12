package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/memstore"
	"github.com/JeremiahM37/grimoire/go/internal/skillgate"
)

// procBody is a procedure whose pre check is a file that exists in a temp dir.
func procBody(t *testing.T) map[string]any {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ready.txt")
	if err := os.WriteFile(marker, []byte("ready\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"name": "Rotate the kestrel key",
		"when": "rotating the kestrel signing key",
		"steps": []string{
			"make rotate-key",
			"systemctl restart kestrel",
		},
		"pre":  []map[string]any{{"type": "file_exists", "path": marker}},
		"post": []map[string]any{{"type": "unit_active", "unit": "kestrel.service"}},
	}
}

func procStore(t *testing.T) (string, func(map[string]any) (int, map[string]any)) {
	t.Helper()
	s, h := testServer(t)
	dir := t.TempDir()
	if err := s.Settings.Update(map[string]string{"memory_canonical_dir": dir}); err != nil {
		t.Fatal(err)
	}
	post := func(body map[string]any) (int, map[string]any) {
		w := postFrom(t, h, anchorLoopback, "POST", "/api/memory/procedure", body)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	return dir, post
}

func TestProcedureIsStoredAsASkillNote(t *testing.T) {
	t.Parallel()
	dir, post := procStore(t)
	code, out := post(procBody(t))
	if code != http.StatusCreated || out["stored"] != true {
		t.Fatalf("%d %v", code, out)
	}
	notes, err := memstore.LoadDir(dir)
	if err != nil || len(notes) != 1 {
		t.Fatalf("store holds %d notes, err %v", len(notes), err)
	}
	n := notes[0]
	if n.Fields["metadata.skill_source"] != skillgate.WrittenSource {
		t.Fatalf("skill_source = %q", n.Fields["metadata.skill_source"])
	}
	spec, ok := skillgate.SpecOf(n)
	if !ok {
		t.Fatalf("no spec read back:\n%s", n.Body)
	}
	if spec.Trigger != "rotating the kestrel signing key" || len(spec.Steps) != 2 {
		t.Fatalf("spec %+v", spec)
	}
	if len(spec.Preconditions) != 1 || !strings.HasPrefix(spec.Preconditions[0], "anchor:") {
		t.Fatalf("pre %v", spec.Preconditions)
	}
	if len(spec.Postconditions) != 1 || !strings.HasPrefix(spec.Postconditions[0], "anchor:") {
		t.Fatalf("post %v", spec.Postconditions)
	}
	if spec.Steps[0] != "make rotate-key" {
		t.Fatalf("steps %q", spec.Steps)
	}
}

func TestProcedureFailingPreStoresNothing(t *testing.T) {
	t.Parallel()
	dir, post := procStore(t)
	body := procBody(t)
	body["pre"] = []map[string]any{{"type": "file_exists", "path": filepath.Join(dir, "absent.txt")}}
	code, out := post(body)
	if code != http.StatusUnprocessableEntity || out["stored"] != false {
		t.Fatalf("%d %v", code, out)
	}
	if checks, _ := out["checks"].([]any); len(checks) != 1 {
		t.Fatalf("checks %v", out["checks"])
	}
	if notes, _ := memstore.LoadDir(dir); len(notes) != 0 {
		t.Fatalf("a failed pre stored %d notes", len(notes))
	}
}

func TestProcedureValidationRefusesBadShapes(t *testing.T) {
	t.Parallel()
	_, post := procStore(t)
	cases := map[string]func(map[string]any){
		"short name":        func(b map[string]any) { b["name"] = "ab" },
		"no when":           func(b map[string]any) { b["when"] = " " },
		"one step":          func(b map[string]any) { b["steps"] = []string{"only"} },
		"no pre":            func(b map[string]any) { b["pre"] = []map[string]any{} },
		"no post":           func(b map[string]any) { b["post"] = []map[string]any{} },
		"long step":         func(b map[string]any) { b["steps"] = []string{"a", strings.Repeat("x", 401)} },
		"unknown type":      func(b map[string]any) { b["post"] = []map[string]any{{"type": "run_shell", "cmd": "rm"}} },
		"remote type":       func(b map[string]any) { b["post"] = []map[string]any{{"type": "remote", "what": "x"}} },
		"slot in pre":       func(b map[string]any) { b["pre"] = []map[string]any{{"type": "unit_active", "unit": "<unit>"}} },
		"relative path":     func(b map[string]any) { b["pre"] = []map[string]any{{"type": "file_exists", "path": "rel.txt"}} },
		"bad host":          func(b map[string]any) { b["host"] = "-rm" },
		"semicolon session": func(b map[string]any) { b["session"] = "a;b" },
	}
	for name, mut := range cases {
		b := procBody(t)
		mut(b)
		if code, out := post(b); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
}

func TestProcedureRefusesCredentialsAndNamesTheStep(t *testing.T) {
	t.Parallel()
	_, post := procStore(t)
	b := procBody(t)
	b["steps"] = []string{"make rotate-key", "curl -H 'X-Grimoire-Admin: abcdefgh1234' http://x"}
	code, out := post(b)
	if code != http.StatusBadRequest {
		t.Fatalf("%d %v", code, out)
	}
	detail, _ := out["detail"].(string)
	if !strings.Contains(detail, "step 2") || strings.Contains(detail, "abcdefgh1234") {
		t.Fatalf("detail should name step 2 without echoing the value: %q", detail)
	}
}

func TestProcedureRemoteCallerIsRefused(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	w := postFrom(t, h, anchorRemote, "POST", "/api/memory/procedure", procBody(t))
	if w.Code != http.StatusForbidden {
		t.Fatalf("remote peer = %d %s", w.Code, w.Body)
	}
	b := procBody(t)
	b["caller_remote"] = true
	w = postFrom(t, h, anchorLoopback, "POST", "/api/memory/procedure", b)
	if w.Code != http.StatusForbidden {
		t.Fatalf("caller_remote = %d %s", w.Code, w.Body)
	}
}

func TestProcedureSlotPostIsNotRunAndNeedsValues(t *testing.T) {
	t.Parallel()
	_, post := procStore(t)
	b := procBody(t)
	b["post"] = []map[string]any{{"type": "binary_version", "bin": "grimoire", "text": "<version>"}}
	code, out := post(b)
	if code != http.StatusCreated {
		t.Fatalf("%d %v", code, out)
	}
	checks, _ := out["checks"].([]any)
	if len(checks) != 2 || !strings.Contains(strings.ToLower(jsonString(checks[1])), "needs values") {
		t.Fatalf("slot post not reported as needing values: %v", checks)
	}
	if nv, _ := out["needs_values"].([]any); len(nv) != 1 {
		t.Fatalf("needs_values %v", out["needs_values"])
	}
}

func TestProcedureRewriteReplacesOnlyItsOwnNote(t *testing.T) {
	t.Parallel()
	dir, post := procStore(t)
	if code, out := post(procBody(t)); code != http.StatusCreated {
		t.Fatalf("first %d %v", code, out)
	}
	code, out := post(procBody(t))
	if code != http.StatusCreated || out["replaced"] != true {
		t.Fatalf("rewrite %d %v", code, out)
	}
	if notes, _ := memstore.LoadDir(dir); len(notes) != 1 {
		t.Fatalf("rewrite left %d notes", len(notes))
	}

	// A hand-written skill with the same name is never overwritten.
	hand := "---\nname: Rotate the kestrel key\ndescription: by hand\n---\n\n1. do it\n2. again\n"
	if err := os.WriteFile(filepath.Join(dir, "hand.md"), []byte(hand), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := post(procBody(t)); code != http.StatusConflict {
		t.Fatalf("hand-written clash = %d %v", code, out)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "hand.md")); string(raw) != hand {
		t.Fatal("a hand-written note was changed")
	}
}

func TestProcedureSkippedPreIsStored(t *testing.T) {
	t.Parallel()
	dir, post := procStore(t)
	b := procBody(t)
	b["host"] = "elsewhere"
	code, out := post(b)
	if code != http.StatusCreated {
		t.Fatalf("a pre that cannot be checked from here must be stored: %d %v", code, out)
	}
	if notes, _ := memstore.LoadDir(dir); len(notes) != 1 {
		t.Fatal("not stored")
	}
}

func TestProcedureNeedsAStore(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	w := postFrom(t, h, anchorLoopback, "POST", "/api/memory/procedure", procBody(t))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store = %d %s", w.Code, w.Body)
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The anchors report counts procedures and runs their pre checks now.
func TestAnchorsReportCountsProceduresAndTheirPreChecks(t *testing.T) {
	t.Parallel()
	srv, h := testServer(t)
	dir := t.TempDir()
	if err := srv.Settings.Update(map[string]string{"memory_canonical_dir": dir}); err != nil {
		t.Fatal(err)
	}
	body := procBody(t)
	pre := body["pre"].([]map[string]any)[0]["path"].(string)
	if w := postFrom(t, h, anchorLoopback, "POST", "/api/memory/procedure", body); w.Code != http.StatusCreated {
		t.Fatalf("write %d %s", w.Code, w.Body)
	}
	// A procedure with no spec is counted, but its checks are not run.
	plain := "---\nname: Plain steps\ndescription: no spec\nmetadata:\n  kind: procedure\n---\n\n1. a\n2. b\n"
	if err := os.WriteFile(filepath.Join(dir, "plain.md"), []byte(plain), 0o644); err != nil {
		t.Fatal(err)
	}
	tally := func() map[string]any {
		w := do(t, h, "GET", "/api/memory/anchors", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("anchors %d %s", w.Code, w.Body)
		}
		var out map[string]any
		decode(t, w, &out)
		p, _ := out["procedures"].(map[string]any)
		return p
	}
	if got := tally(); got["total"] != float64(2) || got["with_spec"] != float64(1) || got["pre_pass"] != float64(1) || got["pre_fail"] != float64(0) {
		t.Fatalf("tally while the pre check holds: %v", got)
	}
	if err := os.Remove(pre); err != nil {
		t.Fatal(err)
	}
	if got := tally(); got["pre_pass"] != float64(0) || got["pre_fail"] != float64(1) {
		t.Fatalf("tally once the pre check breaks: %v", got)
	}
}

// A procedure is a write like remember, so the access class is the same: any
// signed-in caller, from the loopback peer, without the admin token. The
// loopback and caller_remote checks and the secret scan still apply.
func TestProcedureIsWritableByASignedInMemberOnLoopback(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "alice", "admin")
	memberKey := makeUser(t, s, h, adminKey, "bob", "member")
	dir := t.TempDir()
	if err := s.Settings.Update(map[string]string{"memory_canonical_dir": dir}); err != nil {
		t.Fatal(err)
	}
	send := func(peer, key string, body map[string]any) int {
		t.Helper()
		req := requestFor(t, "POST", "/api/memory/procedure", body)
		req.RemoteAddr = peer
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if code := send(anchorLoopback, memberKey, procBody(t)); code != http.StatusCreated {
		t.Fatalf("member on loopback = %d, want 201", code)
	}
	if code := send(anchorRemote, memberKey, procBody(t)); code != http.StatusForbidden {
		t.Fatalf("member from a remote peer = %d, want 403", code)
	}
	if code := send(anchorLoopback, "", procBody(t)); code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401", code)
	}
	b := procBody(t)
	b["steps"] = []string{"make rotate", "curl -H 'X-Grimoire-Admin: abcdefgh1234' http://x"}
	if code := send(anchorLoopback, memberKey, b); code != http.StatusBadRequest {
		t.Fatalf("a credential in a step = %d, want 400", code)
	}
}

func TestProcedureSlotIsNotACredential(t *testing.T) {
	if looksLikeCredential("curl -H 'X-Grimoire-Admin: <admin-token>' 127.0.0.1:9111/api/x") {
		t.Fatal("a slot was taken for a credential")
	}
	if !looksLikeCredential("curl -H 'X-Grimoire-Admin: " + "abcdef" + "0123456789abcdef' 127.0.0.1:9111/api/x") {
		t.Fatal("a literal header value was not caught")
	}
}
