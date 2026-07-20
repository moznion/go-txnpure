package txnpure_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	txnpure "github.com/moznion/go-txnpure"
)

func TestAllowInTransactionSuppresses(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	det.Check(ctx, opHTTP, txnpure.AllowInTransaction("lock-service call must hold the row lock; TICKET-42"))
	rep.RequireNoViolations(t)
	rep.RequireNoStaleAllows(t) // it suppressed something → not stale
}

func TestStaleAllowReported(t *testing.T) {
	det, rep, _ := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	det.Check(ctx, opHTTP, txnpure.AllowInTransaction("TICKET-42"))
	rep.RequireNoViolations(t)
	sas := rep.StaleAllows()
	if len(sas) != 1 {
		t.Fatalf("got %d stale allows, want 1", len(sas))
	}
	if sas[0].Scope != "CreateUser" || sas[0].Op != opHTTP || sas[0].Reason != "TICKET-42" {
		t.Errorf("StaleAllow = %+v", sas[0])
	}
}

func TestStaleAllowSilentWithoutScope(t *testing.T) {
	det, rep, _ := setup(t)
	det.Check(context.Background(), opHTTP, txnpure.AllowInTransaction("TICKET-42"))
	rep.RequireNoStaleAllows(t)
	rep.RequireNoViolations(t)
}

func TestAllowlistExactMatch(t *testing.T) {
	al := txnpure.NewAllowlist().Add("CreateUser", opHTTP, "TICKET-42")
	det, rep, db := setup(t, txnpure.WithAllowlist(al))
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	det.Check(ctx, opHTTP)
	rep.RequireNoViolations(t)
	if unused := al.UnusedEntries(); len(unused) != 0 {
		t.Errorf("UnusedEntries = %+v, want empty after suppression", unused)
	}
}

// The same op in a different scope must still violate: identity is (Scope, Op).
func TestAllowlistIsScopeSpecific(t *testing.T) {
	al := txnpure.NewAllowlist().Add("CreateUser", opHTTP, "TICKET-42")
	det, rep, db := setup(t, txnpure.WithAllowlist(al))
	ctx, finish := det.StartScope(context.Background(), "DeleteUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	det.Check(ctx, opHTTP)
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("got %d violations, want 1 — allowlist entries must not leak across scopes", got)
	}
	unused := al.UnusedEntries()
	if len(unused) != 1 || unused[0].Scope != "CreateUser" {
		t.Errorf("UnusedEntries = %+v, want the CreateUser entry", unused)
	}
}

func TestAllowlistAnyScopeWildcard(t *testing.T) {
	al := txnpure.NewAllowlist().Add(txnpure.AnyScope, opHTTP, "bulk adoption; TICKET-43")
	det, rep, db := setup(t, txnpure.WithAllowlist(al))
	ctx, finish := det.StartScope(context.Background(), "AnythingGoes")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	det.Check(ctx, opHTTP)
	rep.RequireNoViolations(t)
	if unused := al.UnusedEntries(); len(unused) != 0 {
		t.Errorf("UnusedEntries = %+v, want empty", unused)
	}
}

// In-code AllowInTransaction wins before the Allowlist: the allowlist entry
// stays unused.
func TestAllowPrecedenceOverAllowlist(t *testing.T) {
	al := txnpure.NewAllowlist().Add("CreateUser", opHTTP, "TICKET-42")
	det, rep, db := setup(t, txnpure.WithAllowlist(al))
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	det.Check(ctx, opHTTP, txnpure.AllowInTransaction("call-site allow"))
	rep.RequireNoViolations(t)
	if unused := al.UnusedEntries(); len(unused) != 1 {
		t.Errorf("UnusedEntries = %+v, want the untouched allowlist entry", unused)
	}
}

