package txnpure

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Violation is reported the moment a checkpoint fires while one or more
// transactions opened under the same scope are still open: the side effect
// cannot be rolled back when the surrounding transaction aborts, and the
// connection/locks are held across slow I/O.
type Violation struct {
	// Op identifies the side-effect operation that was about to run.
	Op Op
	// Scope is the name given to StartScope.
	Scope string
	// OpenTxs is the number of transactions open in the scope at check time.
	OpenTxs int
	// Stack is the stack captured at Check, resolved to function/file/line,
	// with txnpure-internal frames skipped. Empty when WithStackDepth(0).
	Stack []StackFrame
	// Attrs is the contextual metadata: detector-level (WithScopeAttrsFunc,
	// evaluated at scope start) + scope-level (WithScopeAttrs) + check-level
	// (WithCheckAttrs), in that order. Duplicate keys are kept.
	Attrs []ScopeAttr
	// Time is when the check ran.
	Time time.Time
}

// key returns the violation identity used by Allowlist / Baseline /
// throttling.
func (v Violation) key() ViolationKey {
	return ViolationKey{Scope: v.Scope, Kind: v.Op.Kind, Name: v.Op.Name}
}

// String renders a human-readable multi-line summary including the captured
// stack.
func (v Violation) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "txnpure: side effect [%s] %s ran while %d transaction(s) were open in scope %q", v.Op.Kind, v.Op.Name, v.OpenTxs, v.Scope)
	for _, f := range v.Stack {
		fmt.Fprintf(&sb, "\n  at %s (%s:%d)", f.Function, f.File, f.Line)
	}
	return sb.String()
}

// ViolationKey is the identity of a violation: the (Scope, Op) pair. The same
// operation can be legitimate in one use case and a bug in another, so the
// scope is part of the key.
type ViolationKey struct {
	Scope string
	Kind  string
	Name  string
}

// Reporter receives detected violations. Implementations decide what to do:
// fail a test, log, emit a metric, notify an error tracker.
type Reporter interface {
	Report(ctx context.Context, v Violation)
}

// ReporterFunc adapts a function to the Reporter interface.
type ReporterFunc func(ctx context.Context, v Violation)

func (f ReporterFunc) Report(ctx context.Context, v Violation) { f(ctx, v) }

// UnscopedTx is reported when a transaction is begun with no scope on its
// context (requires WithUnscopedTxDetection): such a transaction is invisible
// to every checkpoint. It carries no judgment — it marks a hole in the
// detection net, the exact analog of txnproof's unbounded writes.
type UnscopedTx struct {
	// Stack is the stack captured at begin time (txnpure-internal frames
	// skipped; database/sql frames remain). Empty when WithStackDepth(0).
	Stack []StackFrame
	// Time is when the transaction was begun.
	Time time.Time
}

// UnscopedTxReporter is an optional extension a Reporter can implement to
// also receive unscoped-transaction occurrences (requires
// WithUnscopedTxDetection).
type UnscopedTxReporter interface {
	ReportUnscopedTx(ctx context.Context, u UnscopedTx)
}

// NestedScope is reported when a scope is started on a context that already
// carries one (requires WithNestedScopeDetection). The shadow semantics are
// unchanged — transactions and checks attribute to the inner scope only — so
// a nesting occurrence is not a Violation but a coverage signal: it usually
// means two instrumentation layers overlap.
type NestedScope struct {
	// Outer is the name of the scope that was already on the context.
	Outer string
	// Inner is the name of the newly started, shadowing scope.
	Inner string
	// Time is when the inner scope was started.
	Time time.Time
}

// NestedScopeReporter is an optional extension a Reporter can implement to
// also receive nested-scope occurrences (requires WithNestedScopeDetection).
type NestedScopeReporter interface {
	ReportNestedScope(ctx context.Context, n NestedScope)
}

// LeakedTx is reported when a scope finishes while its open-transaction
// counter is still positive (requires WithLeakedTxDetection): a transaction
// outlived its logical boundary — a forgotten Commit/Rollback, or a tx handed
// to a detached goroutine. Never a Violation.
type LeakedTx struct {
	// Scope is the name given to StartScope.
	Scope string
	// OpenTxs is how many transactions were still open at scope finish.
	OpenTxs int
	// Time is when the scope finished.
	Time time.Time
}

