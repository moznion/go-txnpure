package main

import (
	"strings"
	"testing"
)

const baseOut = `
goos: linux
BenchmarkCheck/scope_no_tx-12   	100000000	         4.15 ns/op	       0 B/op	       0 allocs/op
BenchmarkCheck/scope_no_tx-12   	100000000	         4.20 ns/op	       0 B/op	       0 allocs/op
BenchmarkCheck/scope_no_tx-12   	100000000	         4.10 ns/op	       0 B/op	       0 allocs/op
BenchmarkObserve/select-12      	50000000	        29.41 ns/op	       8 B/op	       1 allocs/op
BenchmarkGone-12                	10000000	       100.00 ns/op	       0 B/op	       0 allocs/op
PASS
`

func parseString(t *testing.T, s string) map[string]*samples {
	t.Helper()
	m, err := parse(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParse(t *testing.T) {
	m := parseString(t, baseOut)
	s, ok := m["BenchmarkCheck/scope_no_tx"]
	if !ok {
		t.Fatalf("missing benchmark; got keys %v", sortedKeys(m))
	}
	if len(s.ns) != 3 || median(s.ns) != 4.15 {
		t.Errorf("ns samples = %v, want 3 samples with median 4.15", s.ns)
	}
	if maxOf(m["BenchmarkObserve/select"].allocs) != 1 {
		t.Errorf("allocs = %v, want 1", m["BenchmarkObserve/select"].allocs)
	}
}

func TestCompareCleanRun(t *testing.T) {
	base := parseString(t, baseOut)
	head := parseString(t, `
BenchmarkCheck/scope_no_tx-12   	100000000	         4.30 ns/op	       0 B/op	       0 allocs/op
BenchmarkObserve/select-12      	50000000	        20.00 ns/op	       0 B/op	       0 allocs/op
BenchmarkNew-12                 	10000000	        50.00 ns/op	      16 B/op	       2 allocs/op
`)
	report, failures := compare(base, head, 0.25, 5)
	if failures != 0 {
		t.Errorf("failures = %d, want 0; report:\n%s", failures, report)
	}
	for _, want := range []string{"NEW   BenchmarkNew", "GONE  BenchmarkGone"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
}

func TestCompareAllocRegression(t *testing.T) {
	base := parseString(t, baseOut)
	head := parseString(t, `
BenchmarkCheck/scope_no_tx-12   	100000000	         4.15 ns/op	      48 B/op	       1 allocs/op
`)
	report, failures := compare(base, head, 0.25, 5)
	if failures != 1 || !strings.Contains(report, "FAIL allocs/op increased") {
		t.Errorf("failures = %d, report:\n%s", failures, report)
	}
}

func TestCompareBytesRegression(t *testing.T) {
	base := parseString(t, `BenchmarkX-12 	1000	 100.00 ns/op	      16 B/op	       1 allocs/op`)
	head := parseString(t, `BenchmarkX-12 	1000	 100.00 ns/op	      64 B/op	       1 allocs/op`)
	report, failures := compare(base, head, 0.25, 5)
	if failures != 1 || !strings.Contains(report, "FAIL B/op increased") {
		t.Errorf("failures = %d, report:\n%s", failures, report)
	}
}

func TestCompareTimeRegression(t *testing.T) {
	base := parseString(t, `BenchmarkX-12 	1000	 100.00 ns/op	       0 B/op	       0 allocs/op`)
	head := parseString(t, `BenchmarkX-12 	1000	 140.00 ns/op	       0 B/op	       0 allocs/op`)
	report, failures := compare(base, head, 0.25, 5)
	if failures != 1 || !strings.Contains(report, "FAIL ns/op regressed") {
		t.Errorf("failures = %d, report:\n%s", failures, report)
	}
}

// A large relative regression on a nanosecond-scale benchmark must not fail:
// the absolute floor absorbs scheduler jitter.
func TestCompareTinyBenchmarkFloor(t *testing.T) {
	base := parseString(t, `BenchmarkX-12 	1000	 2.00 ns/op	       0 B/op	       0 allocs/op`)
	head := parseString(t, `BenchmarkX-12 	1000	 3.00 ns/op	       0 B/op	       0 allocs/op`)
	report, failures := compare(base, head, 0.25, 5)
	if failures != 0 {
		t.Errorf("failures = %d, want 0 (floor should absorb +1ns); report:\n%s", failures, report)
	}
}

// Benchmarks without -benchmem lines (no B/op / allocs/op) must not gate on
// allocations.
func TestCompareNoBenchmem(t *testing.T) {
	base := parseString(t, `BenchmarkX-12 	1000	 100.00 ns/op`)
	head := parseString(t, `BenchmarkX-12 	1000	 101.00 ns/op`)
	if _, failures := compare(base, head, 0.25, 5); failures != 0 {
		t.Errorf("failures = %d, want 0", failures)
	}
}
