package estimate

import "math"

// Utility is a MemRL-style learned value of one memory: an exponential moving
// average of the reward observed on injections that exposed it. It is updated
// only on exposure. A withheld injection says nothing about what the memory
// is worth, so Observe ignores it (the holdout estimators in this package use
// the withheld records instead).
//
// Q0 is the prior value, used until MinN exposed observations exist. With
// Alpha <= 0 the update is a plain running mean (alpha = 1/n), which is the
// right choice while n is small; a positive Alpha gives the usual constant
// step size and tracks drift.
type Utility struct {
	Q     float64
	N     int // exposed observations
	Alpha float64
	Q0    float64
	MinN  int
}

// NewUtility returns a Utility starting at the prior q0.
func NewUtility(q0, alpha float64, minN int) *Utility {
	return &Utility{Q: q0, Q0: q0, Alpha: alpha, MinN: minN}
}

// Observe folds one reward in when the memory was exposed. It reports whether
// the value changed.
func (u *Utility) Observe(exposed bool, reward float64) bool {
	if !exposed || math.IsNaN(reward) || math.IsInf(reward, 0) {
		return false
	}
	u.N++
	a := u.Alpha
	if a <= 0 {
		a = 1 / float64(u.N) // running mean; the first observation replaces the prior
	}
	u.Q += a * (reward - u.Q)
	return true
}

// Value returns the current utility and whether enough exposed observations
// exist to show it (otherwise the prior, with ok false).
func (u *Utility) Value() (q float64, ok bool) {
	if u.N < u.MinN || u.N == 0 {
		return u.Q0, false
	}
	return u.Q, true
}
