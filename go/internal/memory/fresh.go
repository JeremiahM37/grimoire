package memory

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Freshness: when a remembered fact should be re-checked before it is used.
//
// A fact about the world goes out of date without anything in the store
// noticing. Re-checking every fact on every use (C's `volatile`: always read
// the real thing) is exact and expensive; never re-checking is free and wrong
// a few percent of the time. The model here aims the checks:
//
//   - A fact may declare a TIER. `stable` is never re-checked, `volatile` is
//     re-checked on every use, and a duration ("7d") is re-checked once that
//     long has passed since anyone last confirmed it. No tier means the
//     learned rate below decides.
//   - Every fact carries a CHANGE RATE estimated from its own history: each
//     time an agent superseded it is one observed change, and the time since
//     the fact's first version is the exposure. With a Gamma prior that is a
//     closed-form posterior mean, and P(stale) = 1 − exp(−λ·age since last
//     verified). The prior comes from the fact's class (what shape of value it
//     holds), so a version number starts out more suspect than a preference.
//   - A fact with an optional CHECK says how to verify it — the command or
//     place to look — so a re-check costs one tool call instead of an agent
//     working out where the truth lives.
//
// The design was tested before it was built (see docs/FRESHNESS.md): against
// real agent transcripts and repository history, learned rates served 2-3x
// fewer stale answers than a fixed re-check timer at the same lookup budget,
// and declared tiers were the best policy when lookups are scarce.

// Tier values. A duration string is the fourth, TTL, form.
const (
	TierStable   = "stable"
	TierVolatile = "volatile"
)

// Actions recall attaches to a fact.
const (
	ActionUse    = "use"
	ActionVerify = "verify"
)

// MaxCheckLen bounds a stored check. A check is something an agent will run,
// so it should be a lookup, and a lookup fits on one short line.
const MaxCheckLen = 300

// DefaultVerifyThreshold is the P(stale) above which an untiered fact is
// marked for re-checking. In the evaluation, thresholds between 0.2 and 0.35
// spent about a quarter to a half lookup per recalled fact; 0.3 sits there.
const DefaultVerifyThreshold = 0.3

// ParseFresh reads a tier: "stable", "volatile", or a TTL ("7d", "12h",
// "90m"). It reports false for anything else, and for a TTL under a minute.
func ParseFresh(s string) (tier string, ttl time.Duration, ok bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "auto":
		return "", 0, true
	case TierStable, TierVolatile:
		return s, 0, true
	}
	d, ok := parseTTL(s)
	if !ok || d < time.Minute {
		return "", 0, false
	}
	return s, d, true
}

func parseTTL(s string) (time.Duration, bool) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil || n <= 0 {
			return 0, false
		}
		return time.Duration(n * float64(24*time.Hour)), true
	}
	if strings.HasSuffix(s, "w") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "w"), 64)
		if err != nil || n <= 0 {
			return 0, false
		}
		return time.Duration(n * float64(7*24*time.Hour)), true
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// NormFresh is the stored form of a tier: lower case, with "auto" as empty.
func NormFresh(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "auto" {
		return ""
	}
	return s
}

// ValidCheck reports whether a check can be stored: one line, bounded, and
// free of the characters that would let it escape its trailer field.
func ValidCheck(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > MaxCheckLen || strings.ContainsAny(s, "\n\r\x00") {
		return false
	}
	return true
}

// Shape classes. A fact's class sets the prior on its change rate.
const (
	ShapePlain   = 0
	ShapeVolatil = 1
)

// volatileShapeRE matches the kinds of value that go out of date: an
// address, a port, a version (three parts, a "v", or named as one), usage
// ("43% full"), or wording that says the value is momentary ("currently",
// "as of"). Plain decimals and sizes are left out on purpose: in practice they
// are measurements and limits ("the gate is 0.01", "bounds are 128 MiB"),
// which do not drift. It is a prior, not a verdict — the rate a fact earns
// from its own history overrides it within a change or two.
var volatileShapeRE = regexp.MustCompile(`(?i)\b(?:\d{1,3}\.){3}\d{1,3}\b|:[0-9]{2,5}\b|\bport\s+\d{2,5}\b|\bv\d+(?:\.\d+)+\b|\b\d+\.\d+\.\d+\b|\b(?:version|release|build|pinned(?:\s+(?:at|to))?)\s+v?\d+(?:\.\d+)*|\d+(?:\.\d+)?\s?%\s+(?:full|used|free|utili[sz]ed)\b|\b(?:currently|right now|at the moment|latest|as of|is running|is deployed|is on)\b`)

// ShapeOf classifies a fact's text.
func ShapeOf(text string) int {
	if volatileShapeRE.MatchString(text) {
		return ShapeVolatil
	}
	return ShapePlain
}

// Default prior rates, in changes per day. A volatile-shaped value is assumed
// to change about every two months, anything else about every two years, until
// the store has seen enough of its own facts change to say otherwise.
var defaultRates = [2]float64{ShapePlain: 1.0 / 730, ShapeVolatil: 1.0 / 60}

