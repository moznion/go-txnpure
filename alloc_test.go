package txnpure

import (
	"context"
	"database/sql/driver"
	"net/http"
	"net/url"
	"testing"
)

// TestHotPathAllocations pins the exact allocation budget of every path that
// runs when nothing is wrong. txnpure is designed to be enabled always, so
// these paths must stay allocation-free (StartScope has a small fixed budget);
// any increase is a performance regression and must fail CI deterministically
// — unlike time-based benchmarks, allocation counts do not flake.
//
// Skipped under -race (the race runtime allocates); CI runs this test in a
// separate step without -race.
func TestHotPathAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation budgets are not meaningful under -race")
	}

	det := New()
	scoped, finish := det.StartScope(context.Background(), "AllocBudget")
	defer finish()
	unscoped := context.Background()

	assertAllocs := func(t *testing.T, want float64, f func()) {
		t.Helper()
		if got := testing.AllocsPerRun(100, f); got != want {
			t.Errorf("got %v allocs/op, want %v", got, want)
		}
	}

	t.Run("classifier", func(t *testing.T) {
		queries := []string{
			"select id, name from users where id = $1",
			"insert into users (id, name) values ($1, $2)",
			"begin",
			"with recent as (select * from orders where ts > $1) select count(*) from recent",
			"/* trace:abc */ -- hint\n select 1",
		}
		for _, q := range queries {
			assertAllocs(t, 0, func() { _ = DefaultClassifier(q) })
		}
	})

	t.Run("observe", func(t *testing.T) {
		conn := &wrappedConn{det: det, conn: nullConn{}}
		assertAllocs(t, 0, func() { conn.observe(scoped, "select id from users where id = $1") })
		assertAllocs(t, 0, func() { conn.observe(scoped, "update users set name = $1 where id = $2") })
		assertAllocs(t, 0, func() { conn.observe(unscoped, "select id from users where id = $1") })
	})

	t.Run("check", func(t *testing.T) {
		op := Op{Kind: "http", Name: "GET api.example.com"}
		assertAllocs(t, 0, func() { det.Check(unscoped, op) })
		assertAllocs(t, 0, func() { det.Check(scoped, op) })
	})

	t.Run("roundtrip_no_open_tx", func(t *testing.T) {
		rt := det.WrapRoundTripper(allocFreeRoundTripper{})
		req := (&http.Request{
			Method: http.MethodGet,
			URL:    &url.URL{Scheme: "https", Host: "api.example.com", Path: "/v1/things"},
		}).WithContext(scoped)
		assertAllocs(t, 0, func() { _, _ = rt.RoundTrip(req) })
	})

	t.Run("driver_begin_commit", func(t *testing.T) {
		// The txnpure share of a transaction: the wrapped driver adds no
		// allocations on top of the underlying driver and database/sql.
		conn := &wrappedConn{det: det, conn: nullConn{}}
		assertAllocs(t, 0, func() {
			tx, err := conn.BeginTx(scoped, driver.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		})
	})

	t.Run("prepared_stmt_exec", func(t *testing.T) {
		conn := &wrappedConn{det: det, conn: nullConn{}}
		stmt, err := conn.PrepareContext(scoped, "with recent as (select 1) select * from recent")
		if err != nil {
			t.Fatal(err)
		}
		sec := stmt.(driver.StmtExecContext)
		assertAllocs(t, 0, func() {
			if _, err := sec.ExecContext(scoped, nil); err != nil {
				t.Fatal(err)
			}
		})
	})

	t.Run("violation_stack_disabled", func(t *testing.T) {
		// Even reporting a violation is allocation-free when stack capture is
		// off and no attrs are attached (reporter internals excluded).
		vdet := New(WithReporter(allocFreeReporter{}), WithStackDepth(0))
		vctx, vfinish := vdet.StartScope(context.Background(), "AllocBudget")
		defer vfinish()
		s := scopeFrom(vctx)
		s.openTxs.Add(1)
		defer s.openTxs.Add(-1)
		op := Op{Kind: "http", Name: "GET api.example.com"}
		assertAllocs(t, 0, func() { vdet.Check(vctx, op) })
	})

	t.Run("check_marked_suppressed", func(t *testing.T) {
		// An AllowInTransactionHere region suppressing an in-transaction
		// checkpoint stays allocation-free in steady state: the mark rides
		// the existing scope lookup and the call counter entry is reused.
		mdet := New(WithReporter(allocFreeReporter{}), WithStackDepth(0))
		mctx, mfinish := mdet.StartScope(context.Background(), "AllocBudget")
		defer mfinish()
		s := scopeFrom(mctx)
		s.openTxs.Add(1)
		defer s.openTxs.Add(-1)
		actx := AllowInTransactionHere(mctx, "alloc budget")
		op := Op{Kind: "http", Name: "GET api.example.com"}
		assertAllocs(t, 0, func() { mdet.Check(actx, op) })
	})

	t.Run("check_marked_no_tx", func(t *testing.T) {
		// The marked checkpoint with no open transaction (per-event
		// StaleAllow path, reporter not opting in) is allocation-free too.
		mdet := New(WithReporter(allocFreeReporter{}), WithStackDepth(0))
		mctx, mfinish := mdet.StartScope(context.Background(), "AllocBudget")
		defer mfinish()
		actx := AllowInTransactionHere(mctx, "alloc budget")
		op := Op{Kind: "http", Name: "GET api.example.com"}
		assertAllocs(t, 0, func() { mdet.Check(actx, op) })
	})

	t.Run("start_scope", func(t *testing.T) {
		// Fixed by design: the scope holder, the context value, and the
		// finish closure — one small allocation each, per scope (request).
		assertAllocs(t, 3, func() {
			_, f := det.StartScope(unscoped, "AllocBudget")
			f()
		})
	})
}

type allocFreeRoundTripper struct{}

var sharedResponse = &http.Response{StatusCode: http.StatusOK}

func (allocFreeRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return sharedResponse, nil
}

type allocFreeReporter struct{}

func (allocFreeReporter) Report(context.Context, Violation) {}
