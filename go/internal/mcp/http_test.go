package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
