package txnpure

import (
	"context"
	"time"
)

// Op identifies an instrumented side-effect operation. Together with the
// scope name it forms the violation identity (Scope, Op) that keys the
// Allowlist, the Baseline, and throttling — so Name must stay a small,
// code-defined set: route patterns and hosts, never raw URLs with IDs.
type Op struct {
	// Kind is the coarse category: "http", "enqueue", "mail", "grpc", ...
	Kind string
	// Name is a stable identifier within the kind:
	// "GET api.example.com", "sqs:SendMessage".
	Name string
}

// CheckOption configures a single Check / Do call.
type CheckOption func(*checkConfig)

type checkConfig struct {
	allowed     bool
	allowReason string
	allowCounts []int
	attrs       []ScopeAttr
}

// AllowInTransaction marks the checkpoint as intentionally allowed to run
// inside a transaction, suppressing its Violation at the call site — the
// in-code alternative to a central Allowlist entry. The reason should say why
// and reference a ticket.
//
// Optional exactCalls pin the exact number of in-transaction calls of this
// (scope, op) identity the allow covers per scope execution, so a reviewed
// exemption cannot silently grow when the code starts calling more often (a
// loop, a retry, a second call site sharing the op): calls beyond the
// largest declared count are reported as Violations carrying AllowedCalls,
// and a scope that finishes with a total not among the declared counts
// reports a StaleAllow. Several counts may be listed for a path whose
// legitimate call count varies. Counts below 1 can never cover a call and
// are permanently stale. Omitting them keeps the unconditional behavior: any
// number of in-transaction calls is suppressed.
//
// Rot prevention works per execution instead of per entry: when the allowed
// check runs outside any transaction (the allow suppressed nothing),
// reporters that implement StaleAllowReporter are notified — exact in
// deterministic tests, a hint in production.
func AllowInTransaction(reason string, exactCalls ...int) CheckOption {
	counts := append([]int(nil), exactCalls...)
	return func(c *checkConfig) {
		c.allowed = true
		c.allowReason = reason
		c.allowCounts = counts
	}
}

// Check reports a Violation to the configured Reporters if the scope
// reachable from ctx has one or more open transactions — immediately, with a
// stack trace. The side effect itself is never blocked; txnpure observes and
// reports, tests assert.
//
// A Check whose ctx carries no scope cannot consult any counter and is
// silent (see WithUnscopedTxDetection for the transaction-side analog of
// that hole).
func (d *Detector) Check(ctx context.Context, op Op, opts ...CheckOption) {
	d.check(ctx, op, opts)
}

// Do checks and then runs f — the generic wrapper for side-effect call sites
// that want one-line instrumentation:
//
//	err := detector.Do(ctx, txnpure.Op{Kind: "enqueue", Name: "sqs:SendMessage"}, func(ctx context.Context) error {
//		return queue.Send(ctx, msg)
//	})
//
// The check never blocks f; f runs regardless of the verdict.
func (d *Detector) Do(ctx context.Context, op Op, f func(context.Context) error, opts ...CheckOption) error {
	d.check(ctx, op, opts)
	return f(ctx)
}

func (d *Detector) check(ctx context.Context, op Op, opts []CheckOption) {
	s, mark := scopeAndMarkFrom(ctx)
	if s == nil {
		if d.reportUnscopedCheck {
			u := UnscopedCheck{Op: op, Stack: captureStack(d.stackDepth), Time: time.Now()}
			for _, r := range d.reporters {
				if ur, ok := r.(UnscopedCheckReporter); ok {
					ur.ReportUnscopedCheck(ctx, u)
				}
			}
		}
		return
	}
	var cfg checkConfig
	if len(opts) != 0 {
		cfg = appliedCheckConfig(opts)
	}
	open := int(s.openTxs.Load())
	if open <= 0 {
		// Every in-code allow present here suppressed nothing (§4.13/4.14
		// uniform rule): report each as stale, per execution.
		if cfg.allowed {
			d.reportStaleAllow(ctx, StaleAllow{Scope: s.name, Op: op, Reason: cfg.allowReason})
		}
		if mark != nil {
			d.reportStaleAllow(ctx, StaleAllow{Scope: s.name, Op: op, Reason: mark.reason})
		}
		return
	}
	d.decideAndReport(ctx, s, mark, op, open, cfg)
}

// decideAndReport resolves the allow tiers for one in-transaction detection
// event — AllowInTransaction (call site) → AllowInTransactionHere (ctx
// region) → Allowlist — and reports a Violation when none covers it; the
// Baseline filters last, as a wrapper reporter. Exact-call declarations that
// decline (§4.13) fall through to the next tier and are carried on the
// Violation as AllowedCalls when nothing covers the call.
func (d *Detector) decideAndReport(ctx context.Context, s *scope, mark *allowMark, op Op, open int, cfg checkConfig) {
	key := ViolationKey{Scope: s.name, Kind: op.Kind, Name: op.Name}
	k := s.bumpCall(key)
	var declined []int
	if cfg.allowed {
		s.noteDeclared(key, cfg.allowCounts, cfg.allowReason)
		if coversCalls(cfg.allowCounts, k) {
			return
		}
		declined = append(declined, cfg.allowCounts...)
	}
	if mark != nil {
		// The region pins its own event total, not the per-identity one: a
		// counted AllowInTransactionHere declares how many in-transaction
		// side effects the region performs, whatever their ops.
		mk := int(mark.n.Add(1))
		if coversCalls(mark.counts, mk) {
			return
		}
		declined = append(declined, mark.counts...)
	}
	if d.allowlist != nil {
		covered, counts, reason, present := d.allowlist.decide(s.name, op, k)
		if present {
			s.noteDeclared(key, counts, reason)
			if covered {
				return
			}
			declined = append(declined, counts...)
		}
	}
	d.emitViolation(ctx, s, op, open, k, declined, cfg.attrs)
}

