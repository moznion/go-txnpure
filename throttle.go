package txnpure

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	// maxUnscopedTxKeys bounds how many distinct call-site keys the
	// unscoped-transaction throttle tracks. Call sites are code-defined and
	// small in practice, but the key is derived from stack frames (open-ended
	// in pathological cases, and empty when stack capture is disabled), so
	// the map is capped: once full, reports for sites not already tracked are
	// forwarded unthrottled rather than growing the map without bound.
	maxUnscopedTxKeys = 1024
)

// ThrottlingReporter wraps another Reporter and deduplicates repeated
// reports, so that a violating checkpoint on a hot path does not fire the
// wrapped reporter on every request. Intended for production monitoring.
//
// Per violation identity (scope, kind, name), the first Violation is
// forwarded to the wrapped reporter immediately; subsequent Violations for
// the same identity within the configured interval are suppressed; once the
// interval has elapsed, the next Violation is forwarded again and a new
// interval starts.
//
// The optional reporter extensions are throttled with the same interval but
// with their own keys and independent windows:
//
//   - Unscoped transactions (UnscopedTxReporter) are throttled per begin call
//     site, keyed by the first captured stack frame — a hot-path unscoped
//     begin repeats the same site. With stack capture disabled all unscoped
//     begins share one key.
//   - Stale AllowInTransaction marks (StaleAllowReporter) are throttled per
//     (scope, kind, name), independently of that identity's Violation window.
//   - Nested scopes (NestedScopeReporter) are throttled per outer/inner name
//     pair.
//   - Leaked transactions (LeakedTxReporter) are throttled per scope name.
//
// Each extension is forwarded only when the wrapped reporter implements the
// corresponding interface, so wrapping neither swallows nor fabricates those
// signals.
//
// Suppressed reports are not silently lost: cumulative per-key suppression
// counts are available via the Suppressed* methods, meant to be polled
// periodically (e.g. logged or exported as metrics on a ticker) to recover
// the true report volume.
//
// Memory stays bounded: the identity- and name-keyed maps grow with sets
// that are code-defined and small in practice; the call-site-keyed map is
// capped at maxUnscopedTxKeys.
type ThrottlingReporter struct {
	next     Reporter
	interval time.Duration
	now      func() time.Time // injectable clock for tests

	mu             sync.Mutex
	violations     map[string]*throttleState
	unscoped       map[string]*throttleState
	unscopedChecks map[string]*throttleState
	staleAllows    map[string]*throttleState
	nested         map[string]*throttleState
	leaked         map[string]*throttleState
}

var (
	_ Reporter              = (*ThrottlingReporter)(nil)
	_ UnscopedTxReporter    = (*ThrottlingReporter)(nil)
	_ UnscopedCheckReporter = (*ThrottlingReporter)(nil)
	_ NestedScopeReporter   = (*ThrottlingReporter)(nil)
	_ LeakedTxReporter      = (*ThrottlingReporter)(nil)
	_ StaleAllowReporter    = (*ThrottlingReporter)(nil)
)

// throttleState tracks one throttle key: when a report was last forwarded and
// how many reports have been suppressed in total since creation.
type throttleState struct {
	lastForwarded time.Time
	suppressed    int
}

// NewThrottlingReporter wraps next so that repeated reports for the same key
// are forwarded at most once per interval. A non-positive interval disables
// throttling: every report is forwarded.
func NewThrottlingReporter(next Reporter, interval time.Duration) *ThrottlingReporter {
	return &ThrottlingReporter{
		next:           next,
		interval:       interval,
		now:            time.Now,
		violations:     map[string]*throttleState{},
		unscoped:       map[string]*throttleState{},
		unscopedChecks: map[string]*throttleState{},
		staleAllows:    map[string]*throttleState{},
		nested:         map[string]*throttleState{},
		leaked:         map[string]*throttleState{},
	}
}

// admit decides whether a report for key should be forwarded now, updating
// the throttle window and the suppression count. maxKeys > 0 caps the number
// of distinct tracked keys; reports for untracked keys beyond the cap are
// forwarded unthrottled (failing open loses dedup, never reports).
func (r *ThrottlingReporter) admit(m map[string]*throttleState, key string, maxKeys int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := m[key]
	if !ok {
		if maxKeys > 0 && len(m) >= maxKeys {
			return true
		}
		m[key] = &throttleState{lastForwarded: r.now()}
		return true
	}
	now := r.now()
	if now.Sub(st.lastForwarded) >= r.interval {
		st.lastForwarded = now
		return true
	}
	st.suppressed++
	return false
}

func identityKey(k ViolationKey) string {
	return k.Scope + "\x00" + k.Kind + "\x00" + k.Name
}

// Report forwards the first Violation per identity immediately and at most
// one more per interval afterwards; the rest are counted as suppressed.
func (r *ThrottlingReporter) Report(ctx context.Context, v Violation) {
	if r.interval <= 0 || r.admit(r.violations, identityKey(v.key()), 0) {
		r.next.Report(ctx, v)
	}
}

