package api

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bounds on what one caller can make the server do.
//
// Two things were missing, and the second got worse when this server grew
// expensive endpoints:
//
//   - No body limit. A request could stream gigabytes into memory before any
//     handler looked at it; the JSON decoder would happily try.
//   - No rate limit. `ask` fans out to an LLM, `web/fetch` dials out on the
//     caller's word, and `connectors/{id}/run` talks to someone else's API.
//     A loop over any of them is a denial of service against this instance and
//     a way to get an IP banned by whoever is on the other end.
//
// The limiter is deliberately simple — a token bucket per address, in memory,
// no dependencies. A self-hosted server does not need a distributed rate
// limiter; it needs to not fall over.

// maxBodyBytes is the ceiling for a request body. Attachments and audio memos
// are the large ones; they are multipart and get their own, larger, allowance.
const (
	maxBodyBytes   = 8 << 20
	maxUploadBytes = 128 << 20
)

// limitBodies caps request bodies before any handler reads them.
func limitBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
			limit := int64(maxBodyBytes)
			if isUpload(r) {
				limit = maxUploadBytes
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

func isUpload(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		return true
	}
	// Attachments and audio are posted as raw bytes on these routes.
	return strings.HasPrefix(r.URL.Path, "/api/attach") ||
		strings.HasPrefix(r.URL.Path, "/api/audio") ||
		strings.HasPrefix(r.URL.Path, "/api/import")
}

// costly routes get their own, much tighter, bucket: each one spends someone
// else's resources — an LLM, a website, a third-party API — rather than only
// this server's.
var costlyPrefixes = []string{
	"/api/ask", "/api/web/", "/api/connectors/", "/api/actions", "/api/audio",
}

func costly(path string) bool {
	for _, p := range costlyPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// bucket is a token bucket refilled continuously.
type bucket struct {
	tokens float64
	last   time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64
}

func newRateLimiter(rate, burst float64) *rateLimiter {
	return &rateLimiter{buckets: map[string]*bucket{}, rate: rate, burst: burst}
}

// allow reports whether the caller may proceed, and how long to wait if not.
func (l *rateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
		// Bound the map so a spray of addresses cannot grow it forever.
		if len(l.buckets) > 8192 {
			for k, v := range l.buckets {
				if now.Sub(v.last) > 10*time.Minute {
					delete(l.buckets, k)
				}
			}
		}
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1-b.tokens)/l.rate*float64(time.Second)) + time.Second
}

// Limits, in requests per second and burst. Configurable because a number
// baked into a binary is a number that is wrong for somebody: an agent doing a
// bulk import and a phone on a train are not the same client.
//
// The general limit is deliberately far above any legitimate use. Its job is to
// stop a runaway loop, not to shape traffic — and the first version of it
// refused 143 of 400 plain health checks and made the browser suite fail, which
// is exactly the failure mode a rate limiter is supposed to prevent rather than
// cause. GRIMOIRE_RATE_LIMIT=off disables both.
func rateFromEnv(key string, rate, burst float64) (float64, float64) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("GRIMOIRE_RATE_LIMIT")), "off") {
		return 0, 0
	}
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			return n, n * 20
		}
	}
	return rate, burst
}

// throttle applies the two buckets.
func (s *Server) throttle(next http.Handler) http.Handler {
	generalRate, generalBurst := rateFromEnv("GRIMOIRE_RATE_GENERAL", 500, 5000)
	costlyRate, costlyBurst := rateFromEnv("GRIMOIRE_RATE_EXPENSIVE", 2, 30)
	if generalRate == 0 {
		return next
	}
	general := newRateLimiter(generalRate, generalBurst)
	expensive := newRateLimiter(costlyRate, costlyBurst)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r) // static console assets
			return
		}
		now := time.Now()
		key := clientAddr(r)
		limiter, kind := general, "requests"
		if costly(r.URL.Path) {
			limiter, kind = expensive, "expensive requests"
		}
		if ok, wait := limiter.allow(key, now); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
			writeErr(w, http.StatusTooManyRequests,
				"too many "+kind+" — slow down for "+wait.Round(time.Second).String())
			return
		}
		next.ServeHTTP(w, r)
	})
}
