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
func (a *Allowlist) Add(scope string, op Op, reason string) *Allowlist {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries[ViolationKey{Scope: scope, Kind: op.Kind, Name: op.Name}] = &allowlistEntry{reason: reason}
	return a
}

// allow reports whether the (scope, op) pair is allowlisted — exact scope
// first, then the AnyScope wildcard — marking the matched entry used.
func (a *Allowlist) allow(scope string, op Op) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.entries[ViolationKey{Scope: scope, Kind: op.Kind, Name: op.Name}]; ok {
		e.used = true
		return true
	}
	if e, ok := a.entries[ViolationKey{Scope: AnyScope, Kind: op.Kind, Name: op.Name}]; ok {
		e.used = true
		return true
	}
	return false
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
