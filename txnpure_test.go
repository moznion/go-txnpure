package txnpure_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	txnpure "github.com/moznion/go-txnpure"
)

var opHTTP = txnpure.Op{Kind: "http", Name: "GET api.example.com"}

func setup(t *testing.T, opts ...txnpure.Option) (*txnpure.Detector, *txnpure.CollectingReporter, *sql.DB) {
	t.Helper()
	rep := txnpure.NewCollectingReporter()
	det := txnpure.New(append([]txnpure.Option{txnpure.WithReporter(rep)}, opts...)...)
	db := det.NewNullDB()
	t.Cleanup(func() { _ = db.Close() })
	return det, rep, db
}

func TestDriverTxCheckViolates(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	det.Check(ctx, opHTTP)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	vs := rep.Violations()
	if len(vs) != 1 {
		t.Fatalf("got %d violations, want 1", len(vs))
	}
	v := vs[0]
	if v.Op != opHTTP {
		t.Errorf("Op = %+v, want %+v", v.Op, opHTTP)
	}
	if v.Scope != "CreateUser" {
		t.Errorf("Scope = %q, want %q", v.Scope, "CreateUser")
	}
	if v.OpenTxs != 1 {
		t.Errorf("OpenTxs = %d, want 1", v.OpenTxs)
	}
	if v.Time.IsZero() {
		t.Error("Time is zero")
	}
	if !strings.Contains(v.String(), "CreateUser") || !strings.Contains(v.String(), "GET api.example.com") {
		t.Errorf("String() = %q lacks scope/op", v.String())
	}
}

func TestCheckAfterCommitIsClean(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	det.Check(ctx, opHTTP)
	rep.RequireNoViolations(t)
}

func TestCheckAfterRollbackIsClean(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	det.Check(ctx, opHTTP)
	rep.RequireNoViolations(t)
}

func TestCheckBeforeAnyTxIsClean(t *testing.T) {
	det, rep, _ := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()
	det.Check(ctx, opHTTP)
	rep.RequireNoViolations(t)
}

func TestTextualBeginCommit(t *testing.T) {
	det, rep, db := setup(t)
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

func TestSavepointStatementsKeepTxOpen(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	mustExec(t, conn, ctx, "BEGIN")
	mustExec(t, conn, ctx, "SAVEPOINT sp1")
	mustExec(t, conn, ctx, "ROLLBACK TO SAVEPOINT sp1")
	mustExec(t, conn, ctx, "RELEASE SAVEPOINT sp1")

	det.Check(ctx, opHTTP)
	vs := rep.Violations()
	if len(vs) != 1 || vs[0].OpenTxs != 1 {
		t.Fatalf("savepoint statements must keep the tx open: violations = %+v", vs)
	}

	mustExec(t, conn, ctx, "ROLLBACK")
	det.Check(ctx, opHTTP)
	if got := len(rep.Violations()); got != 1 {
		t.Errorf("after ROLLBACK: got %d violations, want still 1", got)
	}
}

// A textual COMMIT inside a driver-level transaction must decrement the scope
// counter at most once: the later driver-level Commit is a no-op close.
func TestCommitIdempotence(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	det.Check(ctx, opHTTP)
	rep.RequireNoViolations(t)

	// The counter must not have gone negative: a fresh tx must violate with
	// OpenTxs == 1.
	tx2, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Commit() }()
	det.Check(ctx, opHTTP)
	vs := rep.Violations()
	if len(vs) != 1 || vs[0].OpenTxs != 1 {
		t.Fatalf("after double close + fresh begin: violations = %+v, want one with OpenTxs=1", vs)
	}
}

func TestTwoConcurrentTxsInOneScope(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx1, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	tx2, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	det.Check(ctx, opHTTP)
	vs := rep.Violations()
	if len(vs) != 1 || vs[0].OpenTxs != 2 {
		t.Fatalf("with two open txs: violations = %+v, want one with OpenTxs=2", vs)
	}

	if err := tx1.Commit(); err != nil {
		t.Fatal(err)
	}
	det.Check(ctx, opHTTP)
	vs = rep.Violations()
	if len(vs) != 2 || vs[1].OpenTxs != 1 {
		t.Fatalf("with one tx still open: violations = %+v, want a second one with OpenTxs=1", vs)
	}

	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	det.Check(ctx, opHTTP)
	if got := len(rep.Violations()); got != 2 {
		t.Errorf("after both commits: got %d violations, want still 2", got)
	}
}

