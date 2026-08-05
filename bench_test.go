package txnpure

import (
	"context"
	"net/http"
	"net/url"
	"testing"
)

// benchSink prevents the compiler from optimizing the measured call away.
var benchSinkKind StatementKind

func BenchmarkDefaultClassifier(b *testing.B) {
	cases := []struct {
		name  string
		query string
	}{
		{"select_lower", "select id, name from users where id = $1"},
		{"select_upper", "SELECT id, name FROM users WHERE id = $1"},
		{"insert_lower", "insert into users (id, name) values ($1, $2)"},
		{"begin_lower", "begin"},
		{"commit_upper", "COMMIT"},
		{"rollback_to_savepoint", "ROLLBACK TO SAVEPOINT sp1"},
		{"with_read_lower", "with recent as (select * from orders where ts > $1) select count(*) from recent"},
		{"with_write_lower", "with moved as (delete from queue returning *) insert into archive select * from moved"},
		{"comment_prefixed", "/* trace:abc */ -- hint\n select 1"},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				benchSinkKind = DefaultClassifier(c.query)
			}
		})
	}
}

func BenchmarkObserve(b *testing.B) {
	det := New()
	scoped, finish := det.StartScope(context.Background(), "bench")
	defer finish()

	b.Run("select_no_scope", func(b *testing.B) {
		conn := &wrappedConn{det: det, conn: nullConn{}}
		ctx := context.Background()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			conn.observe(ctx, "select id from users where id = $1")
		}
	})
	b.Run("select_in_scope", func(b *testing.B) {
		conn := &wrappedConn{det: det, conn: nullConn{}}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			conn.observe(scoped, "select id from users where id = $1")
		}
	})
	b.Run("write_in_scope_no_tx", func(b *testing.B) {
		conn := &wrappedConn{det: det, conn: nullConn{}}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			conn.observe(scoped, "update users set name = $1 where id = $2")
		}
	})
	b.Run("write_in_scope_own_tx", func(b *testing.B) {
		conn := &wrappedConn{det: det, conn: nullConn{}}
		conn.openTx(scoped)
		defer conn.closeTx()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			conn.observe(scoped, "update users set name = $1 where id = $2")
		}
	})
}

func BenchmarkCheck(b *testing.B) {
	op := Op{Kind: "http", Name: "GET api.example.com"}

	b.Run("no_scope", func(b *testing.B) {
		det := New()
		ctx := context.Background()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			det.Check(ctx, op)
		}
	})
	b.Run("scope_no_tx", func(b *testing.B) {
		det := New()
		ctx, finish := det.StartScope(context.Background(), "bench")
		defer finish()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			det.Check(ctx, op)
		}
	})
	b.Run("violation_stack32", func(b *testing.B) {
		det := New(WithReporter(benchNoopReporter{}))
		ctx, finish := det.StartScope(context.Background(), "bench")
		defer finish()
		s := scopeFrom(ctx)
		s.openTxs.Add(1)
		defer s.openTxs.Add(-1)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			det.Check(ctx, op)
		}
	})
	b.Run("violation_stack0", func(b *testing.B) {
		det := New(WithReporter(benchNoopReporter{}), WithStackDepth(0))
		ctx, finish := det.StartScope(context.Background(), "bench")
		defer finish()
		s := scopeFrom(ctx)
		s.openTxs.Add(1)
		defer s.openTxs.Add(-1)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			det.Check(ctx, op)
		}
	})
	b.Run("marked_suppressed", func(b *testing.B) {
		// An AllowInTransactionHere region suppressing an in-transaction
		// checkpoint: the mark lookup rides the scope lookup, plus one
		// counter bump per event.
		det := New(WithReporter(benchNoopReporter{}), WithStackDepth(0))
		ctx, finish := det.StartScope(context.Background(), "bench")
		defer finish()
		s := scopeFrom(ctx)
		s.openTxs.Add(1)
		defer s.openTxs.Add(-1)
		actx := AllowInTransactionHere(ctx, "bench")
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			det.Check(actx, op)
		}
	})
	b.Run("marked_scope_no_tx", func(b *testing.B) {
		// The marked fast path with no open transaction: the per-event
		// StaleAllow goes to a reporter that does not opt in.
		det := New(WithReporter(benchNoopReporter{}), WithStackDepth(0))
		ctx, finish := det.StartScope(context.Background(), "bench")
		defer finish()
		actx := AllowInTransactionHere(ctx, "bench")
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			det.Check(actx, op)
		}
	})
}

type benchNoopReporter struct{}

func (benchNoopReporter) Report(context.Context, Violation) {}

type benchNoopRoundTripper struct{}

func (benchNoopRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK}, nil
}

func BenchmarkRoundTripper(b *testing.B) {
	det := New()
	rt := det.WrapRoundTripper(benchNoopRoundTripper{})

	newReq := func(ctx context.Context) *http.Request {
		return (&http.Request{
			Method: http.MethodGet,
			URL:    &url.URL{Scheme: "https", Host: "api.example.com", Path: "/v1/things"},
		}).WithContext(ctx)
	}

	b.Run("no_scope", func(b *testing.B) {
		req := newReq(context.Background())
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = rt.RoundTrip(req)
		}
	})
	b.Run("scope_no_tx", func(b *testing.B) {
		ctx, finish := det.StartScope(context.Background(), "bench")
		defer finish()
		req := newReq(ctx)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = rt.RoundTrip(req)
		}
	})
}

func BenchmarkStartScope(b *testing.B) {
	det := New()
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sctx, finish := det.StartScope(ctx, "bench")
		finish()
		_ = sctx
	}
}

func BenchmarkNullDBEndToEnd(b *testing.B) {
	det := New()
	db := det.NewNullDB()
	defer func() { _ = db.Close() }()
	ctx, finish := det.StartScope(context.Background(), "bench")
	defer finish()

	b.Run("exec_select", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := db.ExecContext(ctx, "select 1"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("prepared_stmt_exec_with_cte", func(b *testing.B) {
		stmt, err := db.PrepareContext(ctx,
			"with recent as (select * from orders where ts > $1 and status in ('a','b','c')) select count(*) from recent")
		if err != nil {
			b.Fatal(err)
		}
		defer func() { _ = stmt.Close() }()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := stmt.ExecContext(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("tx_begin_commit", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				b.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
