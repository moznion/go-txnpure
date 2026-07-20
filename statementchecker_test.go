package txnpure_test

import (
	"context"
	"strings"
	"testing"

	txnpure "github.com/moznion/go-txnpure"
)

// notifyChecker treats "SELECT pg_notify(...)" (a stand-in for a
// side-effecting statement) as an external call.
func notifyChecker(query string) (txnpure.Op, bool) {
	if strings.Contains(strings.ToLower(query), "pg_notify") {
		return txnpure.Op{Kind: "notify", Name: "pg_notify"}, true
	}
	return txnpure.Op{}, false
}

// A user-declared external-call statement fires while its OWN transaction is
// open — the embedded side effect is not rollback-safe even inside its tx.
func TestStatementCheckerInsideOwnTx(t *testing.T) {
	det, rep, db := setup(t, txnpure.WithStatementChecker(notifyChecker))
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	if _, err := tx.ExecContext(ctx, "SELECT pg_notify('ch', 'msg')"); err != nil {
		t.Fatal(err)
	}
	vs := rep.Violations()
	if len(vs) != 1 {
		t.Fatalf("got %d violations, want 1", len(vs))
	}
	if vs[0].Op != (txnpure.Op{Kind: "notify", Name: "pg_notify"}) {
		t.Errorf("Op = %+v, want {notify pg_notify}", vs[0].Op)
	}
	if vs[0].OpenTxs != 1 {
		t.Errorf("OpenTxs = %d, want 1 (its own open tx counts)", vs[0].OpenTxs)
	}
}

// Outside any transaction, a matched statement is clean.
func TestStatementCheckerNoTxIsClean(t *testing.T) {
	det, rep, db := setup(t, txnpure.WithStatementChecker(notifyChecker))
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	if _, err := db.ExecContext(ctx, "SELECT pg_notify('ch', 'msg')"); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)
}

// A statement not matched by any checker is clean.
func TestStatementCheckerNonMatchIsClean(t *testing.T) {
	det, rep, db := setup(t, txnpure.WithStatementChecker(notifyChecker))
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	if _, err := tx.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)
}

// Checkers accumulate across calls, and every matching one fires.
func TestStatementCheckerMultiple(t *testing.T) {
	dblink := func(q string) (txnpure.Op, bool) {
		if strings.Contains(strings.ToLower(q), "dblink") {
			return txnpure.Op{Kind: "fdw", Name: "dblink"}, true
		}
		return txnpure.Op{}, false
	}
	det, rep, db := setup(t,
		txnpure.WithStatementChecker(notifyChecker),
		txnpure.WithStatementChecker(dblink),
	)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	if _, err := tx.ExecContext(ctx, "SELECT pg_notify('c','m'), dblink_exec('...')"); err != nil {
		t.Fatal(err)
	}
	vs := rep.Violations()
	if len(vs) != 2 {
		t.Fatalf("got %d violations, want 2 (both checkers match)", len(vs))
	}
}

// A matched statement is suppressible through the Allowlist on its Op.
func TestStatementCheckerAllowlisted(t *testing.T) {
	al := txnpure.NewAllowlist().Add("CreateUser", txnpure.Op{Kind: "notify", Name: "pg_notify"}, "transactional notify is fine here; TICKET-7")
	det, rep, db := setup(t,
		txnpure.WithStatementChecker(notifyChecker),
		txnpure.WithAllowlist(al),
	)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	if _, err := tx.ExecContext(ctx, "SELECT pg_notify('ch', 'msg')"); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)
	if unused := al.UnusedEntries(); len(unused) != 0 {
		t.Errorf("UnusedEntries = %+v, want empty", unused)
	}
}

// With no checkers registered, the mechanism adds nothing.
func TestNoStatementCheckersIsInert(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	if _, err := tx.ExecContext(ctx, "SELECT pg_notify('ch', 'msg')"); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)
}
