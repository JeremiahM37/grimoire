package connectors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"no error", nil, StateOK},
		{"401 from token endpoint", &TokenError{Status: 401}, StateNeedsReauth},
		{"403 from token endpoint", &TokenError{Status: 403}, StateNeedsReauth},
		{"invalid_grant", &TokenError{Status: 400, Code: "invalid_grant"}, StateNeedsReauth},
		{"no refresh token", &TokenError{Code: "no_refresh_token",
			Description: "the google token has expired and has no refresh token"}, StateNeedsReauth},
		{"500 from token endpoint", &TokenError{Status: 500, Code: "server_error"}, StateFailing},
		{"wrapped invalid_grant", fmt.Errorf("refreshing the google token: %w",
			&TokenError{Status: 400, Code: "invalid_grant"}), StateNeedsReauth},
		{"invalid_grant as text", errors.New("refreshing the google token: invalid_grant: " +
			"Token has been expired or revoked."), StateNeedsReauth},
		{"network", errors.New("GET www.googleapis.com: connection reset"), StateFailing},
		{"vault locked", errors.New(`credential "google-oauth": locked`), StateFailing},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("%s: Classify = %s, want %s", c.name, got, c.want)
		}
	}
}

// A dead grant from a real token endpoint, through Refresh, is still
// recognised after the error has been wrapped on the way up.
func TestRefreshDeadGrantClassifiesNeedsReauth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: rewriteTo{target}}

	_, err := Refresh(context.Background(), client, OAuthToken{Provider: "google",
		AccessToken: "ya29.old", RefreshToken: "1//refresh", ClientID: "cid"})
	if err == nil {
		t.Fatal("expected a refresh error")
	}
	if got := Classify(err); got != StateNeedsReauth {
		t.Fatalf("Classify = %s, want needs_reauth (err: %v)", got, err)
	}
}

func TestRefreshServerErrorIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `<html>oops</html>`)
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: rewriteTo{target}}

	_, err := Refresh(context.Background(), client, OAuthToken{Provider: "google",
		AccessToken: "ya29.old", RefreshToken: "1//refresh", ClientID: "cid"})
	if err == nil {
		t.Fatal("expected a refresh error")
	}
	if got := Classify(err); got != StateFailing {
		t.Fatalf("Classify = %s, want failing (err: %v)", got, err)
	}
}

// rewriteTo sends every request to target, so a test can stand in for a
// provider's token endpoint without changing the URL the code builds.
type rewriteTo struct{ target *url.URL }

func (r rewriteTo) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	u.Scheme, u.Host = r.target.Scheme, r.target.Host
	req2 := req.Clone(req.Context())
	req2.URL = &u
	req2.Host = u.Host
	return http.DefaultTransport.RoundTrip(req2)
}

func TestSafeErrorRedactsAndShortens(t *testing.T) {
	secret := "ya29.a0AfB_byXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
	msg := "GET https://x.test/?access_token=" + secret + "&alt=json failed\n  with Bearer " +
		"0123456789abcdefghijklmnopqrstuvwxyzABCD"
	got := SafeError(msg)
	if strings.Contains(got, secret) || strings.Contains(got, "0123456789abcdef") {
		t.Fatalf("credential survived: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("error is not one line: %q", got)
	}
	long := SafeError(strings.Repeat("é", 1000))
	if n := len([]rune(long)); n > maxErrorLen+3 {
		t.Fatalf("error not shortened: %d runes", n)
	}
}

func TestNextHealthTransientNeedsTwoFailures(t *testing.T) {
	t0 := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	h := nextHealth(Health{}, errors.New("timeout"), t0)
	if h.State != StateOK || h.Failures != 1 || h.Since != "" {
		t.Fatalf("one blip should not be failing: %+v", h)
	}
	h = nextHealth(h, errors.New("timeout"), t0.Add(time.Hour))
	if h.State != StateFailing || h.Failures != 2 {
		t.Fatalf("second failure should be failing: %+v", h)
	}
	if h.Since != t0.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("since = %s, want the second failure", h.Since)
	}
	h = nextHealth(h, nil, t0.Add(2*time.Hour))
	if h.State != StateOK || h.Failures != 0 || h.Since != "" || h.LastSuccess == "" {
		t.Fatalf("success should clear the streak: %+v", h)
	}
}

