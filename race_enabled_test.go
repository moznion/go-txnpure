//go:build race

package txnpure

// raceEnabled reports whether the race detector is on: the race runtime
// allocates, so exact allocation-budget assertions are skipped under -race.
const raceEnabled = true
