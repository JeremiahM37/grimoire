package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKnowledgeWorkSharesExpensiveRequestBudget(t *testing.T) {
	_, handler := testServer(t)
	paths := []string{"/api/knowledge/query", "/api/knowledge/extract", "/api/documents/import", "/api/documents/refresh"}
	limited := false
	for attempt := 0; attempt < 60; attempt++ {
		request := httptest.NewRequest(http.MethodPost, paths[attempt%len(paths)], strings.NewReader(`{}`))
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = "203.0.113.71:1234"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code == http.StatusTooManyRequests {
			limited = true
			if response.Header().Get("Retry-After") == "" {
				t.Fatal("resource limit did not report retry timing")
			}
		}
	}
	if !limited {
		t.Fatal("alternating document extraction and knowledge queries bypassed resource limits")
	}
	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	request.RemoteAddr = "203.0.113.71:1234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("exhausting expensive work blocked ordinary health reads: %d", response.Code)
	}
}
