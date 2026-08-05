package txnpure

import (
	"context"
	"sync/atomic"
)

// AllowInTransactionHere derives a context that marks the side effects
// executed with it (or with a context descending from it) as intentionally
// allowed to run inside a transaction — the at-the-effect-site alternative to
// a Check-site AllowInTransaction option or a central Allowlist entry, and
// the only in-code form that reaches the detection points with no call site
// to annotate: cross-connection writes, statement-checker matches, and
// wrapped RoundTrippers (via req.WithContext).
//
//	actx := txnpure.AllowInTransactionHere(ctx, "cache eviction is best-effort (TICKET-99)")
//	_, err := cacheDB.ExecContext(actx, "DELETE FROM order_cache WHERE id = $1", id)
//
// The allow is a lexical region, not a transaction property: it covers
// exactly the calls made under the derived context — whenever they run while
// a transaction is open in the scope, regardless of whether that transaction
// began before or after the mark — and it expires with the region. It
// suppresses every op kind, so keep the region tight: derive the context for
// one call and keep using the original everywhere else. An inner
// AllowInTransactionHere shadows an outer one for contexts descending from
// it, and a nested StartScope does not inherit the mark. With no scope on
// ctx it is a no-op returning ctx unchanged (WithUnscopedCheckDetection /
// WithUnscopedTxDetection surface such mis-wiring).
//
// Optional exactCalls pin the exact number of in-transaction side effects
// the region performs per scope execution (the same contract as
// AllowInTransaction): calls beyond the largest declared count are reported
// as Violations carrying AllowedCalls, and a scope finishing with a region
// total not among the declared counts reports a StaleAllow. A marked call
// running while no transaction is open reports a StaleAllow immediately,
// like every other allow form.
func AllowInTransactionHere(ctx context.Context, reason string, exactCalls ...int) context.Context {
	s, _ := scopeAndMarkFrom(ctx)
	if s == nil {
		return ctx
	}
	m := &allowMark{reason: reason}
	if len(exactCalls) != 0 {
		m.counts = append([]int(nil), exactCalls...)
		s.registerMark(m)
	}
	return context.WithValue(ctx, scopeCtxKey{}, &allowedScope{s: s, mark: m})
}

// allowMark is the in-context allow region installed by
// AllowInTransactionHere. Immutable after creation except for the atomic
// event counter, so it needs no locking — concurrency safety comes from
// context immutability (unlike a mark on shared mutable state would).
type allowMark struct {
	reason string
	counts []int // exact in-transaction call counts covered; empty = any

	// n counts the in-transaction detection events consulted under this
	// region — the quantity the declared counts pin (§4.14 of DESIGN.md).
	n atomic.Int64
}

// allowedScope is the context value installed by AllowInTransactionHere: the
// same scope holder plus the innermost allow region. It is stored under
// scopeCtxKey so no detection path pays a second ctx.Value walk.
type allowedScope struct {
	s    *scope
	mark *allowMark
}

// scopeAndMarkFrom resolves the scope holder and the innermost allow region
// from ctx in a single context lookup.
func scopeAndMarkFrom(ctx context.Context) (*scope, *allowMark) {
	switch v := ctx.Value(scopeCtxKey{}).(type) {
	case *scope:
		return v, nil
	case *allowedScope:
		return v.s, v.mark
	}
	return nil, nil
}

// callCount tracks, per scope execution, how many in-transaction detection
// events one violation identity produced, plus the exact-call declaration
// last consulted for it (§4.13 of DESIGN.md). Guarded by scope.callMu.
type callCount struct {
	n        int
	declared []int
	reason   string
}

// bumpCall counts one in-transaction detection event for the identity and
// returns its 1-based index within this scope execution. It runs only on the
// hazard path (a transaction is open), never on the always-on fast paths;
// the map is allocated lazily so scopes that see no in-transaction event pay
// nothing.
func (s *scope) bumpCall(key ViolationKey) int {
	s.callMu.Lock()
	defer s.callMu.Unlock()
	c := s.calls[key]
	if c == nil {
		if s.calls == nil {
			s.calls = make(map[ViolationKey]*callCount)
		}
		c = &callCount{}
		s.calls[key] = c
	}
	c.n++
	return c.n
}

