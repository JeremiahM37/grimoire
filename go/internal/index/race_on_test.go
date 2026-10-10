//go:build race

package index

// raceEnabled is true under the race detector, which slows the 50k-fact
// corpus tests roughly twentyfold. They check ranking, not concurrency.
const raceEnabled = true
