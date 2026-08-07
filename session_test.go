package txnpure

import (
	"context"
	"testing"
)

// The Session tests drive the exported native-driver observation surface
// directly — no driver, no database — and assert it applies the same rules as
// the driver middleware (which is built on it): checkpoints trip while a
// textual or BeginTx transaction is open, and every governance/observability
// path (cross-conn writes, statement checkers, unscoped/leaked reporting)
// behaves identically.

func TestSessionTextualTransactionTripsCheckpoints(t *testing.T) {
	reporter := NewCollectingReporter()
	det := New(WithReporter(reporter))
	s := det.NewSession()
	op := Op{Kind: "http", Name: "GET api.example.com"}

	ctx, finish := det.StartScope(context.Background(), "session-textual")
	defer finish()

	det.Check(ctx, op)
	s.Observe(ctx, "BEGIN")
	det.Check(ctx, op)
	s.Observe(ctx, "COMMIT")
	det.Check(ctx, op)

	vs := reporter.Violations()
	if len(vs) != 1 {
		t.Fatalf("expected exactly 1 violation (the in-transaction check), got %d: %+v", len(vs), vs)
	}
	if vs[0].Scope != "session-textual" || vs[0].Op != op {
		t.Errorf("unexpected violation identity: %+v", vs[0])
	}
}

func TestSessionRollbackClosesTransaction(t *testing.T) {
	reporter := NewCollectingReporter()
	det := New(WithReporter(reporter))
	s := det.NewSession()

	ctx, finish := det.StartScope(context.Background(), "session-rollback")
	defer finish()

	s.Observe(ctx, "BEGIN")
	s.Observe(ctx, "ROLLBACK")
	det.Check(ctx, Op{Kind: "http", Name: "GET api.example.com"})

	reporter.RequireNoViolations(t)
}

// ROLLBACK TO SAVEPOINT rewinds inside the transaction without ending it: a
// checkpoint after it must still trip.
func TestSessionSavepointRollbackKeepsTransactionOpen(t *testing.T) {
	reporter := NewCollectingReporter()
	det := New(WithReporter(reporter))
	s := det.NewSession()

	ctx, finish := det.StartScope(context.Background(), "session-savepoint")
	defer finish()

	s.Observe(ctx, "BEGIN")
	s.Observe(ctx, "SAVEPOINT sp")
	s.Observe(ctx, "ROLLBACK TO SAVEPOINT sp")
	det.Check(ctx, Op{Kind: "http", Name: "GET api.example.com"})
	s.Observe(ctx, "COMMIT")

	if vs := reporter.Violations(); len(vs) != 1 {
		t.Fatalf("expected 1 violation (savepoint rollback keeps the tx open), got %+v", vs)
	}
}

// BeginTx/EndTx bracket transactions that never surface as statement text
// (driver-API transactions, pgx batch pipelines); the ctx passed to BeginTx
// decides the scope attribution.
func TestSessionBeginTxEndTxBracketTransaction(t *testing.T) {
	reporter := NewCollectingReporter()
	det := New(WithReporter(reporter))
	s := det.NewSession()
	op := Op{Kind: "enqueue", Name: "sqs:SendMessage"}

	ctx, finish := det.StartScope(context.Background(), "session-api-tx")
	defer finish()

	s.BeginTx(ctx)
	det.Check(ctx, op)
	s.EndTx()
	det.Check(ctx, op)
	s.EndTx() // idempotent: must not drive the counter negative

	if vs := reporter.Violations(); len(vs) != 1 {
		t.Fatalf("expected 1 violation (inside BeginTx/EndTx only), got %+v", vs)
	}
}

// A driver-level begin over a textual transaction closes the old one first —
// the counter must end at zero, not stick high.
func TestSessionBeginTxOverTextualTxDoesNotStickCounter(t *testing.T) {
	reporter := NewCollectingReporter()
	det := New(WithReporter(reporter))
	s := det.NewSession()

	ctx, finish := det.StartScope(context.Background(), "session-begin-over-textual")
	defer finish()

	s.Observe(ctx, "BEGIN")
	s.BeginTx(ctx)
	s.EndTx()
	det.Check(ctx, Op{Kind: "http", Name: "GET api.example.com"})

	reporter.RequireNoViolations(t)
}