func TestNextHealthNeedsReauthKeepsOriginalSince(t *testing.T) {
	t0 := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	dead := &TokenError{Status: 400, Code: "invalid_grant", Description: "expired or revoked"}
	h := nextHealth(Health{}, dead, t0)
	if h.State != StateNeedsReauth {
		t.Fatalf("state = %s, want needs_reauth on the first dead grant", h.State)
	}
	h = nextHealth(h, dead, t0.Add(24*time.Hour))
	if h.Since != t0.Format(time.RFC3339) {
		t.Fatalf("since moved to %s; it should stay at the first failure", h.Since)
	}
}

func TestBackoffForDoublesAndCaps(t *testing.T) {
	hour := time.Hour
	cases := []struct {
		interval time.Duration
		failures int
		want     time.Duration
	}{
		{hour, 2, 2 * hour},
		{hour, 3, 4 * hour},
		{hour, 4, backoffCap},
		{hour, 20, backoffCap},
		{7 * hour, 2, 7 * hour}, // never less than the schedule itself
	}
	for _, c := range cases {
		if got := backoffFor(c.interval, c.failures); got != c.want {
			t.Errorf("backoffFor(%v, %d) = %v, want %v", c.interval, c.failures, got, c.want)
		}
	}
}

func TestDueNowSkipsParkedConnector(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	c := Connector{ID: "g", Enabled: true, Interval: 3600,
		LastRun: now.Add(-2 * time.Hour).Format(time.RFC3339)}
	recent := Health{State: StateNeedsReauth, LastFailure: now.Add(-time.Hour).Format(time.RFC3339)}
	if dueNow(c, recent, now) {
		t.Error("a parked connector was due within 24h of its last attempt")
	}
	old := Health{State: StateNeedsReauth, LastFailure: now.Add(-25 * time.Hour).Format(time.RFC3339)}
	if !dueNow(c, old, now) {
		t.Error("a parked connector should get one attempt after 24h")
	}
	disabled := c
	disabled.Enabled = false
	if dueNow(disabled, old, now) {
		t.Error("a disabled connector must never be retried")
	}
}

func TestDueNowBacksOffFailingConnector(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	failing := Health{State: StateFailing, Failures: 2}
	c := Connector{ID: "f", Enabled: true, Interval: 3600,
		LastRun: now.Add(-90 * time.Minute).Format(time.RFC3339)}
	if dueNow(c, failing, now) {
		t.Error("two failures should wait out a two-hour backoff")
	}
	c.LastRun = now.Add(-3 * time.Hour).Format(time.RFC3339)
	if !dueNow(c, failing, now) {
		t.Error("past the backoff the failing connector should run")
	}
	ok := Health{State: StateOK}
	c.LastRun = now.Add(-90 * time.Minute).Format(time.RFC3339)
	if !dueNow(c, ok, now) {
		t.Error("a healthy connector follows its normal schedule")
	}
}

func TestHealthStoreRoundTripAndRunnerSkips(t *testing.T) {
	store := NewStore(testDB(t))
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	c := Connector{ID: "g", Kind: "gmail", Name: "Gmail", Secret: "google-oauth",
		Prefix: "connectors/gmail", Interval: 3600, Enabled: true, Created: "2026-08-19T00:00:00Z",
		LastRun: now.Add(-2 * time.Hour).Format(time.RFC3339)}
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRun("g", &TokenError{Status: 400, Code: "invalid_grant"}, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := store.Health("g")
	if err != nil || got.State != StateNeedsReauth {
		t.Fatalf("health = %+v, %v; want needs_reauth", got, err)
	}

	r := &Runner{Store: store}
	due, err := r.Due(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("parked connector was due: %v", due)
	}

	if err := store.ResetHealth("g"); err != nil {
		t.Fatal(err)
	}
	due, _ = r.Due(now)
	if len(due) != 1 {
		t.Fatalf("reconnected connector should be due on its schedule, got %d", len(due))
	}

	all, err := store.AllHealth()
	if err != nil || len(all) != 0 {
		t.Fatalf("AllHealth after reset = %v, %v", all, err)
	}
	if err := store.RecordRun("g", errors.New("boom"), now); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("g"); err != nil {
		t.Fatal(err)
	}
	if all, _ := store.AllHealth(); len(all) != 0 {
		t.Fatalf("health survived the connector's deletion: %v", all)
	}
}