// Pooled connections must not cause cross-request false positives: another
// scope's open transaction lives on a different scope holder.
func TestPooledConnIsolationAcrossScopes(t *testing.T) {
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

	det.Check(ctxB, opHTTP)
	rep.RequireNoViolations(t)

	det.Check(ctxA, opHTTP)
	vs := rep.Violations()
	if len(vs) != 1 || vs[0].Scope != "ScopeA" {
		t.Fatalf("violations = %+v, want one attributed to ScopeA", vs)
	}
}

// A goroutine sharing the scope ctx that runs a side effect while the
// transaction is open is a true positive.
func TestGoroutineSharingCtx(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		det.Check(ctx, opHTTP)
	}()
	<-done
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("goroutine check during tx: got %d violations, want 1", got)
	}

	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		det.Check(ctx, opHTTP)
	}()
	<-done2
	if got := len(rep.Violations()); got != 1 {
		t.Errorf("goroutine check after commit: got %d violations, want still 1", got)
	}
}

// The innermost scope is the most specific attribution: a transaction opened
// under the outer scope is invisible to checks running under the inner one.
func TestNestedScopeShadows(t *testing.T) {
	det, rep, db := setup(t)
	outerCtx, finishOuter := det.StartScope(context.Background(), "Outer")
	defer finishOuter()

	tx, err := db.BeginTx(outerCtx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	innerCtx, finishInner := det.StartScope(outerCtx, "Inner")
	defer finishInner()

	det.Check(innerCtx, opHTTP)
	rep.RequireNoViolations(t)

	det.Check(outerCtx, opHTTP)
	vs := rep.Violations()
	if len(vs) != 1 || vs[0].Scope != "Outer" {
		t.Fatalf("violations = %+v, want one attributed to Outer", vs)
	}
}

func TestCheckWithNoScopeIsSilent(t *testing.T) {
	det, rep, db := setup(t)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	det.Check(context.Background(), opHTTP)
	rep.RequireNoViolations(t)
}

func TestUnscopedCheckDetection(t *testing.T) {
	det, rep, _ := setup(t, txnpure.WithUnscopedCheckDetection())

	// A checkpoint with no scope on its context is reported (never a
	// Violation), and its stack points at the caller.
	det.Check(context.Background(), opHTTP)
	rep.RequireNoViolations(t)
	us := rep.UnscopedChecks()
	if len(us) != 1 {
		t.Fatalf("got %d unscoped checks, want 1", len(us))
	}
	if us[0].Op != opHTTP {
		t.Errorf("Op = %+v, want %+v", us[0].Op, opHTTP)
	}
	if us[0].Time.IsZero() {
		t.Error("Time is zero")
	}
	if len(us[0].Stack) == 0 || !strings.Contains(us[0].Stack[0].Function, "TestUnscopedCheckDetection") {
		t.Errorf("Stack[0] = %+v, want the caller of Check", us[0].Stack)
	}

	// A checkpoint inside a scope is not reported as unscoped.
	rep.Reset()
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()
	det.Check(ctx, opHTTP)
	rep.RequireNoUnscopedChecks(t)
}

func TestUnscopedCheckDetectionOffByDefault(t *testing.T) {
	det, rep, _ := setup(t)
	det.Check(context.Background(), opHTTP)
	rep.RequireNoUnscopedChecks(t)
}

func TestInScope(t *testing.T) {
	det, rep, db := setup(t)
	wantErr := errors.New("boom")
	err := det.InScope(context.Background(), "CreateUser", func(ctx context.Context) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Commit() }()
		det.Check(ctx, opHTTP)
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("InScope returned %v, want %v", err, wantErr)
	}
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("got %d violations, want 1", got)
	}
}

