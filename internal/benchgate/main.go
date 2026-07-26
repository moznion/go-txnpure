// Command benchgate compares two `go test -bench -benchmem` outputs (base vs
// head) and exits non-zero when head regresses:
//
//   - allocs/op or B/op increased on any benchmark present in both runs.
//     Allocation counts are deterministic, so any increase is a real
//     regression, never noise.
//   - median ns/op regressed by more than -threshold (default +25%) AND by
//     more than -floor nanoseconds (default 5ns). The floor keeps
//     nanosecond-scale benchmarks from flaking on scheduler jitter; the
//     comparison is meant to run base and head in the same CI job on the
//     same runner.
//
// Benchmarks present in only one input are reported but never gate.
//
// Usage: benchgate [-threshold 0.25] [-floor 5] base.txt head.txt
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

type samples struct {
	ns     []float64
	bytes  []float64
	allocs []float64
}

func main() {
	threshold := flag.Float64("threshold", 0.25, "maximum tolerated relative ns/op regression (0.25 = +25%)")
	floor := flag.Float64("floor", 5, "minimum absolute ns/op regression (ns) to fail; guards tiny benchmarks")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: benchgate [-threshold 0.25] [-floor 5] base.txt head.txt")
		os.Exit(2)
	}
	base, err := parseFile(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchgate:", err)
		os.Exit(2)
	}
	head, err := parseFile(flag.Arg(1))
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchgate:", err)
		os.Exit(2)
	}
	report, failures := compare(base, head, *threshold, *floor)
	fmt.Print(report)
	if failures > 0 {
		fmt.Printf("\nbenchgate: %d regression(s) against base. If the regression is intended and justified, adjust the benchmark or budget in the same PR and say why.\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nbenchgate: no regressions.")
}

func parseFile(path string) (map[string]*samples, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return parse(f)
}

// parse extracts per-benchmark samples from `go test -bench` output lines of
// the form:
//
//	BenchmarkName-12   123456   58.04 ns/op   216 B/op   3 allocs/op
func parse(r io.Reader) (map[string]*samples, error) {
	out := map[string]*samples{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || !strings.HasPrefix(fields[0], "Benchmark") {
			continue
		}
		name := stripProcs(fields[0])
		s := out[name]
		if s == nil {
			s = &samples{}
			out[name] = s
		}
		for i := 2; i < len(fields)-1; i++ {
			v, err := strconv.ParseFloat(fields[i], 64)
			if err != nil {
				continue
			}
			switch fields[i+1] {
			case "ns/op":
				s.ns = append(s.ns, v)
			case "B/op":
				s.bytes = append(s.bytes, v)
			case "allocs/op":
				s.allocs = append(s.allocs, v)
			}
		}
	}
	return out, sc.Err()
}

// stripProcs removes the trailing -GOMAXPROCS suffix from a benchmark name.
func stripProcs(name string) string {
	i := strings.LastIndexByte(name, '-')
	if i <= 0 {
		return name
	}
	for _, c := range name[i+1:] {
		if c < '0' || c > '9' {
			return name
		}
	}
	return name[:i]
}

func compare(base, head map[string]*samples, threshold, floor float64) (string, int) {
	var sb strings.Builder
	failures := 0
	for _, name := range sortedKeys(head) {
		h := head[name]
		b, ok := base[name]
		if !ok {
			fmt.Fprintf(&sb, "NEW   %-55s %10.2f ns/op %8.0f B/op %6.0f allocs/op\n",
				name, median(h.ns), maxOf(h.bytes), maxOf(h.allocs))
			continue
		}
		bNs, hNs := median(b.ns), median(h.ns)
		verdict := "ok"
		if maxOf(h.allocs) > maxOf(b.allocs) {
			verdict = "FAIL allocs/op increased"
			failures++
		} else if maxOf(h.bytes) > maxOf(b.bytes) {
			verdict = "FAIL B/op increased"
			failures++
		} else if hNs > bNs*(1+threshold) && hNs-bNs > floor {
			verdict = fmt.Sprintf("FAIL ns/op regressed beyond +%.0f%%", threshold*100)
			failures++
		}
		fmt.Fprintf(&sb, "%-61s %10.2f -> %10.2f ns/op (%+6.1f%%)  %3.0f -> %3.0f allocs/op  %s\n",
			name, bNs, hNs, pct(bNs, hNs), maxOf(b.allocs), maxOf(h.allocs), verdict)
	}
	for _, name := range sortedKeys(base) {
		if _, ok := head[name]; !ok {
			fmt.Fprintf(&sb, "GONE  %s\n", name)
		}
	}
	return sb.String(), failures
}

func sortedKeys(m map[string]*samples) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func maxOf(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

func pct(base, head float64) float64 {
	if base == 0 {
		return 0
	}
	return (head - base) / base * 100
}