// Priors are the per-class change rates recall assesses against.
type Priors [2]float64

// DefaultPriors returns the built-in rates.
func DefaultPriors() Priors { return Priors(defaultRates) }

// classPriorDays is how many fact-days of observation the built-in class rate
// is worth when the store learns its own: a year, so a handful of changes in a
// small store nudges the rate and a large store's history replaces it.
const classPriorDays = 365.0

// factPriorDays is how many days of observation a fact's class rate is worth
// against the fact's own history. Thirty: a fact seen to change four times in
// a month is believed, and one confirmed unchanged for a year earns a slower
// rate than its class.
const factPriorDays = 30.0

// LearnPriors pools a class's observed changes over its observed exposure,
// shrunk toward the default. With no history it returns the default; with a
// lot it returns what the store actually saw.
func LearnPriors(changes, exposureDays [2]float64) Priors {
	var p Priors
	for c := range p {
		r0 := defaultRates[c]
		p[c] = (classPriorDays*r0 + math.Max(changes[c], 0)) / (classPriorDays + math.Max(exposureDays[c], 0))
	}
	return p
}

// Assessment is what recall says about a fact's freshness.
type Assessment struct {
	Tier       string  `json:"tier"`                  // stable | volatile | <ttl> | auto
	PStale     float64 `json:"p_stale"`               // probability the value has changed since last verified
	Action     string  `json:"action"`                // use | verify
	Reason     string  `json:"reason,omitempty"`      // why the action
	Check      string  `json:"check,omitempty"`       // how to verify, when the writer said
	VerifiedAt string  `json:"verified_at,omitempty"` // last confirmation, or the write
	AgeDays    float64 `json:"age_days"`              // days since VerifiedAt
	Changes    int     `json:"changes,omitempty"`     // times this fact has been found changed
	Verifies   int     `json:"verifies,omitempty"`    // times it has been confirmed
}

// MinChangeGap is how long a version must have stood for its replacement to
// count as the world changing. A fact replaced within a day of being written
// is far more often a refinement of the claim ("exceeds the gate" → "exceeds
// the gate purely from reduction order") than a change in what it describes,
// and counting refinements as changes made a store's research findings look
// as volatile as its IP addresses.
const MinChangeGap = 24 * time.Hour

// CountsAsChange reports whether replacing a version written at oldStamp
// with one written at newStamp is evidence the value changed.
func CountsAsChange(oldStamp, newStamp string) bool {
	o, ok1 := parseStamp(oldStamp)
	n, ok2 := parseStamp(newStamp)
	if !ok1 || !ok2 {
		return true
	}
	return n.Sub(o) >= MinChangeGap
}

// LastVerified is when the fact was last known true: an explicit confirmation,
// or failing that the write itself.
func (e Entry) LastVerified() string {
	if e.Verified != "" {
		return e.Verified
	}
	return e.Stamp
}

// ChainStart is when this fact's first version was written, the start of its
// exposure. A fact nothing has replaced starts at its own stamp.
func (e Entry) ChainStart() string {
	if e.Since != "" {
		return e.Since
	}
	return e.Stamp
}

// prior is the fact's class rate. A decision model's probability, when one
// was asked, blends the two classes; otherwise the text's shape picks one.
func (e Entry) prior(p Priors) float64 {
	rate := func(c int) float64 {
		if p[c] > 0 {
			return p[c]
		}
		return defaultRates[c]
	}
	if e.PriorRate > 0 {
		return e.PriorRate
	}
	if e.Vol > 0 {
		return e.Vol*rate(ShapeVolatil) + (1-e.Vol)*rate(ShapePlain)
	}
	return rate(ShapeOf(e.Text))
}

// Speed buckets a decision model may place a fact in, fastest first, with
// the change rate each stands for (changes per day).
var SpeedRates = []struct {
	Name string
	Rate float64
}{
	{"hours", 4},
	{"days", 1.0 / 3},
	{"weeks", 1.0 / 14},
	{"months", 1.0 / 60},
	{"years", 1.0 / 730},
}

// speedGate is how sure the model must be that a fact changes at all before
// its speed estimate is used. Asked about a fact that never changes, a model
// still spreads some probability over "hours" and "days"; below the gate
// that noise would set a fast rate on a settled fact.
const speedGate = 0.5

// PriorFromDecision turns a decision model's two answers into a fact's
// starting change rate: vol, the calibrated probability that the fact
// describes changing state, and speeds, its probability for each speed
// bucket. The speed is averaged geometrically (in log-rate) so that a small
// probability of "hours" cannot dominate the way it would in an arithmetic
// mean.
//
// Chosen on 90 hand-labelled facts against arithmetic and median averaging,
// an ungated blend, and a single "changing" rate: it caught as many changing
// facts as a 30-day rate with fewer wrong flags, and was the only policy that
// flagged day-scale facts within their first week. See docs/FRESHNESS.md.
func PriorFromDecision(vol float64, speeds map[string]float64) float64 {
	plain := defaultRates[ShapePlain]
	if vol <= 0 {
		return 0
	}
	if vol < speedGate || len(speeds) == 0 {
		// Not confidently changing: the speed answer is noise, and the
		// fact keeps the slow rate of a settled one.
		return (1 - vol) * plain
	}
	total, logRate := 0.0, 0.0
	for _, b := range SpeedRates {
		pr := speeds[b.Name]
		if pr <= 0 {
			continue
		}
		total += pr
		logRate += pr * math.Log(b.Rate)
	}
	if total <= 0 {
		return 0
	}
	return vol*math.Exp(logRate/total) + (1-vol)*plain
}