// ReportUnscopedTx forwards unscoped-transaction reports throttled per begin
// call site. It is a no-op when the wrapped reporter does not implement
// UnscopedTxReporter.
func (r *ThrottlingReporter) ReportUnscopedTx(ctx context.Context, u UnscopedTx) {
	ur, ok := r.next.(UnscopedTxReporter)
	if !ok {
		return
	}
	if r.interval <= 0 || r.admit(r.unscoped, unscopedTxKey(u), maxUnscopedTxKeys) {
		ur.ReportUnscopedTx(ctx, u)
	}
}

// ReportUnscopedCheck forwards unscoped-checkpoint reports throttled per
// (kind, name) — an unscoped checkpoint has no scope, so its Op identity is
// the dedup unit. It is a no-op when the wrapped reporter does not implement
// UnscopedCheckReporter.
func (r *ThrottlingReporter) ReportUnscopedCheck(ctx context.Context, u UnscopedCheck) {
	ur, ok := r.next.(UnscopedCheckReporter)
	if !ok {
		return
	}
	if r.interval <= 0 || r.admit(r.unscopedChecks, u.Op.Kind+"\x00"+u.Op.Name, 0) {
		ur.ReportUnscopedCheck(ctx, u)
	}
}

// ReportStaleAllow forwards stale AllowInTransaction reports throttled per
// (scope, kind, name), independently of that identity's Violation window. It
// is a no-op when the wrapped reporter does not implement StaleAllowReporter.
func (r *ThrottlingReporter) ReportStaleAllow(ctx context.Context, s StaleAllow) {
	sr, ok := r.next.(StaleAllowReporter)
	if !ok {
		return
	}
	key := identityKey(ViolationKey{Scope: s.Scope, Kind: s.Op.Kind, Name: s.Op.Name})
	if r.interval <= 0 || r.admit(r.staleAllows, key, 0) {
		sr.ReportStaleAllow(ctx, s)
	}
}

// ReportNestedScope forwards nested-scope occurrences throttled per
// outer/inner name pair. It is a no-op when the wrapped reporter does not
// implement NestedScopeReporter.
func (r *ThrottlingReporter) ReportNestedScope(ctx context.Context, n NestedScope) {
	nr, ok := r.next.(NestedScopeReporter)
	if !ok {
		return
	}
	if r.interval <= 0 || r.admit(r.nested, n.Outer+"\x00"+n.Inner, 0) {
		nr.ReportNestedScope(ctx, n)
	}
}

// ReportLeakedTx forwards leaked-transaction reports throttled per scope
// name. It is a no-op when the wrapped reporter does not implement
// LeakedTxReporter.
func (r *ThrottlingReporter) ReportLeakedTx(ctx context.Context, l LeakedTx) {
	lr, ok := r.next.(LeakedTxReporter)
	if !ok {
		return
	}
	if r.interval <= 0 || r.admit(r.leaked, l.Scope, 0) {
		lr.ReportLeakedTx(ctx, l)
	}
}

// SuppressedViolations returns the cumulative number of suppressed Violations
// per "scope\x00kind\x00name" identity since the reporter was created. Counts
// only grow; keys with zero suppressions are omitted. Poll it periodically to
// recover the true violation volume behind the throttled stream.
func (r *ThrottlingReporter) SuppressedViolations() map[string]int {
	return r.snapshot(r.violations)
}

// SuppressedUnscopedTxs returns the cumulative number of suppressed
// unscoped-transaction reports per call-site key since the reporter was
// created.
func (r *ThrottlingReporter) SuppressedUnscopedTxs() map[string]int {
	return r.snapshot(r.unscoped)
}

// SuppressedUnscopedChecks returns the cumulative number of suppressed
// unscoped-checkpoint reports per "kind\x00name" identity since the reporter
// was created.
func (r *ThrottlingReporter) SuppressedUnscopedChecks() map[string]int {
	return r.snapshot(r.unscopedChecks)
}

// SuppressedStaleAllows returns the cumulative number of suppressed stale
// AllowInTransaction reports per "scope\x00kind\x00name" identity since the
// reporter was created.
func (r *ThrottlingReporter) SuppressedStaleAllows() map[string]int {
	return r.snapshot(r.staleAllows)
}

// SuppressedNestedScopes returns the cumulative number of suppressed
// nested-scope reports per "outer\x00inner" name pair.
func (r *ThrottlingReporter) SuppressedNestedScopes() map[string]int {
	return r.snapshot(r.nested)
}

// SuppressedLeakedTxs returns the cumulative number of suppressed
// leaked-transaction reports per scope name.
func (r *ThrottlingReporter) SuppressedLeakedTxs() map[string]int {
	return r.snapshot(r.leaked)
}

func (r *ThrottlingReporter) snapshot(m map[string]*throttleState) map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(m))
	for k, st := range m {
		if st.suppressed > 0 {
			out[k] = st.suppressed
		}
	}
	return out
}

// unscopedTxKey derives the throttle key for an unscoped transaction: the
// begin call site (first captured stack frame). Empty when stack capture is
// disabled, so all unscoped begins share one window then.
func unscopedTxKey(u UnscopedTx) string {
	if len(u.Stack) == 0 {
		return ""
	}
	f := u.Stack[0]
	return fmt.Sprintf("%s:%d", f.File, f.Line)
}
