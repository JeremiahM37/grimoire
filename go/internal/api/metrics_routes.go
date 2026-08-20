package api

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/metrics"
)

// What gets measured, and what deliberately does not.
//
// Every metric here is something the server already computes while doing its
// job: how long a request took, whether the retrieval cache was patched or
// rebuilt, what a connector run wrote, whether a login failed. Nothing polls,
// nothing samples, and no metric costs a database query — a metrics endpoint
// that adds load is a metrics endpoint that lies about the load.
//
// Paths are reduced to a ROUTE CLASS, never the raw path. A note path is user
// content: emitting one as a label would put note titles into a monitoring
// system, keep them there for the retention period, and show them to whoever
// can read a dashboard. It would also blow the cardinality up by one series per
// note, which is the other reason not to.

func (s *Server) metricsRoutes(mux *http.ServeMux) {
	if metricsDisabled() {
		return
	}
	mux.HandleFunc("GET /metrics", s.serveMetrics)
	s.registerGauges()
}

func metricsDisabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GRIMOIRE_METRICS")), "off")
}

func (s *Server) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder
	metrics.Default.Write(&b)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// registerGauges wires the values read at scrape time.
func (s *Server) registerGauges() {
	metrics.Gauge("grimoire_notes_current", "Notes in the index.", nil, func() float64 {
		// The cache already knows this; asking the database on every scrape
		// would make monitoring a source of load.
		chunks, notes, terms, bytes := s.Index.CacheStats()
		_, _, _ = chunks, terms, bytes
		return float64(notes)
	})
	metrics.Gauge("grimoire_chunks_current", "Chunks resident in the retrieval cache.", nil,
		func() float64 {
			chunks, _, _, _ := s.Index.CacheStats()
			return float64(chunks)
		})
	metrics.Gauge("grimoire_cache_vector_bytes", "Bytes of vectors held in memory.", nil,
		func() float64 {
			_, _, _, bytes := s.Index.CacheStats()
			return float64(bytes)
		})
	metrics.Gauge("grimoire_vault_unlocked_info",
		"1 when the credential vault is unlocked and the broker can serve.", nil,
		func() float64 {
			if s.Secrets != nil && s.Secrets.IsUnlocked() {
				return 1
			}
			return 0
		})
	metrics.Gauge("grimoire_connectors_current", "Configured connectors, by state.",
		map[string]string{"state": "failing"}, func() float64 {
			if s.Connectors == nil {
				return 0
			}
			list, err := s.Connectors.List()
			if err != nil {
				return 0
			}
			n := 0
			for _, c := range list {
				if !c.LastOK {
					n++
				}
			}
			return float64(n)
		})
}

// instrument records one request. Wraps the whole mux, so a route added later
// is measured without anyone remembering to.
func instrument(next http.Handler) http.Handler {
	if metricsDisabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		class := routeClass(r.URL.Path)
		labels := map[string]string{"route": class, "method": r.Method}
		metrics.Observe("grimoire_request_seconds", "Request duration by route class.",
			labels, time.Since(start).Seconds())
		metrics.Count("grimoire_requests_total", "Requests by route class and status.",
			map[string]string{"route": class, "method": r.Method,
				"status": statusClass(rec.status)})
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush and Unwrap keep streaming responses (the ask SSE surface) working
// through the wrapper.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// routeClass reduces a path to a bounded label. Note paths are user content and
// must never become label values.
func routeClass(path string) string {
	if !strings.HasPrefix(path, "/api/") {
		return "console"
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/"), "/")
	switch parts[0] {
	case "notes":
		if len(parts) == 1 {
			return "notes:list"
		}
		return "notes:one"
	case "web":
		if len(parts) > 1 {
			return "web:" + parts[1]
		}
		return "web"
	case "connectors":
		if len(parts) > 2 {
			return "connectors:run"
		}
		return "connectors"
	case "secrets", "grants", "vault", "audit":
		return "credentials"
	case "auth", "users", "spaces", "keys", "me":
		return "identity"
	case "search", "retrieve", "context", "ask", "memory", "briefing":
		return parts[0]
	}
	return parts[0]
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}