// LeakedTxReporter is an optional extension a Reporter can implement to also
// receive leaked-transaction occurrences (requires WithLeakedTxDetection).
type LeakedTxReporter interface {
	ReportLeakedTx(ctx context.Context, l LeakedTx)
}

// UnscopedCheck is reported when a checkpoint (Check / Do / a wrapped
// RoundTripper) runs with no scope on its context (requires
// WithUnscopedCheckDetection): it can consult no counter and is otherwise
// silent, so it marks a hole in the detection net — usually a missing or
// mis-wired scope middleware on that code path. Never a Violation.
type UnscopedCheck struct {
	// Op is the operation the silent checkpoint guarded.
	Op Op
	// Stack is the stack captured at the checkpoint (txnpure-internal frames
	// skipped). Empty when WithStackDepth(0).
	Stack []StackFrame
	// Time is when the checkpoint ran.
	Time time.Time
}

// UnscopedCheckReporter is an optional extension a Reporter can implement to
// also receive unscoped-checkpoint occurrences (requires
// WithUnscopedCheckDetection).
type UnscopedCheckReporter interface {
	ReportUnscopedCheck(ctx context.Context, u UnscopedCheck)
}

// StaleAllow is reported when a check marked with AllowInTransaction runs
// outside any transaction: the allow suppressed nothing for this execution.
// Note that this is per execution — a call site reached both inside and
// outside transactions can legitimately produce both suppressions and
// StaleAllow reports.
type StaleAllow struct {
	// Scope is the name of the scope the check ran in.
	Scope string
	// Op is the operation the check guarded.
	Op Op
	// Reason is the reason given to AllowInTransaction.
	Reason string
}

// StaleAllowReporter is an optional extension a Reporter can implement to
// receive stale AllowInTransaction marks (see StaleAllow).
type StaleAllowReporter interface {
	ReportStaleAllow(ctx context.Context, s StaleAllow)
}

// TestingT is the subset of *testing.T that txnpure's test helpers need.
type TestingT interface {
	Helper()
	Errorf(format string, args ...any)
}

// CollectingReporter accumulates reports in memory. Intended for tests.
type CollectingReporter struct {
	mu             sync.Mutex
	violations     []Violation
	unscoped       []UnscopedTx
	unscopedChecks []UnscopedCheck
	nested         []NestedScope
	leaked         []LeakedTx
	staleAllows    []StaleAllow
}

var (
	_ Reporter              = (*CollectingReporter)(nil)
	_ UnscopedTxReporter    = (*CollectingReporter)(nil)
	_ UnscopedCheckReporter = (*CollectingReporter)(nil)
	_ NestedScopeReporter   = (*CollectingReporter)(nil)
	_ LeakedTxReporter      = (*CollectingReporter)(nil)
	_ StaleAllowReporter    = (*CollectingReporter)(nil)
)

// NewCollectingReporter creates an empty CollectingReporter.
func NewCollectingReporter() *CollectingReporter { return &CollectingReporter{} }

func (r *CollectingReporter) Report(_ context.Context, v Violation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.violations = append(r.violations, v)
}

func (r *CollectingReporter) ReportUnscopedTx(_ context.Context, u UnscopedTx) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unscoped = append(r.unscoped, u)
}

func (r *CollectingReporter) ReportUnscopedCheck(_ context.Context, u UnscopedCheck) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unscopedChecks = append(r.unscopedChecks, u)
}

func (r *CollectingReporter) ReportNestedScope(_ context.Context, n NestedScope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nested = append(r.nested, n)
}

func (r *CollectingReporter) ReportLeakedTx(_ context.Context, l LeakedTx) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaked = append(r.leaked, l)
}

func (r *CollectingReporter) ReportStaleAllow(_ context.Context, s StaleAllow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.staleAllows = append(r.staleAllows, s)
}