// Two sessions are two connections: a write on one while the other holds an
// open transaction in the same scope is a cross-connection-write Violation,
// exactly as with two wrapped driver conns.
func TestSessionCrossConnectionWrite(t *testing.T) {
	reporter := NewCollectingReporter()
	det := New(WithReporter(reporter))
	s1 := det.NewSession()
	s2 := det.NewSession()

	ctx, finish := det.StartScope(context.Background(), "session-cross-conn")
	defer finish()

	s1.Observe(ctx, "BEGIN")
	s1.Observe(ctx, "INSERT INTO orders (id) VALUES (1)") // own tx: silent
	s2.Observe(ctx, "INSERT INTO audit (id) VALUES (1)")  // foreign tx open: violation
	s1.Observe(ctx, "COMMIT")

	vs := reporter.Violations()
	if len(vs) != 1 {
		t.Fatalf("expected 1 cross-connection violation, got %+v", vs)
	}
	if want := (Op{Kind: "db", Name: "audit"}); vs[0].Op != want {
		t.Errorf("expected op %+v, got %+v", want, vs[0].Op)
	}
}

// Statement checkers run on the Session path too: a statement declared to be
// an external call violates inside the session's own transaction.
func TestSessionStatementChecker(t *testing.T) {
	reporter := NewCollectingReporter()
	det := New(
		WithReporter(reporter),
		WithStatementChecker(func(query string) (Op, bool) {
			if query == "select notify_external('x')" {
				return Op{Kind: "ext", Name: "notify_external"}, true
			}
			return Op{}, false
		}),
	)
	s := det.NewSession()

	ctx, finish := det.StartScope(context.Background(), "session-stmt-checker")
	defer finish()

	s.Observe(ctx, "BEGIN")
	s.Observe(ctx, "select notify_external('x')")
	s.Observe(ctx, "COMMIT")

	vs := reporter.Violations()
	if len(vs) != 1 || vs[0].Op != (Op{Kind: "ext", Name: "notify_external"}) {
		t.Fatalf("expected 1 statement-checker violation, got %+v", vs)
	}
}

// A transaction begun with no scope on its context is invisible to
// checkpoints; WithUnscopedTxDetection surfaces that hole — never a
// Violation — identically to the driver middleware.
func TestSessionUnscopedTxDetection(t *testing.T) {
	reporter := NewCollectingReporter()
	det := New(WithReporter(reporter), WithUnscopedTxDetection())
	s := det.NewSession()

	s.Observe(context.Background(), "BEGIN")
	s.Observe(context.Background(), "COMMIT")

	reporter.RequireNoViolations(t)
	if got := reporter.UnscopedTxs(); len(got) != 1 {
		t.Fatalf("expected 1 unscoped tx, got %+v", got)
	}
}

// A scope finishing while a session transaction is still open reports a
// LeakedTx (never a Violation) — the forgotten-EndTx / leaked-connection net.
func TestSessionLeakedTxDetection(t *testing.T) {
	reporter := NewCollectingReporter()
	det := New(WithReporter(reporter), WithLeakedTxDetection())
	s := det.NewSession()

	ctx, finish := det.StartScope(context.Background(), "session-leak")
	s.Observe(ctx, "BEGIN")
	finish()

	reporter.RequireNoViolations(t)
	if got := reporter.LeakedTxs(); len(got) != 1 || got[0].Scope != "session-leak" {
		t.Fatalf("expected 1 leaked tx in scope session-leak, got %+v", got)
	}
	s.EndTx()
}

// The driver middleware is built on Session, so both paths must produce the
// same verdict for the same statement stream: this pins the wiring (a
// regression here means the driver stopped delegating).
func TestSessionAndDriverPathsAgree(t *testing.T) {
	run := func(t *testing.T, observe func(ctx context.Context, det *Detector, stmts []string)) []Violation {
		t.Helper()
		reporter := NewCollectingReporter()
		det := New(WithReporter(reporter))
		ctx, finish := det.StartScope(context.Background(), "parity")
		defer finish()
		stmts := []string{"BEGIN", "SELECT 1"}
		observe(ctx, det, stmts)
		det.Check(ctx, Op{Kind: "http", Name: "GET api.example.com"})
		return reporter.Violations()
	}

	viaSession := run(t, func(ctx context.Context, det *Detector, stmts []string) {
		s := det.NewSession()
		for _, q := range stmts {
			s.Observe(ctx, q)
		}
	})
	viaDriver := run(t, func(ctx context.Context, det *Detector, stmts []string) {
		db := det.NewNullDB()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range stmts {
			if _, err := conn.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
	})

	if len(viaSession) != 1 || len(viaDriver) != 1 {
		t.Fatalf("expected 1 violation on both paths, got session=%d driver=%d", len(viaSession), len(viaDriver))
	}
	if viaSession[0].Scope != viaDriver[0].Scope || viaSession[0].Op != viaDriver[0].Op || viaSession[0].OpenTxs != viaDriver[0].OpenTxs {
		t.Errorf("paths disagree: session=%+v driver=%+v", viaSession[0], viaDriver[0])
	}
}