func TestDoRunsFAndChecks(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	ran := false
	wantErr := errors.New("send failed")
	err = det.Do(ctx, txnpure.Op{Kind: "enqueue", Name: "sqs:SendMessage"}, func(context.Context) error {
		ran = true
		return wantErr
	})
	if !ran {
		t.Error("Do did not run f — the side effect must never be blocked")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Do returned %v, want %v", err, wantErr)
	}
	vs := rep.Violations()
	if len(vs) != 1 || vs[0].Op.Kind != "enqueue" {
		t.Fatalf("violations = %+v, want one enqueue violation", vs)
	}
}

func TestStackPointsAtCheckCaller(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	det.Check(ctx, opHTTP)
	vs := rep.Violations()
	if len(vs) != 1 {
		t.Fatalf("got %d violations, want 1", len(vs))
	}
	stack := vs[0].Stack
	if len(stack) == 0 {
		t.Fatal("stack is empty")
	}
	if !strings.Contains(stack[0].Function, "TestStackPointsAtCheckCaller") {
		t.Errorf("stack[0] = %+v, want the caller of Check, not txnpure internals", stack[0])
	}
	if !strings.HasSuffix(stack[0].File, "_test.go") {
		t.Errorf("stack[0].File = %q, want this test file", stack[0].File)
	}
	if stack[0].Line <= 0 {
		t.Errorf("stack[0].Line = %d, want > 0", stack[0].Line)
	}
}

func TestWithStackDepthZeroDisablesCapture(t *testing.T) {
	det, rep, db := setup(t, txnpure.WithStackDepth(0))
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	det.Check(ctx, opHTTP)
	vs := rep.Violations()
	if len(vs) != 1 {
		t.Fatalf("got %d violations, want 1", len(vs))
	}
	if len(vs[0].Stack) != 0 {
		t.Errorf("stack = %+v, want empty with WithStackDepth(0)", vs[0].Stack)
	}
}

func TestUnscopedTxDetection(t *testing.T) {
	det, rep, db := setup(t, txnpure.WithUnscopedTxDetection())

	tx, err := db.Begin() // no ctx → necessarily unscoped
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	us := rep.UnscopedTxs()
	if len(us) != 1 {
		t.Fatalf("got %d unscoped txs, want 1", len(us))
	}
	if us[0].Time.IsZero() {
		t.Error("UnscopedTx.Time is zero")
	}
	rep.RequireNoViolations(t)

	// A scoped begin must not be reported.
	rep.Reset()
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()
	tx2, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoUnscopedTxs(t)
}

func TestUnscopedTxDetectionOffByDefault(t *testing.T) {
	_, rep, db := setup(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoUnscopedTxs(t)
}

func TestLeakedTxDetection(t *testing.T) {
	det, rep, db := setup(t, txnpure.WithLeakedTxDetection())
	ctx, finish := det.StartScope(context.Background(), "CreateUser")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	finish()
	finish() // idempotent: must not double-report

	ls := rep.LeakedTxs()
	if len(ls) != 1 {
		t.Fatalf("got %d leaked txs, want 1", len(ls))
	}
	if ls[0].Scope != "CreateUser" || ls[0].OpenTxs != 1 {
		t.Errorf("LeakedTx = %+v, want scope CreateUser with 1 open tx", ls[0])
	}
	_ = tx.Rollback()
}

func TestNoLeakReportWhenTxClosed(t *testing.T) {
	det, rep, db := setup(t, txnpure.WithLeakedTxDetection())
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finish()
	rep.RequireNoLeakedTxs(t)
}

func TestNestedScopeDetection(t *testing.T) {
	det, rep, _ := setup(t, txnpure.WithNestedScopeDetection())
	outerCtx, finishOuter := det.StartScope(context.Background(), "Outer")
	defer finishOuter()
	_, finishInner := det.StartScope(outerCtx, "Inner")
	defer finishInner()

	ns := rep.NestedScopes()
	if len(ns) != 1 || ns[0].Outer != "Outer" || ns[0].Inner != "Inner" {
		t.Fatalf("nested scopes = %+v, want one Outer/Inner occurrence", ns)
	}
}

func mustExec(t *testing.T, conn *sql.Conn, ctx context.Context, query string) {
	t.Helper()
	if _, err := conn.ExecContext(ctx, query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
