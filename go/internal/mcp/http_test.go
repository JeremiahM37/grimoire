package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The HTTP transport must answer exactly what the stdio loop answers — it is a
// second door onto one implementation, and a divergence between the two is the
// failure this guards.
func TestHTTPTransportMatchesStdio(t *testing.T) {
	s := New("http://127.0.0.1:1", "agent")
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	var stdioOut bytes.Buffer
	if err := s.Serve(bytes.NewReader(body), &stdioOut); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
	s.HTTPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("http transport returned %d", rec.Code)
	}

	var viaStdio, viaHTTP any
	if err := json.Unmarshal(stdioOut.Bytes(), &viaStdio); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &viaHTTP); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(viaStdio)
	b, _ := json.Marshal(viaHTTP)
	if string(a) != string(b) {
		t.Errorf("transports disagree:\n stdio: %s\n  http: %s", a, b)
	}
}

func TestHTTPTransportRejectsBadFrames(t *testing.T) {
	s := New("http://127.0.0.1:1", "agent")
	h := s.HTTPHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/mcp", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp = %d, want 405", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/mcp", bytes.NewReader([]byte("{not json"))))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed frame = %d, want 400", rec.Code)
	}

	// A notification carries no id and takes no reply.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/mcp",
		bytes.NewReader([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))))
	if rec.Code != http.StatusAccepted {
		t.Errorf("notification = %d, want 202", rec.Code)
	}
}

// ---- inbound auth on the HTTP transport ------------------------------------
//
// This transport is how a hosted agent reaches Grimoire, which means exposing
// it — and what is exposed is not a read-only search box. The same mount
// carries remember, create_note and the credential broker.

func rpcPost(h http.Handler, token string, viaQuery bool) *httptest.ResponseRecorder {
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	url := "/mcp"
	if viaQuery && token != "" {
		url += "?token=" + token
	}
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if token != "" && !viaQuery {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHTTPTransportIsOpenWhenNoTokenIsSet(t *testing.T) {
	// The loopback default. A local agent is already inside the trust boundary,
	// and demanding a secret there only gets one written into a dotfile.
	s := New("http://127.0.0.1:9111", "test")
	if got := rpcPost(s.HTTPHandler(), "", false).Code; got != http.StatusOK {
		t.Fatalf("status %d with no token configured, want 200", got)
	}
}

func TestHTTPTransportRefusesAWrongOrMissingToken(t *testing.T) {
	s := New("http://127.0.0.1:9111", "test")
	s.InboundToken = "correct-horse"
	h := s.HTTPHandler()

	for _, tc := range []struct{ name, token string }{
		{"no token", ""},
		{"wrong token", "wrong"},
		{"prefix of the real token", "correct"},
	} {
		rec := rpcPost(h, tc.token, false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", tc.name, rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no bearer challenge, so a client has nothing to act on", tc.name)
		}
	}
	if got := rpcPost(h, "correct-horse", false).Code; got != http.StatusOK {
		t.Fatalf("the correct token was rejected: %d", got)
	}
}

// Some hosted clients cannot set headers on the URL they are given.
func TestHTTPTransportAcceptsATokenInTheQuery(t *testing.T) {
	s := New("http://127.0.0.1:9111", "test")
	s.InboundToken = "tok"
	if got := rpcPost(s.HTTPHandler(), "tok", true).Code; got != http.StatusOK {
		t.Fatalf("query-parameter token rejected: %d", got)
	}
}

// The configuration that would publish the vault must be impossible, not merely
// discouraged in a paragraph nobody reads.
func TestServingOffLoopbackWithoutATokenIsRefused(t *testing.T) {
	s := New("http://127.0.0.1:9111", "test")
	err := s.ListenAndServe("0.0.0.0:9112")
	if err == nil {
		t.Fatal("bound a public address with no inbound token")
	}
	for _, want := range []string{"GRIMOIRE_MCP_TOKEN", "credential broker"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not explain the risk (%q missing): %v", want, err)
		}
	}
}

func TestLoopbackDetection(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9112", true},
		{"localhost:9112", true},
		{"[::1]:9112", true},
		{"0.0.0.0:9112", false},
		{":9112", false}, // every interface — the easiest way to expose it by accident
		{"192.168.1.10:9112", false},
		{"100.96.103.31:9112", false}, // a tailnet address is still not loopback
	} {
		if got := loopback(tc.addr); got != tc.want {
			t.Errorf("loopback(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}