// Shape is the fact's class for the store's pooled rates: the decision
// model's verdict when there is one, the text's shape otherwise.
func (e Entry) Shape() int {
	if e.Vol > 0 {
		if e.Vol >= 0.5 {
			return ShapeVolatil
		}
		return ShapePlain
	}
	return ShapeOf(e.Text)
}

// ObservedDays is how long this fact's history has actually been watched:
// from its first version to the last time anyone confirmed or replaced it.
// Time since then is not evidence of anything — nobody looked — which is why
// this is not "now minus first write".
func (e Entry) ObservedDays() float64 {
	start, ok1 := parseStamp(e.ChainStart())
	last, ok2 := parseStamp(e.LastVerified())
	if !ok1 || !ok2 {
		return 0
	}
	return math.Max(last.Sub(start).Hours()/24, 0)
}

// Rate is the posterior mean change rate (per day): a Gamma prior worth
// factPriorDays of observation at the class rate, updated by the changes seen
// over the observed span. A fact with no history gets its class's rate.
func (e Entry) Rate(p Priors, _ time.Time) float64 {
	prior := e.prior(p)
	return (factPriorDays*prior + float64(e.Changes)) / (factPriorDays + e.ObservedDays())
}

// Assess decides whether a fact should be re-checked before it is used.
func (e Entry) Assess(now time.Time, p Priors, theta float64) Assessment {
	if theta <= 0 || theta >= 1 {
		theta = DefaultVerifyThreshold
	}
	a := Assessment{Tier: e.Fresh, VerifiedAt: e.LastVerified(),
		Changes: e.Changes, Verifies: e.Verifies}
	if a.Tier == "" {
		a.Tier = "auto"
	}
	// A check is a command an agent may run. One written by a source other
	// people control is not offered, however it got stored.
	if !e.Untrusted() {
		a.Check = e.Check
	}
	age := 0.0
	if t, ok := parseStamp(a.VerifiedAt); ok {
		age = math.Max(now.Sub(t).Hours()/24, 0)
	}
	a.AgeDays = math.Round(age*10) / 10
	lambda := e.Rate(p, now)
	a.PStale = math.Round((1-math.Exp(-lambda*age))*1000) / 1000

	tier, ttl, _ := ParseFresh(e.Fresh)
	switch {
	case tier == TierVolatile:
		a.Action, a.Reason = ActionVerify, "volatile: re-check on every use"
	case tier == TierStable:
		a.Action, a.Reason = ActionUse, "stable: declared not to change"
	case ttl > 0:
		if age*24 > ttl.Hours() {
			a.Action, a.Reason = ActionVerify, "older than its "+e.Fresh+" re-check interval"
		} else {
			a.Action, a.Reason = ActionUse, "verified within its "+e.Fresh+" re-check interval"
		}
	case a.PStale > theta:
		a.Action, a.Reason = ActionVerify, "likely changed since last verified"
	default:
		a.Action = ActionUse
	}
	return a
}

// SuggestTier is what the dream pass proposes for a fact given its history:
// a fact confirmed many times and never found changed has earned a longer
// interval, and one found changed repeatedly within a short span is volatile
// in practice whatever it was declared. It returns "" when the current tier
// fits.
func (e Entry) SuggestTier(now time.Time, p Priors) (tier, why string) {
	cur, ttl, _ := ParseFresh(e.Fresh)
	lambda := e.Rate(p, now)
	switch {
	case cur == TierVolatile && e.Verifies >= 5 && e.Changes == 0:
		return "30d", "confirmed " + strconv.Itoa(e.Verifies) + " times and never found changed"
	case cur == TierStable && e.Changes >= 2:
		return "auto", "declared stable but found changed " + strconv.Itoa(e.Changes) + " times"
	case cur != TierVolatile && e.Changes >= 3 && lambda > 1.0/14:
		return TierVolatile, "changed " + strconv.Itoa(e.Changes) + " times; about every " +
			strconv.Itoa(int(math.Round(1/lambda))) + " days"
	case ttl > 0 && e.Changes >= 2 && lambda > 0:
		// The interval at which P(stale) reaches the default threshold.
		want := -math.Log(1-DefaultVerifyThreshold) / lambda
		if want*24 < ttl.Hours()/2 {
			d := int(math.Max(1, math.Round(want)))
			return strconv.Itoa(d) + "d", "changes faster than its " + e.Fresh + " interval assumes"
		}
	}
	return "", ""
}
