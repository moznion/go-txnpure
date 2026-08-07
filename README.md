# go-txnpure

[![check](https://github.com/moznion/go-txnpure/actions/workflows/check.yml/badge.svg)](https://github.com/moznion/go-txnpure/actions/workflows/check.yml)

`go-txnpure` detects **side effects executed while a database transaction is
open**: HTTP calls, message/job enqueues, mail sending — anything that cannot
be rolled back when the surrounding transaction aborts, and anything that can
observe (or be observed by) state the transaction has not committed yet.

It is the sibling of [go-txnproof](https://github.com/moznion/go-txnproof)
and covers the inverse failure mode:

|            | go-txnproof                              | go-txnpure                              |
|------------|------------------------------------------|-----------------------------------------|
| Detects    | writes **not** wrapped in one transaction | side effects **inside** an open transaction |
| Property   | atomicity of a logical boundary          | purity of a transaction                 |
| Reports at | boundary finish (counting)               | the moment the side effect runs         |

Zero dependencies outside the Go standard library. One detector serves three
modes: pure unit tests (`NewNullDB`), tests against a real database (wrap the
real driver), and continuous production monitoring (pluggable `Reporter`s —
the overhead per checkpoint is a context lookup and an atomic load).

## Inspiration

The idea is borrowed from [palkan/isolator](https://github.com/palkan/isolator),
a Ruby gem that catches side effects performed inside a database transaction.
go-txnpure is **not** a port, and deliberately not named after it:

- Ruby can hook side-effect libraries globally by monkey-patching; Go cannot,
  so detection points are explicit instrumentation (a wrapped
  `http.RoundTripper`, a `Check` call in a client wrapper, a driver-observed
  statement) — which fits Go's dependency-injection idioms.
- The overhead here is a context lookup and an atomic load, so the same
  detector runs in **production monitoring**, not just dev/test.
- "isolator"/"isolation" would wrongly suggest isolation-*level* detection,
  which is explicitly out of scope. The name follows the txn- family instead:
  "pure" = free of side effects.

go-txnpure also goes beyond isolator's original scope with cross-connection
write detection (a destructive change to another database inside a
transaction), a governance layer (allowlist, baseline ratchet, throttling),
and opt-in net-hole signals.

## Install

```console
go get github.com/moznion/go-txnpure
```

Requires Go 1.25 or newer. The root module has **zero dependencies** outside
the standard library. The gRPC client interceptors live in a separate
submodule (it pulls in `google.golang.org/grpc`, so the root stays
dependency-free):

```console
go get github.com/moznion/go-txnpure/grpc
```

## How it works

Three cooperating parts, glued by a mutable holder carried on
`context.Context`:

```
middleware            driver wrapper                side-effect instrumentation
──────────            ──────────────                ───────────────────────────
StartScope(ctx)  ───► *scope in ctx
                      BeginTx(ctx):  open-tx counter++
                      Commit/Rollback: counter--
                                                    Check(ctx, op):
                                                      counter > 0 → Violation
```

1. **Scope**: `StartScope(ctx, name)` marks a logical execution (a request, a
   use case, a job) — install it in middleware so every code path is covered.
2. **Driver wrapper**: `Wrap`/`WrapConnector` intercept `database/sql`
   transactions (driver-level `BeginTx` and textual `BEGIN`/`COMMIT` alike)
   and attribute each one to the scope on its context. Works with anything on
   top of `database/sql` — sqlx, GORM, ent, pgx via `stdlib` — no per-ORM
   adapters. Connections that bypass `database/sql` (native pgx, ...) feed
   the same state machine through `Session` (see "Native drivers" below).
3. **Checkpoints**: instrumented side-effect clients call `Check` (or use
   `WrapRoundTripper` / `Do`). If the scope has an open transaction, a
   `Violation` is reported immediately, with a stack trace. The side effect
   itself is **never blocked** — txnpure observes and reports; tests assert.

The database is also a checkpoint on its own, with no extra instrumentation:
a **write on one connection while another connection's transaction is open in
the same scope** is a violation (see below).

### Detection points

There are four ways a side effect becomes a checkpoint, all reported through
the same `(Scope, Op)` identity and governance:

| Detection point | Trigger | Wiring |
|---|---|---|
| `Check` / `Do` | you call it at the side-effect call site | manual, any `Op` |
| `WrapRoundTripper` | an outbound HTTP request is sent | wrap the client's transport |
| Cross-connection writes | a write runs while another connection's tx is open | **automatic** — just wrap the driver |
| `WithStatementChecker` | a statement you declared external runs in a tx | register a matcher |

gRPC RPCs are covered by the interceptors in the `grpc/` submodule (below).
Non-SQL clients (Redis, mail, job queues) use `Check` / `Do`.

## Cross-connection writes (multi-DB) — on by default

In `database/sql` a transaction is bound to a single connection, so each
connection is its own transaction boundary. A write issued on a *different*
connection than the one holding an open transaction cannot be rolled back with
it — the classic "destructive change to DB B inside DB A's transaction" bug,
since two databases are necessarily two connections:

```go
ctx, finish := detector.StartScope(ctx, "Checkout")
defer finish()

tx, _ := dbA.BeginTx(ctx, nil) // DB A transaction open
defer tx.Rollback()
// ...
dbB.ExecContext(ctx, "UPDATE inventory SET qty = qty - 1") // ← Violation:
//   this write lands even if dbA's tx rolls back.
```

This needs no `Check` — wrapping both databases' drivers with the same
detector is enough. It also catches two connections to the *same* database.
Only **writes** trip it (a read on another connection has nothing to roll
back); the violation identity is `(scope, {Kind: "db", Name: "<table>"})`, so
intentional cases are suppressed through the `Allowlist` / `Baseline` keyed on
the table, exactly like any other operation — or right at the write itself
with `AllowInTransactionHere` (see Governance).

## Quick start

```go
package main

import (
	"context"
	"database/sql"
	"net/http"

	txnpure "github.com/moznion/go-txnpure"
)

var detector = txnpure.New(
	txnpure.WithReporter(txnpure.NewSlogReporter(nil)),
)

func main() {
	// 1. Wrap your database driver (or use detector.NewNullDB() in tests).
	sql.Register("pgx-txnpure", detector.Wrap(stdlibDriver))
	db, _ := sql.Open("pgx-txnpure", dsn)

	// 2. Wrap the HTTP client(s) your application uses.
	client := &http.Client{Transport: detector.WrapRoundTripper(nil)}

	// 3. Open a scope per request in middleware.
	handler := scopeMiddleware(mux)
	_ = http.ListenAndServe(":8080", handler)
}

func scopeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, finish := detector.StartScope(r.Context(), r.Method+" "+routePattern(r))
		defer finish()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
```

Now this bug is reported the moment it runs:

```go
tx, _ := db.BeginTx(ctx, nil)
defer tx.Rollback()
// ... writes ...
resp, _ := client.Do(req) // ← Violation: HTTP call while the tx is open.
                          //   If tx rolls back, this call cannot be undone.
tx.Commit()
```

For non-HTTP side effects, instrument the call site with `Check` or `Do`:

```go
err := detector.Do(ctx, txnpure.Op{Kind: "enqueue", Name: "sqs:SendMessage"},
	func(ctx context.Context) error { return queue.Send(ctx, msg) })
```

### Native drivers (pgx, …)

Connections that do not go through `database/sql` have no driver to wrap.
`Session` is the same transaction-attribution state machine the driver
wrapper is built on, exported for those integrations: create one Session per
connection, feed it every executed statement with the context that carried
it, and bracket protocol-level transactions that never surface as statement
text with `BeginTx`/`EndTx`.

With native pgx, a tracer is the natural feed — pgx's `Begin()`/`Commit()`
execute textual `begin`/`commit`, so `Observe` tracks them from the text
alone, and a batch (one implicit transaction server-side) is bracketed by
hand:

```go
type txnpureTracer struct {
	det           *txnpure.Detector
	mu            sync.Mutex
	sessions      map[*pgx.Conn]*txnpure.Session
	implicitBatch map[*pgx.Conn]bool
}

func (t *txnpureTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.sessionFor(conn).Observe(ctx, data.SQL) // textual BEGIN/COMMIT tracked here
	return ctx
}

func (t *txnpureTracer) TraceBatchStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	if conn.PgConn().TxStatus() == 'I' { // idle: the batch runs as one implicit tx
		t.sessionFor(conn).BeginTx(ctx)
		t.markImplicitBatch(conn) // a batch inside an explicit tx belongs to that tx: bracket only this one
	}
	return ctx
}

func (t *txnpureTracer) TraceBatchEnd(_ context.Context, conn *pgx.Conn, _ pgx.TraceBatchEndData) {
	if t.unmarkImplicitBatch(conn) {
		t.sessionFor(conn).EndTx()
	}
}
```

Call `EndTx` when a connection is torn down, too — it is idempotent, and a
dropped connection must not leave the scope counter stuck high. Everything
else (checkpoints, cross-connection writes, statement checkers, governance)
behaves exactly as with the wrapped driver, because it is the same machine.

### Declaring your own external calls

Beyond the built-in detection points (HTTP, cross-connection writes), you can
register matchers that treat certain SQL statements as external calls — an
HTTP-calling stored procedure, a `NOTIFY`/publish, a `dblink`/FDW call. Each
match is checked against the open transaction automatically, no per-call-site
`Check` needed:

```go
det := txnpure.New(
	txnpure.WithReporter(rep),
	txnpure.WithStatementChecker(func(query string) (txnpure.Op, bool) {
		if strings.Contains(query, "pg_notify") {
			return txnpure.Op{Kind: "notify", Name: "pg_notify"}, true
		}
		return txnpure.Op{}, false
	}),
)
```

A matched statement is a violation even inside its own transaction (the
embedded side effect cannot be rolled back). Matchers run on the statement hot
path while a transaction is open, so keep them cheap (a substring or
leading-keyword test). For non-SQL clients — Redis, a mail API, a job queue —
there is no global hook in Go; wrap them with `Do`/`Check` (the
`Op.Kind`/`Op.Name` are free-form). See `examples/enqueue` for the job/mail
pattern.

### gRPC

The `grpc/` submodule ships client interceptors so every outgoing RPC is a
checkpoint:

```go
import txnpuregrpc "github.com/moznion/go-txnpure/grpc"

conn, err := grpc.NewClient(target,
	grpc.WithUnaryInterceptor(txnpuregrpc.UnaryClientInterceptor(detector)),
	grpc.WithStreamInterceptor(txnpuregrpc.StreamClientInterceptor(detector)),
)
```

An RPC issued while a transaction is open reports a violation with
`Op{Kind: "grpc", Name: "/pkg.Service/Method"}`. It is a separate module, so
the root stays zero-dependency.

### Asserting in unit tests

`NewNullDB` returns a `*sql.DB` backed by a no-op driver — no sqlmock, no
expectations, no real database:

```go
func TestCreateUser(t *testing.T) {
	rep := txnpure.NewCollectingReporter()
	det := txnpure.New(txnpure.WithReporter(rep))
	db := det.NewNullDB()
	client := &http.Client{Transport: det.WrapRoundTripper(nil)}

	err := det.InScope(context.Background(), "CreateUser", func(ctx context.Context) error {
		return NewUserService(db, client).Create(ctx, input)
	})
	if err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t) // fails with op, scope, and stack trace
}
```

## Governance

Violation identity is the pair `(Scope, Op)` — the same HTTP call may be
acceptable in one use case and a bug in another. Keep `Op.Name` and scope
names small, code-defined sets (route patterns, hosts), never raw URLs with
IDs.

- **`AllowInTransaction(reason)`** at the call site suppresses the violation
  there. If the allowed check runs outside any transaction, reporters
  implementing `StaleAllowReporter` are notified — remove the stale allow.
- **`AllowInTransactionHere(ctx, reason)`** declares the exemption **at the
  side effect itself** — the only in-code form for the detection points with
  no call site to annotate (a cross-connection write, a statement-checker
  match, a wrapped HTTP request):

  ```go
  // The cache row lives in another DB on purpose: a rollback leaves a stale
  // row that expires by TTL (TICKET-99).
  actx := txnpure.AllowInTransactionHere(ctx, "cache eviction is best-effort (TICKET-99)")
  cacheDB.ExecContext(actx, "DELETE FROM order_cache WHERE id = $1", id)
  ```

  It returns a derived context: the allow covers exactly the calls made with
  it, whatever their kind, and expires with it — wrap one call, not a block,
  and keep using the original `ctx` everywhere else. A marked call that runs
  outside any transaction reports a `StaleAllow`, like the option form.
- **`Allowlist`** is the central list: entries carry a reason and track
  usage; fail CI when `UnusedEntries()` is non-empty. Entries key on the exact
  `(scope, Op)`; `AnyScope` is a wildcard scope for bulk adoption (discouraged
  long-term).
- **`Baseline`** is the adoption ratchet: capture existing violations once
  (`BaselineFromViolations` + `Save`), commit the file, wrap your reporter
  with `NewBaselineReporter` — only new violations fail, and
  `UnusedEntries()` tells you when a baselined call site got fixed.

Precedence: `AllowInTransaction` → `AllowInTransactionHere` → `Allowlist` →
baseline filter.

### Pinning the exact call count

Every allow form (except the baseline) takes optional exact counts of the
in-transaction calls it covers per scope execution, so a reviewed exemption
cannot silently grow when the code starts calling more often (a loop, a
retry, a new call site):

```go
txnpure.AllowInTransaction("one heartbeat per order (TICKET-42)", 1)
allowlist.Add("POST /orders", op, "one heartbeat per order (TICKET-42)", 1)
txnpure.AllowInTransactionHere(ctx, "exactly one eviction (TICKET-99)", 1)
```

Calls beyond the declared count are reported as Violations immediately —
carrying `AllowedCalls` and the excess call's own stack — and a scope that
finishes with a total not among the declared counts reports a `StaleAllow`
(`RequireNoStaleAllows` fails the test), so both growth and shrinkage
surface. Several counts may be listed (`AllowInTransaction(reason, 1, 3)`)
for a path whose legitimate call count varies; `AllowInTransaction` /
`Allowlist` counts pin the per-`(scope, Op)` total, while an
`AllowInTransactionHere` count pins the region's own total across ops.

## Production monitoring

- `NewSlogReporter` logs violations through `log/slog`.
- `NewThrottlingReporter(next, interval)` deduplicates hot-path reports per
  identity (first report immediately, then at most one per interval), with
  cumulative suppressed-count snapshots for metrics.
- `WithStackDepth(0)` disables stack capture on hot paths.
- `WithScopeAttrsFunc` stamps every violation with trace/request IDs derived
  from the context (evaluated once per scope). `WithScopeAttrs` adds static
  attrs to one scope and `WithCheckAttrs` to one checkpoint; `SlogAttrs`
  bridges them to `log/slog`.

Opt-in signals (never Violations):

- `WithUnscopedTxDetection()` — transactions begun with no scope on their
  context (detached goroutines, `db.Begin()` without ctx, missing
  middleware): holes in the detection net.
- `WithUnscopedCheckDetection()` — the checkpoint-side analog: a `Check` / `Do`
  / wrapped request that ran with no scope in its context, so a mis-wired
  middleware fails a test (`RequireNoUnscopedChecks`) instead of silently
  disabling detection on that path.
- `WithLeakedTxDetection()` — a scope finished while a transaction it opened
  was still open (forgotten Commit/Rollback).
- `WithNestedScopeDetection()` — overlapping instrumentation layers.

## Performance

txnpure is meant to stay enabled everywhere — unit tests, real-database
tests, and production — so the paths that run when nothing is wrong are
engineered to be allocation-free and cheap:

| Always-on path | Cost (indicative)¹ |
| --- | --- |
| Classify a statement (`DefaultClassifier`) | ~5–12 ns, **0 allocs** |
| Driver observation per statement | ~7–13 ns, **0 allocs** |
| `Check` with a scope and no open transaction | ~4 ns, **0 allocs** |
| `Check` with no scope on the context | ~2 ns, **0 allocs** |
| Wrapped `RoundTrip` with no open transaction | ~2 ns on top of the request, **0 allocs** |
| Driver-level begin+commit (txnpure's share) | **0 allocs** |
| `StartScope` + finish | ~45 ns, 3 allocs (scope holder, context value, finish closure) |

¹ Apple M-series laptop; run `go test -bench . -benchmem` for your hardware.

Costs you pay only when something is actually reported:

- **Stack capture on a Violation** (default depth 32) is ~1.7 µs and ~2 KB
  per report, and it happens before any throttling. If a production service
  can produce sustained violations, lower `WithStackDepth` — `0` disables
  capture entirely and makes even the reporting path allocation-free
  (reporter internals aside).

Design notes that keep the hot path cheap — and what they ask of you:

- **Prepared statements are classified once at prepare time**, and statement
  checkers are matched once there too; executions reuse the results. This is
  why `Classifier` and `StatementChecker` must be pure functions of the query
  string.
- **`StatementChecker`s run per statement on the direct-exec path** (only
  consulted while relevant): keep them to a leading-keyword or substring
  test, never a full SQL parse.
- **`Check` walks the context chain** to find the scope, so its cost grows by
  roughly a nanosecond per `context.WithValue` layer between the scope and
  the checkpoint. Ordinary middleware stacks are fine; just avoid putting the
  scope hundreds of layers away from the side-effect call sites.

Regressions are gated deterministically:

- `TestHotPathAllocations` pins the exact allocation budget of every path
  above with `testing.AllocsPerRun` — deterministic, so any new allocation
  fails CI outright (it skips under `-race`; CI runs it in a separate
  no-race step).
- Timing is *not* gated in CI. Benchmarking a PR's head and base on a shared
  runner costs more than ten minutes per PR for a noisy signal, so the suite
  is a local tool instead: `make bench`, and `internal/benchgate` compares two
  runs (any `allocs/op` or `B/op` increase fails it, as does a median `ns/op`
  regression beyond +25% past a 5 ns absolute floor).

```console
go test -run '^$' -bench . -benchmem -count=8 . | tee /tmp/base.txt  # on main
go test -run '^$' -bench . -benchmem -count=8 . | tee /tmp/head.txt  # on your branch
go run ./internal/benchgate /tmp/base.txt /tmp/head.txt
```

## Blind spots (accepted, documented)

- **Detached contexts**: a side effect (or transaction) running on
  `context.Background()` carries no scope and is invisible. Mitigation:
  `WithUnscopedTxDetection` surfaces the transaction side of the hole.
- **Uninstrumented clients**: an HTTP call through a raw `http.Client` with
  no wrapped transport is invisible. There is no global hook in Go; coverage
  is a convention concern — centralize client construction.
- **MySQL implicit commits**: DDL inside a transaction implicitly commits
  it; the database-agnostic wrapper does not model this, so a side effect
  after such DDL can be a false positive.
- **Concurrent begin/commit races**: a checkpoint racing a commit in another
  goroutine may report or miss borderline cases; the verdict is best-effort
  by design.
- **Isolation levels**: out of scope, as in the whole txn- family.

## Development

The root module and each child module (`grpc/`, `examples/*`) are separate Go
modules; the children `replace` the root to keep it zero-dependency. Dev tools
(golangci-lint, goimports) live in `internal/tools/go.mod`, invoked via
`go tool -modfile=internal/tools/go.mod`. The Makefile wraps the common tasks:

```console
make fmt    # gofmt -s + goimports
make lint   # golangci-lint
make test   # go test -race, plus the no-race allocation-budget test
make bench  # the benchmark suite, local only (compare runs with internal/benchgate)
make fuzz   # the fuzz targets, 30s each (FUZZTIME=10m for a soak)
```

txnpure sits on an always-on path — every statement, every outgoing request —
so a panic in it would take down a process for a problem it only observes. The
fuzz suite (`fuzz_test.go`) therefore covers the statement classifier and
table-name extraction, arbitrary sequences of statements/transactions/
checkpoints through the driver wrapper (the scope's open-transaction counter
must never go negative, exceed the number of connections, or stay stuck high),
a `WithClassifier` returning arbitrary kinds, prepared-statement vs. direct
execution parity, the baseline file parser, and arbitrary identity strings
through the reporting pipeline. Seed corpora run as part of `go test ./...`,
and CI runs a short generative pass on every PR; a failing input is written to
`testdata/fuzz/` and committed as a regression seed.

Before committing, `gofmt -l .` must be empty, `golangci-lint run` clean, and
`go test -race ./...` green. CI enforces all three on the root, the `grpc/`
module, and the examples (which are also smoke-run) across stable/oldstable Go.

The `e2e/` module self-verifies txnpure through a real pgx v5 driver against a
throwaway PostgreSQL. `e2e/run.sh` initdbs a socket-only cluster (no Docker),
exports `TXNPURE_E2E_PG_DSN`, and runs the tests; they skip without that env
var:

```console
./e2e/run.sh -v
```

## License

[MIT](./LICENSE)

## Author

moznion (<moznion@mail.moznion.net>)
