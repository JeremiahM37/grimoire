package rulecheck

// Status of a compiled check.
const (
	StatusSuggestion = "suggestion" // measured or not, never acts
	StatusActive     = "active"     // reminds: the rule is injected when the check fires
	StatusEnforce    = "enforce"    // asks before the action (enforce: ask)
	StatusDisabled   = "disabled"   // a person turned it off
)

// User states, set by `grimoire rules enable|disable`.
const (
	UserNone    = ""
	UserEnable  = "enabled"
	UserEnforce = "enforce"
	UserOff     = "disabled"
)

// Policy is the activation policy.
type Policy struct {
	MinPrecision     float64 // default 0.9
	MinLabelled      int     // default 5
	EnforcePrecision float64 // default 0.95
	MaxMatchRate     float64 // default 0.2: a check flagging this share of calls is a topic detector
	Faithful         float64 // default 0.5: P(check says what the rule says) needed before its matches are trusted
}

// DefaultPolicy is the shipped policy.
func DefaultPolicy() Policy {
	return Policy{MinPrecision: 0.9, MinLabelled: 5, EnforcePrecision: 0.95, MaxMatchRate: 0.2, Faithful: 0.5}
}

// Evidence is everything the policy reads about one check.
type Evidence struct {
	RuleText   string
	Shape      string
	Labelled   int // historical sample + live labels
	True       int
	Matches    int
	MatchRate  float64
	User       string
	Previous   string // status before this decision, to name a demotion
	Unfaithful bool   // the labelling model judged the check not to follow from the rule
}

// Precision is true / labelled, or 0 with no labels.
func (e Evidence) Precision() float64 {
	if e.Labelled == 0 {
		return 0
	}
	return float64(e.True) / float64(e.Labelled)
}

// Decide returns the status the evidence earns and why.
func (p Policy) Decide(e Evidence) (status, reason string) {
	if e.User == UserOff {
		return StatusDisabled, "disabled by a person"
	}
	prec := e.Precision()
	if e.User == UserEnforce {
		return StatusEnforce, "enabled with enforce by a person"
	}
	if e.User == UserEnable {
		return StatusActive, "enabled by a person"
	}
	demoted := e.Previous == StatusActive || e.Previous == StatusEnforce
	switch {
	case e.Unfaithful:
		return StatusSuggestion, "check does not follow from what the rule says"
	case e.Matches == 0:
		return StatusSuggestion, "no matches in history, so nothing to measure precision on"
	case e.MatchRate > p.MaxMatchRate && p.MaxMatchRate > 0:
		return StatusSuggestion, "flags too large a share of calls to be a detector"
	case e.Labelled < p.MinLabelled:
		return StatusSuggestion, "fewer than the required labelled matches"
	case prec < p.MinPrecision:
		if demoted {
			return StatusSuggestion, "demoted: precision fell below the threshold"
		}
		return StatusSuggestion, "precision below the threshold"
	}
	if HardRule(e.RuleText) && prec >= p.EnforcePrecision {
		return StatusEnforce, "rule says never/always and precision meets the enforce threshold"
	}
	return StatusActive, "precision meets the threshold"
}