func TestBaselineSaveLoadRoundtrip(t *testing.T) {
	b := txnpure.NewBaseline().
		Add("CreateUser", opHTTP).
		Add("DeleteUser", txnpure.Op{Kind: "enqueue", Name: "sqs:SendMessage"}).
		Add("CreateUser", opHTTP) // duplicate collapses

	path := filepath.Join(t.TempDir(), "txnpure-baseline.json")
	if err := b.Save(path); err != nil {
		t.Fatal(err)
	}
	data1, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Save(path); err != nil {
		t.Fatal(err)
	}
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data1) != string(data2) {
		t.Error("Save is not deterministic")
	}

	loaded, err := txnpure.LoadBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	want := b.Entries()
	got := loaded.Entries()
	if len(got) != len(want) {
		t.Fatalf("loaded %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLoadBaselineMissingFileErrors(t *testing.T) {
	_, err := txnpure.LoadBaseline(filepath.Join(t.TempDir(), "nope.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestBaselineReporterFiltersKnownDebt(t *testing.T) {
	rep := txnpure.NewCollectingReporter()
	b := txnpure.NewBaseline().Add("CreateUser", opHTTP)
	det := txnpure.New(txnpure.WithReporter(txnpure.NewBaselineReporter(b, rep)))
	db := det.NewNullDB()
	t.Cleanup(func() { _ = db.Close() })

	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	det.Check(ctx, opHTTP) // baselined → swallowed
	rep.RequireNoViolations(t)
	if unused := b.UnusedEntries(); len(unused) != 0 {
		t.Errorf("UnusedEntries = %+v, want empty after suppression", unused)
	}

	det.Check(ctx, txnpure.Op{Kind: "mail", Name: "smtp:Send"}) // new violation → passes
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("got %d violations, want 1 — only baselined identities are tolerated", got)
	}
}

func TestBaselineFromViolations(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()
	det.Check(ctx, opHTTP)
	det.Check(ctx, opHTTP) // duplicate identity

	b := txnpure.BaselineFromViolations(rep.Violations())
	entries := b.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want the single collapsed identity", entries)
	}
	if entries[0] != (txnpure.ViolationKey{Scope: "CreateUser", Kind: "http", Name: "GET api.example.com"}) {
		t.Errorf("entry = %+v", entries[0])
	}
	if unused := b.UnusedEntries(); len(unused) != 1 {
		t.Errorf("fresh baseline UnusedEntries = %+v, want 1", unused)
	}
}

// Every wrapper reporter must forward every optional interface — the class of
// bug txnproof's guidelines warn about.
func TestWrapperReportersForwardAllOptionalInterfaces(t *testing.T) {
	ctx := context.Background()
	wrappers := map[string]func(next txnpure.Reporter) txnpure.Reporter{
		"baseline":   func(next txnpure.Reporter) txnpure.Reporter { return txnpure.NewBaselineReporter(nil, next) },
		"throttling": func(next txnpure.Reporter) txnpure.Reporter { return txnpure.NewThrottlingReporter(next, 0) },
	}
	for name, wrap := range wrappers {
		t.Run(name, func(t *testing.T) {
			collect := txnpure.NewCollectingReporter()
			w := wrap(collect)

			w.Report(ctx, txnpure.Violation{Op: opHTTP, Scope: "S", OpenTxs: 1})
			if got := len(collect.Violations()); got != 1 {
				t.Errorf("Report not forwarded: got %d", got)
			}
			w.(txnpure.UnscopedTxReporter).ReportUnscopedTx(ctx, txnpure.UnscopedTx{})
			if got := len(collect.UnscopedTxs()); got != 1 {
				t.Errorf("ReportUnscopedTx not forwarded: got %d", got)
			}
			w.(txnpure.UnscopedCheckReporter).ReportUnscopedCheck(ctx, txnpure.UnscopedCheck{Op: opHTTP})
			if got := len(collect.UnscopedChecks()); got != 1 {
				t.Errorf("ReportUnscopedCheck not forwarded: got %d", got)
			}
			w.(txnpure.NestedScopeReporter).ReportNestedScope(ctx, txnpure.NestedScope{Outer: "O", Inner: "I"})
			if got := len(collect.NestedScopes()); got != 1 {
				t.Errorf("ReportNestedScope not forwarded: got %d", got)
			}
			w.(txnpure.LeakedTxReporter).ReportLeakedTx(ctx, txnpure.LeakedTx{Scope: "S", OpenTxs: 1})
			if got := len(collect.LeakedTxs()); got != 1 {
				t.Errorf("ReportLeakedTx not forwarded: got %d", got)
			}
			w.(txnpure.StaleAllowReporter).ReportStaleAllow(ctx, txnpure.StaleAllow{Scope: "S", Op: opHTTP})
			if got := len(collect.StaleAllows()); got != 1 {
				t.Errorf("ReportStaleAllow not forwarded: got %d", got)
			}

			// With a next that implements nothing optional, the wrapper must
			// neither panic nor fabricate.
			plain := txnpure.ReporterFunc(func(context.Context, txnpure.Violation) {})
			w2 := wrap(plain)
			w2.(txnpure.UnscopedTxReporter).ReportUnscopedTx(ctx, txnpure.UnscopedTx{})
			w2.(txnpure.UnscopedCheckReporter).ReportUnscopedCheck(ctx, txnpure.UnscopedCheck{})
			w2.(txnpure.NestedScopeReporter).ReportNestedScope(ctx, txnpure.NestedScope{})
			w2.(txnpure.LeakedTxReporter).ReportLeakedTx(ctx, txnpure.LeakedTx{})
			w2.(txnpure.StaleAllowReporter).ReportStaleAllow(ctx, txnpure.StaleAllow{})
		})
	}
}
