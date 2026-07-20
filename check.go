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
	attrs       []ScopeAttr
}

// AllowInTransaction marks the checkpoint as intentionally allowed to run
// inside a transaction, suppressing its Violation at the call site — the
// in-code alternative to a central Allowlist entry. The reason should say why
// and reference a ticket.
//
// Rot prevention works per execution instead of per entry: when the allowed
// check runs outside any transaction (the allow suppressed nothing),
// reporters that implement StaleAllowReporter are notified — exact in
// deterministic tests, a hint in production.
func AllowInTransaction(reason string) CheckOption {
	return func(c *checkConfig) {
		c.allowed = true
		c.allowReason = reason
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
	s := scopeFrom(ctx)
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
	for _, o := range opts {
		o(&cfg)
	}
	open := int(s.openTxs.Load())
	if open <= 0 {
		if cfg.allowed {
			sa := StaleAllow{Scope: s.name, Op: op, Reason: cfg.allowReason}
			for _, r := range d.reporters {
				if sr, ok := r.(StaleAllowReporter); ok {
					sr.ReportStaleAllow(ctx, sa)
				}
			}
		}
		return
	}
	// Precedence: AllowInTransaction → Allowlist → baseline (a wrapper
	// reporter, so it filters last by construction).
	if cfg.allowed {
		return
	}
	d.emitViolation(ctx, s, op, open, cfg.attrs)
}

// emitViolation assembles a Violation (allowlist filter, attrs merge, stack
// capture) and hands it to every reporter. Shared by the checkpoint path
// (Check/Do/RoundTripper) and the cross-connection-write path; the Allowlist
// is consulted here, and the Baseline filters later as a wrapper reporter.
func (d *Detector) emitViolation(ctx context.Context, s *scope, op Op, open int, checkAttrs []ScopeAttr) {
	if d.allowlist != nil && d.allowlist.allow(s.name, op) {
		return
	}
	attrs := make([]ScopeAttr, 0, len(s.attrs)+len(checkAttrs))
	attrs = append(attrs, s.attrs...)
	attrs = append(attrs, checkAttrs...)
	v := Violation{
		Op:      op,
		Scope:   s.name,
		OpenTxs: open,
		Stack:   captureStack(d.stackDepth),
		Attrs:   attrs,
		Time:    time.Now(),
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
// full SQL parse.
type StatementChecker func(query string) (Op, bool)

// runStatementCheckers reports a Violation for each registered checker that
// recognizes query as an external call, when a transaction is open in the ctx
// scope.
func (d *Detector) runStatementCheckers(ctx context.Context, query string) {
	if len(d.stmtCheckers) == 0 {
		return
	}
	s := scopeFrom(ctx)
	if s == nil {
		return
	}
	open := int(s.openTxs.Load())
	if open <= 0 {
		return
	}
	for _, chk := range d.stmtCheckers {
		if op, ok := chk(query); ok {
			d.emitViolation(ctx, s, op, open, nil)
		}
	}
}