// noteDeclared remembers the exact-call declaration consulted for the
// identity (last one wins), so finishScope can verify the final total
// against it. Called after bumpCall within the same event, so the entry
// exists.
func (s *scope) noteDeclared(key ViolationKey, counts []int, reason string) {
	if len(counts) == 0 {
		return
	}
	s.callMu.Lock()
	defer s.callMu.Unlock()
	if c := s.calls[key]; c != nil {
		c.declared = counts
		c.reason = reason
	}
}

// registerMark records a counted allow region on the scope so finishScope
// can verify its final total. Uncounted marks are not registered — they have
// nothing to verify at finish; their rot signal is the per-event StaleAllow.
func (s *scope) registerMark(m *allowMark) {
	s.callMu.Lock()
	defer s.callMu.Unlock()
	s.marks = append(s.marks, m)
}

// coversCalls reports whether an exact-call declaration covers the k-th
// in-transaction call: no declared counts cover any k; otherwise every call
// up to the largest declared count is suppressed, and exactness against the
// full declared set is verified when the scope finishes — an immediate
// verdict cannot know the final total. Counts below 1 can never cover a
// call; they are permanently stale rather than rejected.
func coversCalls(counts []int, k int) bool {
	if len(counts) == 0 {
		return true
	}
	return k <= maxCount(counts)
}

func maxCount(counts []int) int {
	m := 0
	for _, c := range counts {
		if c > m {
			m = c
		}
	}
	return m
}

func containsCount(counts []int, n int) bool {
	for _, c := range counts {
		if c == n {
			return true
		}
	}
	return false
}

// reportCallShortfalls fires a StaleAllow for every exact-call declaration
// the finishing scope did not live up to: a total below the declared counts,
// or between them. Totals above the declared maximum are not re-reported —
// the excess calls already surfaced as Violations. Identity declarations are
// reported in sorted order, then counted allow regions in creation order.
func (d *Detector) reportCallShortfalls(ctx context.Context, s *scope) {
	s.callMu.Lock()
	if len(s.calls) == 0 && len(s.marks) == 0 {
		// Nothing was counted; keep the empty-scope finish allocation-free.
		s.callMu.Unlock()
		return
	}
	var stale []StaleAllow
	keys := make([]ViolationKey, 0, len(s.calls))
	for key, c := range s.calls {
		if len(c.declared) != 0 {
			keys = append(keys, key)
		}
	}
	sortKeys(keys)
	for _, key := range keys {
		c := s.calls[key]
		if c.n <= maxCount(c.declared) && !containsCount(c.declared, c.n) {
			stale = append(stale, StaleAllow{
				Scope:        s.name,
				Op:           Op{Kind: key.Kind, Name: key.Name},
				Reason:       c.reason,
				Calls:        c.n,
				AllowedCalls: c.declared,
			})
		}
	}
	for _, m := range s.marks {
		n := int(m.n.Load())
		if n <= maxCount(m.counts) && !containsCount(m.counts, n) {
			stale = append(stale, StaleAllow{
				Scope:        s.name,
				Reason:       m.reason,
				Calls:        n,
				AllowedCalls: m.counts,
			})
		}
	}
	s.callMu.Unlock()
	for _, sa := range stale {
		d.reportStaleAllow(ctx, sa)
	}
}

// reportStaleAllow hands one StaleAllow to every reporter that opts in.
func (d *Detector) reportStaleAllow(ctx context.Context, sa StaleAllow) {
	for _, r := range d.reporters {
		if sr, ok := r.(StaleAllowReporter); ok {
			sr.ReportStaleAllow(ctx, sa)
		}
	}
}
