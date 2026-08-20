package websearch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Fetch runs its URLs concurrently. It must not touch the shared client while
// doing so: an earlier version set client.Timeout in place, which raced with
// every other goroutine in the same batch and would have quietly changed the
// timeout for every other caller of that client. Run with -race.
func TestFetchDoesNotMutateTheSharedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><p>ok</p></body></html>")
	}))
	defer srv.Close()
	// A client with no timeout is the case the code "fixes" in place.
	c := &Client{Settings: settings{}, HTTP: &http.Client{Transport: srv.Client().Transport}}
	urls := make([]string, 8)
	for i := range urls {
		urls[i] = srv.URL
	}
	c.Fetch(context.Background(), urls, 0)
}
