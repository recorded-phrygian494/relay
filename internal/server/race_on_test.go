//go:build race

package server

// raceEnabled scales the DESIGN §11 latency budgets: -race slows the
// whole process severalfold, which is not gateway overhead.
const raceEnabled = true