// Violations returns a copy of the collected violations.
func (r *CollectingReporter) Violations() []Violation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Violation, len(r.violations))
	copy(out, r.violations)
	return out
}

// UnscopedTxs returns a copy of the collected unscoped-transaction reports.
func (r *CollectingReporter) UnscopedTxs() []UnscopedTx {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]UnscopedTx, len(r.unscoped))
	copy(out, r.unscoped)
	return out
}

// UnscopedChecks returns a copy of the collected unscoped-checkpoint reports.
func (r *CollectingReporter) UnscopedChecks() []UnscopedCheck {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]UnscopedCheck, len(r.unscopedChecks))
	copy(out, r.unscopedChecks)
	return out
}

// NestedScopes returns a copy of the collected nested-scope occurrences.
func (r *CollectingReporter) NestedScopes() []NestedScope {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]NestedScope, len(r.nested))
	copy(out, r.nested)
	return out
}

// LeakedTxs returns a copy of the collected leaked-transaction reports.
func (r *CollectingReporter) LeakedTxs() []LeakedTx {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LeakedTx, len(r.leaked))
	copy(out, r.leaked)
	return out
}

// StaleAllows returns a copy of the collected stale AllowInTransaction
// reports.
func (r *CollectingReporter) StaleAllows() []StaleAllow {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]StaleAllow, len(r.staleAllows))
	copy(out, r.staleAllows)
	return out
}

// Reset clears everything collected so far.
func (r *CollectingReporter) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.violations = nil
	r.unscoped = nil
	r.unscopedChecks = nil
	r.nested = nil
	r.leaked = nil
	r.staleAllows = nil
}

// RequireNoViolations fails the test with one error per collected violation.
func (r *CollectingReporter) RequireNoViolations(t TestingT) {
	t.Helper()
	for _, v := range r.Violations() {
		t.Errorf("%s", v.String())
	}
}

// RequireNoUnscopedTxs fails the test with one error per collected unscoped
// transaction, enforcing that every transaction in the exercised code was
// begun with a scope in its context (requires WithUnscopedTxDetection).
func (r *CollectingReporter) RequireNoUnscopedTxs(t TestingT) {
	t.Helper()
	for _, u := range r.UnscopedTxs() {
		loc := ""
		if len(u.Stack) > 0 {
			loc = fmt.Sprintf(" at %s (%s:%d)", u.Stack[0].Function, u.Stack[0].File, u.Stack[0].Line)
		}
		t.Errorf("txnpure: transaction begun with no scope in its context%s", loc)
	}
}

// RequireNoUnscopedChecks fails the test with one error per collected
// unscoped checkpoint, enforcing that every instrumented side effect in the
// exercised code ran with a scope in its context — i.e. the scope middleware
// covers the path (requires WithUnscopedCheckDetection).
func (r *CollectingReporter) RequireNoUnscopedChecks(t TestingT) {
	t.Helper()
	for _, u := range r.UnscopedChecks() {
		loc := ""
		if len(u.Stack) > 0 {
			loc = fmt.Sprintf(" at %s (%s:%d)", u.Stack[0].Function, u.Stack[0].File, u.Stack[0].Line)
		}
		t.Errorf("txnpure: checkpoint [%s] %s ran with no scope in its context%s", u.Op.Kind, u.Op.Name, loc)
	}
}

// RequireNoNestedScopes fails the test with one error per collected
// nested-scope occurrence, enforcing that instrumentation layers do not
// overlap (requires WithNestedScopeDetection).
func (r *CollectingReporter) RequireNoNestedScopes(t TestingT) {
	t.Helper()
	for _, n := range r.NestedScopes() {
		t.Errorf("txnpure: scope %q started inside scope %q; transactions and checks attribute to the inner one only", n.Inner, n.Outer)
	}
}

// RequireNoLeakedTxs fails the test with one error per collected leaked
// transaction, enforcing that every transaction closes before its scope
// finishes (requires WithLeakedTxDetection).
func (r *CollectingReporter) RequireNoLeakedTxs(t TestingT) {
	t.Helper()
	for _, l := range r.LeakedTxs() {
		t.Errorf("txnpure: scope %q finished with %d transaction(s) still open", l.Scope, l.OpenTxs)
	}
}

