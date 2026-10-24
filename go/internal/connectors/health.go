package connectors

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// Whether a connector is actually working.
//
// A connector's last run is a single bit, and the console showed it only when
// someone opened the page. A Google sign-in that expired on 2026-10-17 kept
// failing every few minutes for a week, and the only trace was a log line,
// while the server reported itself healthy. So each connector keeps a small
// health record: when it last worked, when it last failed, the last error in a
// form that is safe to show, and how many runs in a row have failed.
//
// Two kinds of failure are told apart. A dead grant (the owner must sign in
// again) will not heal by itself, so retrying it on the schedule only makes
// noise: it is parked until the connector is reconnected, with one attempt a
// day to notice if it came back. Anything else is treated as transient: after
// two failures in a row the connector is "failing" and retried with
// exponential backoff, capped at six hours.

// Connector states.
const (
	StateOK          = "ok"
	StateFailing     = "failing"
	StateNeedsReauth = "needs_reauth"
)

const (
	// failThreshold is how many consecutive failures make a transient error
	// count as failing rather than a blip.
	failThreshold = 2
	// reauthRetry is how often a parked connector is tried anyway, in case the
	// grant came back by some other route.
	reauthRetry = 24 * time.Hour
	// backoffCap bounds the wait between attempts for a failing connector.
	backoffCap = 6 * time.Hour
)

// Health is one connector's record. Empty fields mean "never happened".
type Health struct {
	Connector   string `json:"connector"`
	LastSuccess string `json:"last_success,omitempty"`
	LastFailure string `json:"last_failure,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	Failures    int    `json:"consecutive_failures"`
	State       string `json:"state"`
	// Since is when the connector entered its current bad state (RFC3339).
	Since string `json:"since,omitempty"`
}

// Classify names the kind of failure: StateNeedsReauth for a dead grant, and
// StateFailing for anything else. Whether a transient error has crossed the
// threshold is the caller's business, because it depends on the count.
func Classify(err error) string {
	if err == nil {
		return StateOK
	}
	var te *TokenError
	if errors.As(err, &te) && te.NeedsReauth() {
		return StateNeedsReauth
	}
	// Errors that reached us as text, e.g. wrapped by a source that did not
	// keep the type.
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "invalid_grant") || strings.Contains(msg, "expired or revoked") ||
		strings.Contains(msg, "has no refresh token") {
		return StateNeedsReauth
	}
	return StateFailing
}

var (
	// Long opaque runs are how tokens look. Redacting them is a backstop: the
	// connectors should never put a credential in an error in the first place.
	opaqueRE = regexp.MustCompile(`[A-Za-z0-9_\-./+=]{32,}`)
	// key=value pairs that carry credentials in URLs and form bodies.
	credPairRE = regexp.MustCompile(`(?i)(access_token|refresh_token|client_secret|id_token|token|password|key|secret)=[^\s&"']+`)
)

// maxErrorLen keeps a stored error short enough to read in a list.
const maxErrorLen = 200

// SafeError returns an error message fit to store and show: one line, no
// obvious credentials, and short.
func SafeError(msg string) string {
	msg = strings.Join(strings.Fields(msg), " ")
	msg = credPairRE.ReplaceAllString(msg, "$1=[redacted]")
	msg = opaqueRE.ReplaceAllString(msg, "[redacted]")
	if r := []rune(msg); len(r) > maxErrorLen {
		msg = string(r[:maxErrorLen]) + "..."
	}
	return msg
}

// nextHealth applies one run's outcome to a connector's record.
func nextHealth(h Health, err error, now time.Time) Health {
	stamp := now.UTC().Format(time.RFC3339)
	if err == nil {
		return Health{Connector: h.Connector, LastSuccess: stamp, LastFailure: h.LastFailure, State: StateOK}
	}
	h.LastFailure = stamp
	h.LastError = SafeError(err.Error())
	h.Failures++
	prev := h.State
	class := Classify(err)
	switch {
	case class == StateNeedsReauth:
		h.State = StateNeedsReauth
	case h.Failures >= failThreshold:
		h.State = StateFailing
	default:
		h.State = StateOK
	}
	if h.State == StateOK {
		h.Since = ""
	} else if h.Since == "" || h.State != prev {
		h.Since = stamp
	}
	return h
}

// backoffFor is how long a failing connector waits after its n-th consecutive
// failure: the schedule interval doubled per failure, capped, and never less
// than the interval itself.
func backoffFor(interval time.Duration, failures int) time.Duration {
	d := interval
	for i := 1; i < failures && d < backoffCap; i++ {
		d *= 2
	}
	if d > backoffCap {
		d = backoffCap
	}
	if d < interval {
		d = interval
	}
	return d
}

// dueNow decides a scheduled run given the connector's health.
//
// A parked connector is skipped entirely, except for one attempt every
// reauthRetry. A failing connector waits out its backoff after its last
// attempt. Everything else follows the normal schedule.
func dueNow(c Connector, h Health, now time.Time) bool {
	if h.State == StateNeedsReauth {
		if !c.Enabled || c.Interval <= 0 {
			return false
		}
		last, ok := parseStamp(h.LastFailure)
		return !ok || now.Sub(last) >= reauthRetry
	}
	if !c.Due(now) {
		return false
	}
	if h.State == StateFailing {
		last, ok := parseStamp(c.LastRun)
		if ok && now.Sub(last) < backoffFor(time.Duration(c.Interval)*time.Second, h.Failures) {
			return false
		}
	}
	return true
}

func parseStamp(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}
