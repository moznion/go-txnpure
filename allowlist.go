package txnpure

import (
	"sort"
	"sync"
)

// AnyScope is the wildcard scope for Allowlist entries: the op is allowed in
// every scope. Supported for bulk adoption but discouraged long-term — the
// same operation can be legitimate in one use case and a bug in another,
// which is exactly why the scope is part of the violation identity.
const AnyScope = "*"

// Allowlist suppresses violations for (scope, op) pairs that are
// intentionally executed inside a transaction (e.g. a lock-service call that
// must happen while the row lock is held).
//
// To keep the list from rotting, every entry tracks whether it actually
// suppressed a violation; check UnusedEntries in CI and fail when an entry no
// longer matches anything (the same discipline as unused //nolint
// directives).
type Allowlist struct {
	mu      sync.Mutex
	entries map[ViolationKey]*allowlistEntry
}

type allowlistEntry struct {
	reason string
	counts []int // exact in-transaction call counts covered; empty = any
	used   bool
}

// NewAllowlist creates an empty Allowlist.
func NewAllowlist() *Allowlist {
	return &Allowlist{entries: map[ViolationKey]*allowlistEntry{}}
}

// Add registers a (scope, op) pair as intentionally allowed inside a
// transaction. scope may be AnyScope to allow the op in every scope. The
// reason should say why and reference a ticket. Returns the Allowlist for
// chaining.
//
// Optional exactCalls pin the exact number of in-transaction calls of the
// (scope, op) identity the entry covers per scope execution — the same
// contract as AllowInTransaction: further calls are reported as Violations
// carrying AllowedCalls, a scope finishing with a total not among the
// declared counts reports a StaleAllow, and counts below 1 can never cover
// a call. Omitting them keeps the unconditional behavior.
func (a *Allowlist) Add(scope string, op Op, reason string, exactCalls ...int) *Allowlist {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := &allowlistEntry{reason: reason}
	if len(exactCalls) != 0 {
		e.counts = append([]int(nil), exactCalls...)
	}
	a.entries[ViolationKey{Scope: scope, Kind: op.Kind, Name: op.Name}] = e
	return a
}

// decide resolves the entry for the (scope, op) pair — exact scope first,
// then the AnyScope wildcard — against the k-th in-transaction call of that
// identity. present reports whether an entry matched at all; covered whether
// it suppresses this call (marking the entry used). counts and reason echo
// the entry so a declining declaration can be carried on the Violation and
// verified at scope finish.
func (a *Allowlist) decide(scope string, op Op, k int) (covered bool, counts []int, reason string, present bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.entries[ViolationKey{Scope: scope, Kind: op.Kind, Name: op.Name}]
	if !ok {
		e, ok = a.entries[ViolationKey{Scope: AnyScope, Kind: op.Kind, Name: op.Name}]
	}
	if !ok {
		return false, nil, "", false
	}
	if coversCalls(e.counts, k) {
		e.used = true
		return true, e.counts, e.reason, true
	}
	return false, e.counts, e.reason, true
}

// UnusedEntries returns the keys that never suppressed a violation, sorted by
// scope, kind, name. A non-empty result in CI means the allowlist has stale
// entries that should be removed.
func (a *Allowlist) UnusedEntries() []ViolationKey {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []ViolationKey
	for k, e := range a.entries {
		if !e.used {
			out = append(out, k)
		}
	}
	sortKeys(out)
	return out
}

func sortKeys(keys []ViolationKey) {
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
}