// RequireNoStaleAllows fails the test with one error per stale
// AllowInTransaction mark, keeping in-code allows subject to the same rot
// discipline as Allowlist.UnusedEntries.
func (r *CollectingReporter) RequireNoStaleAllows(t TestingT) {
	t.Helper()
	for _, s := range r.StaleAllows() {
		t.Errorf("txnpure: check [%s] %s in scope %q is marked AllowInTransaction (%s) but ran outside any transaction; remove the stale allow", s.Op.Kind, s.Op.Name, s.Scope, s.Reason)
	}
}

// SlogReporter reports through a *slog.Logger. Intended for production
// monitoring.
type SlogReporter struct {
	Logger *slog.Logger
}

var (
	_ Reporter              = (*SlogReporter)(nil)
	_ UnscopedTxReporter    = (*SlogReporter)(nil)
	_ UnscopedCheckReporter = (*SlogReporter)(nil)
	_ NestedScopeReporter   = (*SlogReporter)(nil)
	_ LeakedTxReporter      = (*SlogReporter)(nil)
	_ StaleAllowReporter    = (*SlogReporter)(nil)
)

// NewSlogReporter creates a SlogReporter. A nil logger means slog.Default().
func NewSlogReporter(l *slog.Logger) *SlogReporter {
	if l == nil {
		l = slog.Default()
	}
	return &SlogReporter{Logger: l}
}

func stackStrings(stack []StackFrame) []string {
	out := make([]string, len(stack))
	for i, f := range stack {
		out[i] = fmt.Sprintf("%s (%s:%d)", f.Function, f.File, f.Line)
	}
	return out
}

func (r *SlogReporter) Report(ctx context.Context, v Violation) {
	args := []any{
		slog.String("kind", v.Op.Kind),
		slog.String("name", v.Op.Name),
		slog.String("scope", v.Scope),
		slog.Int("open_txs", v.OpenTxs),
	}
	if len(v.Stack) > 0 {
		args = append(args, slog.Any("stack", stackStrings(v.Stack)))
	}
	for _, a := range SlogAttrs(v.Attrs) {
		args = append(args, a)
	}
	r.Logger.ErrorContext(ctx, "txnpure: side effect inside an open transaction", args...)
}

func (r *SlogReporter) ReportUnscopedTx(ctx context.Context, u UnscopedTx) {
	args := []any{}
	if len(u.Stack) > 0 {
		args = append(args, slog.Any("stack", stackStrings(u.Stack)))
	}
	r.Logger.WarnContext(ctx, "txnpure: transaction begun with no scope in its context", args...)
}

func (r *SlogReporter) ReportUnscopedCheck(ctx context.Context, u UnscopedCheck) {
	args := []any{
		slog.String("kind", u.Op.Kind),
		slog.String("name", u.Op.Name),
	}
	if len(u.Stack) > 0 {
		args = append(args, slog.Any("stack", stackStrings(u.Stack)))
	}
	r.Logger.WarnContext(ctx, "txnpure: checkpoint ran with no scope in its context", args...)
}

func (r *SlogReporter) ReportNestedScope(ctx context.Context, n NestedScope) {
	r.Logger.WarnContext(ctx, "txnpure: scope started inside another scope; transactions and checks attribute to the inner one only",
		slog.String("outer", n.Outer),
		slog.String("inner", n.Inner),
	)
}

func (r *SlogReporter) ReportLeakedTx(ctx context.Context, l LeakedTx) {
	r.Logger.WarnContext(ctx, "txnpure: scope finished with transactions still open",
		slog.String("scope", l.Scope),
		slog.Int("open_txs", l.OpenTxs),
	)
}

func (r *SlogReporter) ReportStaleAllow(ctx context.Context, s StaleAllow) {
	r.Logger.WarnContext(ctx, "txnpure: check is marked AllowInTransaction but ran outside any transaction",
		slog.String("scope", s.Scope),
		slog.String("kind", s.Op.Kind),
		slog.String("name", s.Op.Name),
		slog.String("reason", s.Reason),
	)
}
