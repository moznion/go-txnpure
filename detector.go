package txnpure

import (
	"context"
	"sync/atomic"
	"time"
)

// Detector is the core of txnpure. Wrap a driver (or connector) with it, mark
// scopes with StartScope / InScope, instrument side-effect clients with Check
// / Do (or WrapRoundTripper), and it reports a Violation the moment a
// checkpoint fires while a transaction opened under the same scope is still
// open.
type Detector struct {
	reporters           []Reporter
	allowlist           *Allowlist
	classify            Classifier
	stmtCheckers        []StatementChecker
	attrsFunc           func(ctx context.Context) []ScopeAttr
	reportUnscoped      bool
	reportUnscopedCheck bool
	reportNested        bool
	reportLeaked        bool
	stackDepth          int

	txSeq atomic.Uint64
}

// Option configures a Detector.
type Option func(*Detector)

// WithReporter appends reporters that receive detected violations.
func WithReporter(rs ...Reporter) Option {
	return func(d *Detector) { d.reporters = append(d.reporters, rs...) }
}

// WithAllowlist installs an allowlist of (scope, op) pairs whose violations
// are intentionally suppressed.
func WithAllowlist(a *Allowlist) Option {
	return func(d *Detector) { d.allowlist = a }
}

// WithClassifier replaces DefaultClassifier for transaction-lifecycle
// classification of textual statements.
func WithClassifier(c Classifier) Option {
	return func(d *Detector) { d.classify = c }
}

// WithStatementChecker registers matchers that declare statements executed
// through a wrapped driver to be external calls, in addition to the built-in
// detection (HTTP via WrapRoundTripper, cross-connection writes). Each matched
// statement is reported as a Violation when a transaction is open in its
// scope. Multiple calls accumulate; every registered checker runs.
func WithStatementChecker(cs ...StatementChecker) Option {
	return func(d *Detector) { d.stmtCheckers = append(d.stmtCheckers, cs...) }
}

// WithUnscopedTxDetection makes the detector notify reporters that implement
// UnscopedTxReporter about transactions begun with no scope on their context
// (detached goroutines, db.Begin() without ctx, missing middleware). Such
// transactions are invisible to every checkpoint, so this surfaces the holes
// in the detection net — never as a Violation.
func WithUnscopedTxDetection() Option {
	return func(d *Detector) { d.reportUnscoped = true }
}

// WithUnscopedCheckDetection makes the detector notify reporters that
// implement UnscopedCheckReporter when a checkpoint (Check / Do / a wrapped
// RoundTripper) runs with no scope on its context. Such a checkpoint can
// consult no counter and is otherwise silent (§4.5), so a mis-wired
// middleware would disable detection unnoticed; this surfaces that hole from
// the checkpoint side, the analog of WithUnscopedTxDetection on the
// transaction side. Never a Violation.
func WithUnscopedCheckDetection() Option {
	return func(d *Detector) { d.reportUnscopedCheck = true }
}

// WithNestedScopeDetection makes the detector notify reporters that implement
// NestedScopeReporter whenever a scope is started on a context that already
// carries one. The shadow semantics are unchanged — transactions and checks
// still attribute to the inner scope only — this option merely makes the
// nesting itself observable, so accidental double instrumentation (e.g.
// middleware at two layers) does not go unnoticed.
func WithNestedScopeDetection() Option {
	return func(d *Detector) { d.reportNested = true }
}

// WithLeakedTxDetection makes the detector notify reporters that implement
// LeakedTxReporter when a scope finishes while transactions opened under it
// are still open (forgotten Commit/Rollback, tx handed to a detached
// goroutine) — never as a Violation.
func WithLeakedTxDetection() Option {
	return func(d *Detector) { d.reportLeaked = true }
}

// WithStackDepth sets how many stack frames are captured per Violation (and
// per UnscopedTx). The default is 32; 0 disables capture entirely for hot
// production paths.
func WithStackDepth(n int) Option {
	return func(d *Detector) { d.stackDepth = n }
}

