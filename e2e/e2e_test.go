// Package e2e self-verifies txnpure against a real PostgreSQL server: each
// scenario runs through a detector-wrapped pgx v5 driver and asserts the
// client-side checkpoint verdict.
//
// Unlike go-txnproof's e2e there is no server-log cross-check — the property
// under test is client-side timing (did a checkpoint fire while a transaction
// was open), which the server has no record of. The value here is exercising
// a real driver's Begin/Commit/Rollback, textual-transaction, savepoint,
// prepared-statement, and multi-connection paths, rather than the in-memory
// NullDB.
//
// The tests skip unless TXNPURE_E2E_PG_DSN is set to a libpq/pgx connection
// string. run.sh spins up a throwaway cluster on a unix socket and runs them.
package e2e

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	txnpure "github.com/moznion/go-txnpure"
)

var opHTTP = txnpure.Op{Kind: "http", Name: "GET api.example.com"}

func dsn(t *testing.T) string {
	t.Helper()
	d := os.Getenv("TXNPURE_E2E_PG_DSN")
	if d == "" {
		t.Skip("set TXNPURE_E2E_PG_DSN to run the e2e tests (see run.sh)")
	}
	return d
}

// newDB opens a real PostgreSQL through detector.WrapConnector and ensures the
// scratch tables exist.
func newDB(t *testing.T, opts ...txnpure.Option) (*txnpure.Detector, *txnpure.CollectingReporter, *sql.DB) {
	t.Helper()
	rep := txnpure.NewCollectingReporter()
	det := txnpure.New(append([]txnpure.Option{txnpure.WithReporter(rep)}, opts...)...)

	cfg, err := pgx.ParseConfig(dsn(t))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	db := sql.OpenDB(det.WrapConnector(stdlib.GetConnector(*cfg)))
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}
	// DDL runs with no scope in context, so it never trips a checkpoint.
	for _, ddl := range []string{
		"CREATE TABLE IF NOT EXISTS e2e_users (id bigserial PRIMARY KEY, v int NOT NULL)",
		"CREATE TABLE IF NOT EXISTS e2e_audit (id bigserial PRIMARY KEY, v int NOT NULL)",
	} {
		if _, err := db.ExecContext(context.Background(), ddl); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return det, rep, db
}

