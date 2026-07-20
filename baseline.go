package txnpure

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// baselineFileComment is written into every saved baseline file so human
// readers know what the file is and how it is meant to evolve.
const baselineFileComment = "txnpure baseline: (scope, op) pairs that already ran side effects inside transactions when txnpure was adopted. Their violations are tolerated until fixed; new violations still fail. Remove entries as call sites get fixed (the ratchet only goes down); regenerate deliberately with Baseline.Save, never automatically."

// baselineFile is the on-disk JSON representation of a Baseline.
type baselineFile struct {
	Comment string          `json:"comment"`
	Entries []baselineEntry `json:"entries"`
}

type baselineEntry struct {
	Scope string `json:"scope"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
}

// Baseline is the ratchet helper for adopting txnpure on an existing
// codebase: capture the current violations once (BaselineFromViolations +
// Save), commit the file, and from then on only new violations fail —
// baselined (scope, op) pairs are tolerated until fixed.
//
// Entries are keyed on (scope, kind, name) alone. Open-transaction counts,
// stacks, and timestamps vary by run, so they would make the baseline
// unstable; the identity pair is the stable key.
//
// To keep the ratchet going down, every entry tracks whether it actually
// suppressed a violation; check UnusedEntries in CI and fail when an entry no
// longer matches anything — the same discipline as Allowlist.UnusedEntries.
type Baseline struct {
	mu      sync.Mutex
	entries map[ViolationKey]bool // key -> suppressed a violation
}

// NewBaseline creates an empty Baseline.
func NewBaseline() *Baseline {
	return &Baseline{entries: map[ViolationKey]bool{}}
}

// BaselineFromViolations builds a Baseline from the identities of the given
// violations (typically CollectingReporter.Violations after a full run
// without any baseline installed). Duplicate identities collapse into one
// entry.
func BaselineFromViolations(vs []Violation) *Baseline {
	b := NewBaseline()
	for _, v := range vs {
		b.entries[v.key()] = false
	}
	return b
}

// Add registers a (scope, op) pair in the baseline. Returns the Baseline for
// chaining. Prefer BaselineFromViolations + Save for the normal adoption
// flow; Add exists for programmatic construction.
func (b *Baseline) Add(scope string, op Op) *Baseline {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := ViolationKey{Scope: scope, Kind: op.Kind, Name: op.Name}
	if _, ok := b.entries[k]; !ok {
		b.entries[k] = false
	}
	return b
}

// Entries returns the baselined keys, sorted by scope, kind, name.
func (b *Baseline) Entries() []ViolationKey {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]ViolationKey, 0, len(b.entries))
	for k := range b.entries {
		out = append(out, k)
	}
	sortKeys(out)
	return out
}

// covers reports whether the key is baselined, marking the entry used.
func (b *Baseline) covers(k ViolationKey) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.entries[k]; !ok {
		return false
	}
	b.entries[k] = true
	return true
}

// UnusedEntries returns the baselined keys that never suppressed a violation,
// sorted. A non-empty result in CI means those call sites are fixed: remove
// their entries from the baseline file so the ratchet keeps going down.
func (b *Baseline) UnusedEntries() []ViolationKey {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []ViolationKey
	for k, used := range b.entries {
		if !used {
			out = append(out, k)
		}
	}
	sortKeys(out)
	return out
}

// Save writes the baseline to path as deterministic, human-readable JSON:
// indented, entries sorted, with a comment field explaining the file, so
// diffs stay clean. Call it deliberately — on first adoption and on
// intentional regeneration — never on every run.
func (b *Baseline) Save(path string) error {
	keys := b.Entries()
	entries := make([]baselineEntry, len(keys))
	for i, k := range keys {
		entries[i] = baselineEntry(k)
	}
	f := baselineFile{
		Comment: baselineFileComment,
		Entries: entries,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("txnpure: marshal baseline: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("txnpure: write baseline file: %w", err)
	}
	return nil
}

// LoadBaseline reads a baseline file written by Save. A missing file is an
// error (check with errors.Is against fs.ErrNotExist): creating the baseline
// must stay a deliberate Save call, not a silent fallback.
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("txnpure: read baseline file: %w", err)
	}
	var f baselineFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("txnpure: parse baseline file %s: %w", path, err)
	}
	b := NewBaseline()
	for _, e := range f.Entries {
		b.entries[ViolationKey(e)] = false
	}
	return b, nil
}

// BaselineReporter filters violations through a Baseline before forwarding
// them to the wrapped Reporter: violations of baselined (scope, op) pairs are
// swallowed (marking the entry used), everything else passes through. The
// optional signals (unscoped/nested/leaked/stale-allow) are never baselined
// and are forwarded unchanged when the wrapped Reporter implements the
// corresponding interfaces.
type BaselineReporter struct {
	baseline *Baseline
	next     Reporter
}

var (
	_ Reporter              = (*BaselineReporter)(nil)
	_ UnscopedTxReporter    = (*BaselineReporter)(nil)
	_ UnscopedCheckReporter = (*BaselineReporter)(nil)
	_ NestedScopeReporter   = (*BaselineReporter)(nil)
	_ LeakedTxReporter      = (*BaselineReporter)(nil)
	_ StaleAllowReporter    = (*BaselineReporter)(nil)
)

// NewBaselineReporter wraps next so that violations of (scope, op) pairs in
// baseline are suppressed. A nil baseline suppresses nothing.
func NewBaselineReporter(baseline *Baseline, next Reporter) *BaselineReporter {
	return &BaselineReporter{baseline: baseline, next: next}
}

func (r *BaselineReporter) Report(ctx context.Context, v Violation) {
	if r.baseline != nil && r.baseline.covers(v.key()) {
		return
	}
	r.next.Report(ctx, v)
}

func (r *BaselineReporter) ReportUnscopedTx(ctx context.Context, u UnscopedTx) {
	if ur, ok := r.next.(UnscopedTxReporter); ok {
		ur.ReportUnscopedTx(ctx, u)
	}
}

func (r *BaselineReporter) ReportUnscopedCheck(ctx context.Context, u UnscopedCheck) {
	if ur, ok := r.next.(UnscopedCheckReporter); ok {
		ur.ReportUnscopedCheck(ctx, u)
	}
}

func (r *BaselineReporter) ReportNestedScope(ctx context.Context, n NestedScope) {
	if nr, ok := r.next.(NestedScopeReporter); ok {
		nr.ReportNestedScope(ctx, n)
	}
}

func (r *BaselineReporter) ReportLeakedTx(ctx context.Context, l LeakedTx) {
	if lr, ok := r.next.(LeakedTxReporter); ok {
		lr.ReportLeakedTx(ctx, l)
	}
}

func (r *BaselineReporter) ReportStaleAllow(ctx context.Context, s StaleAllow) {
	if sr, ok := r.next.(StaleAllowReporter); ok {
		sr.ReportStaleAllow(ctx, s)
	}
}
