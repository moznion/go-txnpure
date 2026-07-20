package txnpure_test

import (
	"context"
	"database/sql"
	"testing"

	txnpure "github.com/moznion/go-txnpure"
)

// A write issued on a different connection than the one holding the open
// transaction, in the same scope, is a violation: the write cannot be rolled
// back with that transaction.
func TestCrossConnWriteSameDBViolates(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	txConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txConn.Close() }()
	tx, err := txConn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	// A second, independent connection issues a write while tx is open.
	otherConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = otherConn.Close() }()
	if _, err := otherConn.ExecContext(ctx, "INSERT INTO audit_log (msg) VALUES ('x')"); err != nil {
		t.Fatal(err)
	}

	vs := rep.Violations()
	if len(vs) != 1 {
		t.Fatalf("got %d violations, want 1", len(vs))
	}
	v := vs[0]
	if v.Op.Kind != "db" || v.Op.Name != "audit_log" {
		t.Errorf("Op = %+v, want {db audit_log}", v.Op)
	}
	if v.Scope != "CreateUser" {
		t.Errorf("Scope = %q, want CreateUser", v.Scope)
	}
	if v.OpenTxs != 1 {
		t.Errorf("OpenTxs = %d, want 1 (one other connection's tx)", v.OpenTxs)
	}
}

// The paradigmatic multi-DB case: DB A's tx open, a destructive write to DB B.
func TestCrossConnWriteAcrossTwoDBsViolates(t *testing.T) {
	rep := txnpure.NewCollectingReporter()
	det := txnpure.New(txnpure.WithReporter(rep))
	dbA := det.NewNullDB()
	dbB := det.NewNullDB()
	t.Cleanup(func() { _ = dbA.Close(); _ = dbB.Close() })

	ctx, finish := det.StartScope(context.Background(), "Checkout")
	defer finish()

	tx, err := dbA.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	if _, err := dbB.ExecContext(ctx, "UPDATE inventory SET qty = qty - 1"); err != nil {
		t.Fatal(err)
	}

	vs := rep.Violations()
	if len(vs) != 1 || vs[0].Op.Name != "inventory" {
		t.Fatalf("violations = %+v, want one write to inventory", vs)
	}
}

// A write inside the same connection's own transaction is normal — no other
// connection's transaction is open.
func TestWriteInsideOwnTxIsClean(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	if _, err := tx.ExecContext(ctx, "INSERT INTO users (name) VALUES ('a')"); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)
}

// An autocommit write with no other transaction open is clean.
func TestAutocommitWriteNoTxIsClean(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	if _, err := db.ExecContext(ctx, "INSERT INTO users (name) VALUES ('a')"); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)
}

// A transaction open in a different scope must not make another scope's write
// a violation (connection pooling isolation).
func TestCrossConnWriteIsScopeIsolated(t *testing.T) {
	det, rep, db := setup(t)
	ctxA, finishA := det.StartScope(context.Background(), "ScopeA")
	defer finishA()
	ctxB, finishB := det.StartScope(context.Background(), "ScopeB")
	defer finishB()

	tx, err := db.BeginTx(ctxA, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	if _, err := db.ExecContext(ctxB, "INSERT INTO users (name) VALUES ('a')"); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)
}

// A cross-connection write with no scope on its context is silent.
func TestCrossConnWriteNoScopeIsSilent(t *testing.T) {
	_, rep, db := setup(t)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	if _, err := db.ExecContext(context.Background(), "INSERT INTO users (name) VALUES ('a')"); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)
}

// A read on another connection during an open transaction is not a violation:
// only writes are hazardous (a read has nothing to roll back).
func TestCrossConnReadIsClean(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	rows, err := db.QueryContext(ctx, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	rep.RequireNoViolations(t)
}

// Cross-connection writes are suppressible through the Allowlist, keyed on
// (scope, {Kind: "db", Name: table}).
func TestCrossConnWriteAllowlisted(t *testing.T) {
	al := txnpure.NewAllowlist().Add("CreateUser", txnpure.Op{Kind: "db", Name: "audit_log"}, "best-effort audit; TICKET-99")
	det, rep, db := setup(t, txnpure.WithAllowlist(al))
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	other, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if _, err := other.ExecContext(ctx, "INSERT INTO audit_log (msg) VALUES ('x')"); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)
	if unused := al.UnusedEntries(); len(unused) != 0 {
		t.Errorf("UnusedEntries = %+v, want empty", unused)
	}
}

// Two other connections holding transactions → OpenTxs reflects both.
func TestCrossConnWriteCountsMultipleForeignTxs(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx1 := beginOnNewConn(t, db, ctx)
	defer tx1()
	tx2 := beginOnNewConn(t, db, ctx)
	defer tx2()

	if _, err := db.ExecContext(ctx, "DELETE FROM sessions WHERE expired"); err != nil {
		t.Fatal(err)
	}
	vs := rep.Violations()
	if len(vs) != 1 || vs[0].OpenTxs != 2 || vs[0].Op.Name != "sessions" {
		t.Fatalf("violations = %+v, want one DELETE on sessions with OpenTxs=2", vs)
	}
}

func beginOnNewConn(t *testing.T, db *sql.DB, ctx context.Context) func() {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		_ = tx.Commit()
		_ = conn.Close()
	}
}
