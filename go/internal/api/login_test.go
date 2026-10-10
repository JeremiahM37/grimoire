package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBrowserTokenLoginDoesNotExposeToken(t *testing.T) {
	t.Parallel()
	s, _ := testServer(t)
	s.AuthToken = "private-token"
	h := s.Routes()
	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest("GET", "/", nil))
	if page.Code != 401 || !strings.Contains(page.Body.String(), "Access token") || strings.Contains(page.Body.String(), s.AuthToken) {
		t.Fatal("missing safe login page", page.Code)
	}
	for _, token := range []string{"wrong", s.AuthToken} {
		req := httptest.NewRequest("POST", "/auth/token", strings.NewReader(url.Values{"token": {token}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), token) {
			t.Fatal("login reflected token")
		}
		if token == "wrong" {
			if rec.Code != 401 || len(rec.Result().Cookies()) != 0 {
				t.Fatal("invalid login succeeded")
			}
			continue
		}
		if rec.Code != 303 || rec.Header().Get("Location") != "/" {
			t.Fatal("login did not redirect", rec.Code)
		}
		cookies := rec.Result().Cookies()
		if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
			t.Fatal("unsafe auth cookie")
		}
		next := httptest.NewRequest("GET", "/api/notes", nil)
		next.AddCookie(cookies[0])
		ok := httptest.NewRecorder()
		h.ServeHTTP(ok, next)
		if ok.Code != 200 {
			t.Fatal("cookie did not authenticate API", ok.Code)
		}
	}
}

func TestBrowserLoginRejectsLargeFormAndQueryOnlyToken(t *testing.T) {
	t.Parallel()
	s, _ := testServer(t)
	s.AuthToken = "secret"
	h := s.Routes()
	for _, body := range []string{"", "token=" + strings.Repeat("x", 5000)} {
		req := httptest.NewRequest("POST", "/auth/token?token=secret", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 401 || len(rec.Result().Cookies()) != 0 {
			t.Fatal("invalid form authenticated", rec.Code)
		}
	}
}