// appliedCheckConfig folds opts into a config. Kept out of check so that the
// zero-option path never heap-allocates the config: handing &cfg to the
// option closures forces cfg to escape, and check runs on every checkpoint.
func appliedCheckConfig(opts []CheckOption) checkConfig {
	var cfg checkConfig
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}

// checkNeeded reports whether an option-less checkpoint on ctx could produce
// any report, so adapters (WrapRoundTripper) can skip deriving the Op name on
// the per-request fast path. It mirrors check(): with no scope only
// unscoped-check detection would report; a scope with no open transaction
// reports nothing unless an in-code allow is in play (StaleAllow needs it) —
// an AllowInTransactionHere mark on the ctx, or options, which this must not
// gate.
func (d *Detector) checkNeeded(ctx context.Context) bool {
	if s, mark := scopeAndMarkFrom(ctx); s != nil {
		return s.openTxs.Load() > 0 || mark != nil
	}
	return d.reportUnscopedCheck
}

// emitViolation assembles a Violation (attrs merge, stack capture) and hands
// it to every reporter. Shared by the checkpoint path (Check/Do/RoundTripper),
// the cross-connection-write path and the statement-checker path — always via
// decideAndReport, which resolves the allow tiers first; the Baseline filters
// later as a wrapper reporter.
func (d *Detector) emitViolation(ctx context.Context, s *scope, op Op, open, call int, allowedCalls []int, checkAttrs []ScopeAttr) {
	attrs := make([]ScopeAttr, 0, len(s.attrs)+len(checkAttrs))
	attrs = append(attrs, s.attrs...)
	attrs = append(attrs, checkAttrs...)
	v := Violation{
		Op:           op,
		Scope:        s.name,
		OpenTxs:      open,
		Call:         call,
		AllowedCalls: allowedCalls,
		Stack:        captureStack(d.stackDepth),
		Attrs:        attrs,
		Time:         time.Now(),
	}
	for _, r := range d.reporters {
		r.Report(ctx, v)
	}
}

// StatementChecker inspects a statement executed through a wrapped driver and,
// when it represents an external call (a side effect the surrounding
// transaction cannot roll back — an HTTP-calling stored procedure, a
// notification publish, a foreign-data-wrapper call, ...), returns the Op to
// report and true. Registered with WithStatementChecker, it is the pluggable
// way to declare your own external calls in addition to the built-in
// detection (HTTP via WrapRoundTripper, cross-connection writes).
//
// Unlike cross-connection writes, a matched statement is checked against
// *every* open transaction in the scope, including the one on its own
// connection: the external effect embedded in the statement is not
// rollback-safe even when it runs inside its own transaction.
//
// Matchers run on the statement hot path (only while a transaction is open in
// the scope), so keep them cheap — a leading-keyword or substring test, not a
// full SQL parse. A checker must be a pure function of the query: for
// prepared statements it is evaluated once at prepare time and the match is
// reused for every execution.
type StatementChecker func(query string) (Op, bool)

// runStatementCheckers reports a Violation for each registered checker that
// recognizes query as an external call, when a transaction is open in the ctx
// scope. Callers guard with len(d.stmtCheckers) != 0. With no transaction
// open the checkers normally do not even run; an AllowInTransactionHere mark
// on the ctx is the exception — a marked match running outside any
// transaction is a stale allow (§4.14), and the extra work is gated on the
// mark being present.
func (d *Detector) runStatementCheckers(ctx context.Context, query string) {
	s, mark := scopeAndMarkFrom(ctx)
	if s == nil {
		return
	}
	open := int(s.openTxs.Load())
	if open <= 0 {
		if mark == nil {
			return
		}
		for _, chk := range d.stmtCheckers {
			if op, ok := chk(query); ok {
				d.reportStaleAllow(ctx, StaleAllow{Scope: s.name, Op: op, Reason: mark.reason})
			}
		}
		return
	}
	for _, chk := range d.stmtCheckers {
		if op, ok := chk(query); ok {
			d.decideAndReport(ctx, s, mark, op, open, checkConfig{})
		}
	}
}

// reportStatementOps is runStatementCheckers for the prepared-statement path:
// the checker matches were computed once at prepare time (checkers are pure
// functions of the query), so each execution only consults the scope counter
// and reports the pre-matched ops. Callers guard with len(ops) != 0.
func (d *Detector) reportStatementOps(ctx context.Context, ops []Op) {
	s, mark := scopeAndMarkFrom(ctx)
	if s == nil {
		return
	}
	open := int(s.openTxs.Load())
	if open <= 0 {
		if mark == nil {
			return
		}
		for _, op := range ops {
			d.reportStaleAllow(ctx, StaleAllow{Scope: s.name, Op: op, Reason: mark.reason})
		}
		return
	}
	for _, op := range ops {
		d.decideAndReport(ctx, s, mark, op, open, checkConfig{})
	}
}
