package txnpure

import (
	"context"
	"testing"
	"time"
)

// In-package test: the injectable clock (ThrottlingReporter.now) is
// unexported to keep the public API minimal.
func newThrottled(next Reporter, interval time.Duration) (*ThrottlingReporter, func(d time.Duration)) {
	r := NewThrottlingReporter(next, interval)
	now := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return now }
	return r, func(d time.Duration) { now = now.Add(d) }
}

func violationNamed(scope string) Violation {
	return Violation{Op: Op{Kind: "http", Name: "GET api.example.com"}, Scope: scope, OpenTxs: 1}
}

func TestThrottlingReporterViolations(t *testing.T) {
	ctx := context.Background()
	collect := NewCollectingReporter()
	r, advance := newThrottled(collect, time.Minute)

	r.Report(ctx, violationNamed("A"))
	r.Report(ctx, violationNamed("A")) // suppressed
	r.Report(ctx, violationNamed("A")) // suppressed
	r.Report(ctx, violationNamed("B")) // different identity: own window
	if got := len(collect.Violations()); got != 2 {
		t.Fatalf("got %d forwarded violations, want 2", got)
	}

	advance(time.Minute)
	r.Report(ctx, violationNamed("A")) // window elapsed: forwarded again
	if got := len(collect.Violations()); got != 3 {
		t.Fatalf("after interval: got %d forwarded, want 3", got)
	}

	suppressed := r.SuppressedViolations()
	key := "A\x00http\x00GET api.example.com"
	if suppressed[key] != 2 {
		t.Errorf("SuppressedViolations = %v, want 2 for %q", suppressed, key)
	}
	if len(suppressed) != 1 {
		t.Errorf("SuppressedViolations = %v, want zero-count keys omitted", suppressed)
	}
}

func TestThrottlingReporterNonPositiveIntervalForwardsAll(t *testing.T) {
	ctx := context.Background()
	collect := NewCollectingReporter()
	r := NewThrottlingReporter(collect, 0)
	for range 5 {
		r.Report(ctx, violationNamed("A"))
	}
	if got := len(collect.Violations()); got != 5 {
		t.Fatalf("got %d forwarded, want all 5", got)
	}
}

func TestThrottlingReporterOptionalSignals(t *testing.T) {
	ctx := context.Background()
	collect := NewCollectingReporter()
	r, advance := newThrottled(collect, time.Minute)

	u := UnscopedTx{Stack: []StackFrame{{Function: "main.f", File: "main.go", Line: 10}}}
	r.ReportUnscopedTx(ctx, u)
	r.ReportUnscopedTx(ctx, u) // same call site: suppressed
	r.ReportUnscopedTx(ctx, UnscopedTx{Stack: []StackFrame{{Function: "main.g", File: "main.go", Line: 99}}})
	if got := len(collect.UnscopedTxs()); got != 2 {
		t.Errorf("unscoped: got %d forwarded, want 2", got)
	}
	if s := r.SuppressedUnscopedTxs(); s["main.go:10"] != 1 {
		t.Errorf("SuppressedUnscopedTxs = %v", s)
	}

	sa := StaleAllow{Scope: "A", Op: Op{Kind: "http", Name: "n"}, Reason: "r"}
	r.ReportStaleAllow(ctx, sa)
	r.ReportStaleAllow(ctx, sa)
	if got := len(collect.StaleAllows()); got != 1 {
		t.Errorf("stale allows: got %d forwarded, want 1", got)
	}

	n := NestedScope{Outer: "O", Inner: "I"}
	r.ReportNestedScope(ctx, n)
	r.ReportNestedScope(ctx, n)
	if got := len(collect.NestedScopes()); got != 1 {
		t.Errorf("nested: got %d forwarded, want 1", got)
	}
	if s := r.SuppressedNestedScopes(); s["O\x00I"] != 1 {
		t.Errorf("SuppressedNestedScopes = %v", s)
	}

	l := LeakedTx{Scope: "A", OpenTxs: 1}
	r.ReportLeakedTx(ctx, l)
	r.ReportLeakedTx(ctx, l)
	if got := len(collect.LeakedTxs()); got != 1 {
		t.Errorf("leaked: got %d forwarded, want 1", got)
	}
	if s := r.SuppressedLeakedTxs(); s["A"] != 1 {
		t.Errorf("SuppressedLeakedTxs = %v", s)
	}
	if s := r.SuppressedStaleAllows(); s["A\x00http\x00n"] != 1 {
		t.Errorf("SuppressedStaleAllows = %v", s)
	}

	advance(time.Minute)
	r.ReportStaleAllow(ctx, sa)
	r.ReportNestedScope(ctx, n)
	r.ReportLeakedTx(ctx, l)
	if got := len(collect.StaleAllows()); got != 2 {
		t.Errorf("stale allows after interval: got %d, want 2", got)
	}
	if got := len(collect.NestedScopes()); got != 2 {
		t.Errorf("nested after interval: got %d, want 2", got)
	}
	if got := len(collect.LeakedTxs()); got != 2 {
		t.Errorf("leaked after interval: got %d, want 2", got)
	}
}

// Past the call-site cap the unscoped-tx throttle fails open: forwarded
// unthrottled, never lost.
func TestThrottlingReporterUnscopedTxKeyCapFailsOpen(t *testing.T) {
	ctx := context.Background()
	collect := NewCollectingReporter()
	r, _ := newThrottled(collect, time.Minute)
	for i := range maxUnscopedTxKeys {
		r.ReportUnscopedTx(ctx, UnscopedTx{Stack: []StackFrame{{File: "f.go", Line: i}}})
	}
	before := len(collect.UnscopedTxs())
	r.ReportUnscopedTx(ctx, UnscopedTx{Stack: []StackFrame{{File: "overflow.go", Line: 1}}})
	r.ReportUnscopedTx(ctx, UnscopedTx{Stack: []StackFrame{{File: "overflow.go", Line: 1}}})
	if got := len(collect.UnscopedTxs()); got != before+2 {
		t.Fatalf("got %d forwarded, want %d — past the cap reports must fail open", got, before+2)
	}
}