// New creates a Detector.
func New(opts ...Option) *Detector {
	d := &Detector{
		classify:   DefaultClassifier,
		stackDepth: 32,
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

func (d *Detector) nextTxID() uint64 { return d.txSeq.Add(1) }

type scopeCtxKey struct{}

// scope is the mutable holder StartScope installs on the context. It is
// reachable both from the driver wrapper (via the ctx passed to BeginTx) and
// from checkpoints (via their own ctx), because both contexts descend from
// the same scope context — that shared holder is the whole mechanism.
type scope struct {
	name  string
	attrs []ScopeAttr // immutable after StartScope returns

	// openTxs counts transactions currently open under this scope. It is
	// atomic because checkpoints (any goroutine sharing the ctx) race
	// begin/commit on the driver side; the verdict is best-effort by design.
	openTxs  atomic.Int64
	finished atomic.Bool
}

func scopeFrom(ctx context.Context) *scope {
	s, _ := ctx.Value(scopeCtxKey{}).(*scope)
	return s
}

// ScopeOption configures a single scope at StartScope / InScope.
type ScopeOption func(*scope)

// StartScope marks the beginning of a logical scope (a use case, a request
// handler, a job) on the context. Every transaction begun through a wrapped
// driver with a context descending from the returned one is attributed to
// this scope, and every Check with such a context consults its counter.
//
// The scope name is half of the violation identity (Scope, Op): keep names a
// small, code-defined set (route patterns, use-case names), never raw URLs
// with IDs.
//
// The returned finish function ends the scope; call it exactly when the scope
// ends (typically via defer); it is idempotent. Starting a scope on a context
// that already carries one shadows the outer scope for contexts descending
// from the new one (reported when WithNestedScopeDetection is on).
func (d *Detector) StartScope(ctx context.Context, name string, opts ...ScopeOption) (context.Context, func()) {
	if outer := scopeFrom(ctx); outer != nil && d.reportNested {
		n := NestedScope{Outer: outer.name, Inner: name, Time: time.Now()}
		for _, r := range d.reporters {
			if nr, ok := r.(NestedScopeReporter); ok {
				nr.ReportNestedScope(ctx, n)
			}
		}
	}
	s := &scope{name: name}
	if d.attrsFunc != nil {
		// Evaluated once per scope, before per-scope options so that
		// WithScopeAttrs entries come after detector-level ones.
		s.attrs = append(s.attrs, d.attrsFunc(ctx)...)
	}
	for _, o := range opts {
		o(s)
	}
	sctx := context.WithValue(ctx, scopeCtxKey{}, s)
	return sctx, func() { d.finishScope(sctx, s) }
}

// InScope runs f inside a scope and finishes it when f returns.
func (d *Detector) InScope(ctx context.Context, name string, f func(context.Context) error, opts ...ScopeOption) error {
	ctx, finish := d.StartScope(ctx, name, opts...)
	defer finish()
	return f(ctx)
}

func (d *Detector) finishScope(ctx context.Context, s *scope) {
	if !s.finished.CompareAndSwap(false, true) {
		return
	}
	if !d.reportLeaked {
		return
	}
	if open := s.openTxs.Load(); open > 0 {
		l := LeakedTx{Scope: s.name, OpenTxs: int(open), Time: time.Now()}
		for _, r := range d.reporters {
			if lr, ok := r.(LeakedTxReporter); ok {
				lr.ReportLeakedTx(ctx, l)
			}
		}
	}
}

// reportUnscopedTx notifies reporters about a transaction begun with no scope
// on its context (requires WithUnscopedTxDetection).
func (d *Detector) reportUnscopedTx(ctx context.Context) {
	if !d.reportUnscoped {
		return
	}
	u := UnscopedTx{Stack: captureStack(d.stackDepth), Time: time.Now()}
	for _, r := range d.reporters {
		if ur, ok := r.(UnscopedTxReporter); ok {
			ur.ReportUnscopedTx(ctx, u)
		}
	}
}