// A real driver-level transaction: a checkpoint fires while it is open, and is
// clean again after commit.
func TestDriverTxCheckpoint(t *testing.T) {
	det, rep, db := newDB(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	det.Check(ctx, opHTTP)
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("check during real tx: got %d violations, want 1", got)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	det.Check(ctx, opHTTP)
	if got := len(rep.Violations()); got != 1 {
		t.Errorf("check after commit: got %d violations, want still 1", got)
	}
}

// A rolled-back real transaction: violating while open, clean after rollback.
func TestDriverTxRollback(t *testing.T) {
	det, rep, db := newDB(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	det.Check(ctx, opHTTP)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	det.Check(ctx, opHTTP)
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("got %d violations, want 1 (only the in-tx check)", got)
	}
}

// Textual BEGIN/COMMIT executed as plain statements on a real connection are
// tracked: a checkpoint violates after BEGIN and is clean after COMMIT.
func TestTextualBeginCommit(t *testing.T) {
	det, rep, db := newDB(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	mustExec(t, conn, ctx, "BEGIN")
	det.Check(ctx, opHTTP)
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("after textual BEGIN: got %d violations, want 1", got)
	}
	mustExec(t, conn, ctx, "COMMIT")
	det.Check(ctx, opHTTP)
	if got := len(rep.Violations()); got != 1 {
		t.Errorf("after textual COMMIT: got %d violations, want still 1", got)
	}
}

// ROLLBACK TO SAVEPOINT must not end the real transaction: a checkpoint after
// it still violates, and only COMMIT clears it.
func TestSavepointKeepsTxOpen(t *testing.T) {
	det, rep, db := newDB(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	for _, q := range []string{
		"BEGIN",
		"INSERT INTO e2e_users (v) VALUES (1)",
		"SAVEPOINT sp",
		"INSERT INTO e2e_audit (v) VALUES (1)",
		"ROLLBACK TO SAVEPOINT sp",
	} {
		mustExec(t, conn, ctx, q)
	}
	det.Check(ctx, opHTTP)
	vs := rep.Violations()
	if len(vs) == 0 {
		t.Fatal("after ROLLBACK TO SAVEPOINT: want a violation (tx still open)")
	}
	mustExec(t, conn, ctx, "COMMIT")
	before := len(rep.Violations())
	det.Check(ctx, opHTTP)
	if got := len(rep.Violations()); got != before {
		t.Errorf("after COMMIT: got %d violations, want still %d", got, before)
	}
}

// Pooled connections stay isolated across scopes: a real transaction open in
// one scope does not make a checkpoint in another scope violate.
func TestPoolIsolationAcrossScopes(t *testing.T) {
	det, rep, db := newDB(t)
	ctxA, finishA := det.StartScope(context.Background(), "ScopeA")
	defer finishA()
	ctxB, finishB := det.StartScope(context.Background(), "ScopeB")
	defer finishB()

	tx, err := db.BeginTx(ctxA, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	det.Check(ctxB, opHTTP)
	if got := len(rep.Violations()); got != 0 {
		t.Fatalf("check in ScopeB: got %d violations, want 0", got)
	}
	det.Check(ctxA, opHTTP)
	vs := rep.Violations()
	if len(vs) != 1 || vs[0].Scope != "ScopeA" {
		t.Fatalf("violations = %+v, want one attributed to ScopeA", vs)
	}
}

// Cross-connection write against real PostgreSQL: a real INSERT on a second
// connection while a transaction is open on the first is a violation, with the
// table extracted as the Op name.
func TestCrossConnectionWrite(t *testing.T) {
	det, rep, db := newDB(t)
	ctx, finish := det.StartScope(context.Background(), "Checkout")
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

	other, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if _, err := other.ExecContext(ctx, "INSERT INTO e2e_audit (v) VALUES (7)"); err != nil {
		t.Fatal(err)
	}

	vs := rep.Violations()
	if len(vs) != 1 {
		t.Fatalf("got %d violations, want 1", len(vs))
	}
	if vs[0].Op != (txnpure.Op{Kind: "db", Name: "e2e_audit"}) {
		t.Errorf("Op = %+v, want {db e2e_audit}", vs[0].Op)
	}
}

// The prepared-statement path (extended protocol) records writes once: a
// prepared INSERT executed on a second connection while the first holds a
// transaction is a single cross-connection violation.
func TestPreparedStatementCrossConnection(t *testing.T) {
	det, rep, db := newDB(t)
	ctx, finish := det.StartScope(context.Background(), "Checkout")
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

	other, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	stmt, err := other.PrepareContext(ctx, "INSERT INTO e2e_users (v) VALUES ($1)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stmt.Close() }()
	if _, err := stmt.ExecContext(ctx, 8); err != nil {
		t.Fatal(err)
	}

	vs := rep.Violations()
	if len(vs) != 1 || vs[0].Op.Name != "e2e_users" {
		t.Fatalf("violations = %+v, want one cross-connection write to e2e_users", vs)
	}
}

// A wrapped RoundTripper and a real transaction together: an outbound HTTP
// request issued while a real tx is open is a violation.
func TestRoundTripperInsideRealTx(t *testing.T) {
	det, rep, db := newDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	client := &http.Client{Transport: det.WrapRoundTripper(nil)}

	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	vs := rep.Violations()
	if len(vs) != 1 || vs[0].Op.Kind != "http" {
		t.Fatalf("violations = %+v, want one http violation", vs)
	}
}

// A real transaction begun with no scope in its context is surfaced by
// WithUnscopedTxDetection.
func TestUnscopedTxDetection(t *testing.T) {
	_, rep, db := newDB(t, txnpure.WithUnscopedTxDetection())

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := len(rep.UnscopedTxs()); got != 1 {
		t.Fatalf("got %d unscoped txs, want 1", got)
	}
	rep.RequireNoViolations(t)
}

func mustExec(t *testing.T, conn *sql.Conn, ctx context.Context, query string) {
	t.Helper()
	if _, err := conn.ExecContext(ctx, query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
