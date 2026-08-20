package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A request body must be capped before a handler reads it: the JSON decoder
// will otherwise stream whatever it is given into memory.
func TestEnormousBodiesAreRefused(t *testing.T) {
	_, h := testServer(t)
	huge := bytes.NewReader([]byte(`{"path":"big.md","body":"` +
		strings.Repeat("x", 12<<20) + `"}`))
	req := httptest.NewRequest("POST", "/api/notes", huge)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusCreated {
		t.Fatalf("a 12 MB note body was accepted")
	}
}

// The expensive routes spend someone else's resources — an LLM, a website, a
// third-party API — so a loop over them must be stopped.
func TestExpensiveRoutesAreRateLimited(t *testing.T) {
	_, h := testServer(t)
	limited := 0
	for i := 0; i < 60; i++ {
		req := httptest.NewRequest("POST", "/api/ask", strings.NewReader(`{"q":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.9:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			limited++
			if w.Header().Get("Retry-After") == "" {
				t.Fatal("a throttled response does not say when to retry")
			}
		}
	}
	if limited == 0 {
		t.Fatal("60 asks in a row were all allowed")
	}
}

// Ordinary reads must not be throttled at a level the console would hit: it
// makes a burst of requests every time a note is opened.
func TestOrdinaryReadsAreNotThrottledAtConsoleSpeed(t *testing.T) {
	_, h := testServer(t)
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest("GET", "/api/health", nil)
		req.RemoteAddr = "203.0.113.10:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("throttled an ordinary read after %d requests", i)
		}
	}
}

// One caller's limit must not be another's: rate limiting by a header anyone
// can set, or not by caller at all, would make this either useless or a way to
// lock other people out.
func TestRateLimitIsPerCaller(t *testing.T) {
	_, h := testServer(t)
	spend := func(addr string) int {
		throttled := 0
		for i := 0; i < 40; i++ {
			req := httptest.NewRequest("POST", "/api/ask", strings.NewReader(`{"q":"x"}`))
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = addr
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code == http.StatusTooManyRequests {
				throttled++
			}
		}
		return throttled
	}
	if spend("198.51.100.1:1") == 0 {
		t.Fatal("the first caller was never throttled")
	}
	if got := spend("198.51.100.2:1"); got > 30 {
		t.Fatalf("a second caller was throttled %d/40 times by the first one's usage", got)
	}
}

// X-Forwarded-For is caller-supplied. Honouring it unconditionally lets anyone
// mint a fresh identity per request and walk straight through every limit.
func TestForwardedHeadersAreIgnoredUnlessAProxyIsTrusted(t *testing.T) {
	_, h := testServer(t)
	throttled := 0
	for i := 0; i < 60; i++ {
		req := httptest.NewRequest("POST", "/api/ask", strings.NewReader(`{"q":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.50:1"
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d", i)) // a new "caller" each time
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			throttled++
		}
	}
	if throttled == 0 {
		t.Fatal("a forged X-Forwarded-For walked through the rate limit")
	}
}

// Login is the one credential check an unauthenticated caller can drive, so it
// has to back off — the same reasoning the secret vault has always applied.
func TestLoginBacksOffAfterRepeatedFailures(t *testing.T) {
	s, h := testServer(t)
	makeUser(t, s, h, "", "alice", "admin")

	locked := false
	for i := 0; i < 12; i++ {
		req := httptest.NewRequest("POST", "/api/auth/login",
			strings.NewReader(`{"name":"alice","password":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.77:1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			locked = true
			break
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d %s", i, w.Code, w.Body)
		}
	}
	if !locked {
		t.Fatal("twelve wrong passwords in a row were all answered at full speed")
	}
	// And the correct password is refused too while locked out — otherwise the
	// lockout is only a speed bump for the attacker who guesses right.
	req := httptest.NewRequest("POST", "/api/auth/login",
		strings.NewReader(`{"name":"alice","password":"correct horse battery"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.77:1"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatal("the lockout does not apply to a correct password")
	}
	_ = time.Now
}
