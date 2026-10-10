//go:build race

package replay

// raceEnabled is true under the race detector, which slows this package's
// hot loops roughly tenfold; timing budgets do not apply there.
const raceEnabled = true
