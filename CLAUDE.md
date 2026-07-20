# Guidelines for go-txnpure

## What this is

A detector for **side effects executed while a database transaction is open**:
HTTP calls, message/job enqueues, mail sending — anything that cannot be
rolled back when the surrounding transaction aborts. It combines a
`database/sql` driver middleware (transaction-lifecycle tracking) with
explicit checkpoints in side-effect clients (`Check`/`Do`/`WrapRoundTripper`).
One core mechanism serves three modes: unit tests (`NewNullDB`),
real-database tests (wrap the real driver), and production monitoring
(pluggable `Reporter`s). Sibling of go-txnproof (inverse failure mode);
see DESIGN.md for the full design log.

## Hard constraints

- **Zero dependencies outside the Go standard library.** Do not add any.
  Dev tools (golangci-lint, goimports) live in the separate
  `internal/tools/go.mod` module, invoked via
  `go tool -modfile=internal/tools/go.mod` (see Makefile). Never add `tool`
  directives or requires to the root `go.mod`: they bump its go directive
  and leak requirements into every child module that `replace`s the root,
  breaking their builds (this happened in txnproof once).
- Before committing: `gofmt -l .` must be empty, `golangci-lint run ./...`
  clean, `go test -race -count=1 ./...` green. CI enforces all three on
  stable/oldstable.

## Core semantics (deliberate decisions — do not change casually)

- **A violation is "a checkpoint fired while ≥1 transaction opened under the
  same scope is still open."** The check runs before the side effect; whether
  it later succeeds is irrelevant. An *empty* open transaction still trips
  checkpoints (the hazard is structural). The side effect is **never blocked
  or delayed** — txnpure observes and reports, tests assert.
- **Cross-connection writes are a violation too, default on** (§4.11): a write
  on one connection while *another* connection's transaction is open in the
  same scope. Connection = transaction boundary (database/sql binds a tx to
  one conn), so `wrappedConn` identity is the boundary — no DB tagging. Verdict
  `foreign = scope.openTxs − self`, where `self` is this conn's own open tx;
  `foreign > 0` → Violation `(scope, {Kind:"db", Name:<table>})`. Writes only
  (reads have nothing to roll back). This is the answer to "destructive change
  to DB B inside DB A's tx" — two DBs are two connections. Suppress via
  Allowlist/Baseline keyed on the table Op. Do NOT make it opt-in: it is a
  true core Violation, decided deliberately.
- **Scope attribution, not connection attribution**: the open-tx counter
  lives on the `scope` holder carried by context. A transaction is attributed
  to the scope on the ctx passed to `BeginTx` (or the textual `BEGIN`'s ctx).
  Pooled connections therefore never cause cross-request false positives; a
  goroutine sharing the ctx that observes an open counter is a true positive.
- **Violation identity is the pair `(Scope, Op)`** — it keys the Allowlist,
  the Baseline, and throttling. `Op.Name` and scope names must stay small,
  code-defined sets (route patterns, hosts) — never raw URLs with IDs.
- **Nested scopes shadow** (innermost = most specific attribution), same
  contract as txnproof. `WithNestedScopeDetection` makes nesting observable,
  never a Violation. A `Check` with no scope in ctx is **silent** (§4.5 of
  DESIGN.md); the transaction-side analog is `WithUnscopedTxDetection`
  (reported as `UnscopedTx`, never a Violation).
- **Leaked transactions** (`WithLeakedTxDetection`): a scope finishing with
  counter > 0 reports `LeakedTx`, never a Violation.
- **Unscoped checkpoints** (`WithUnscopedCheckDetection`, §4.5): a Check/Do/
  RoundTripper running with no scope in ctx reports `UnscopedCheck`, never a
  Violation — the checkpoint-side analog of `WithUnscopedTxDetection`, so a
  mis-wired scope middleware fails a test via `RequireNoUnscopedChecks` instead
  of silently disabling detection. Statement-checker matches are NOT counted
  here (scopeless statements are the tx side's concern).
- **User-declared external calls** (`WithStatementChecker`, §4.12): a
  `StatementChecker func(query string) (Op, bool)` is the pluggable, third
  external-call detection point (alongside HTTP RoundTripper and cross-conn
  writes). Runs on the driver statement path; matched statements are checked
  against the **full** `scope.openTxs` (including their own tx, unlike §4.11's
  `foreign`), because an external effect embedded in a statement is not
  rollback-safe even inside its own tx. Non-SQL clients have no Go hook →
  `Check`/`Do` adapters remain their extension point (Op is already
  free-form). Matchers run on the hot path — keep them cheap.
- **No panicking reporter**: test-failure ergonomics are `Require*`
  assertions + stack traces in violations (`stack.go`, default depth 32,
  `WithStackDepth(0)` disables). Stack capture skips leading txnpure-internal
  frames but never `_test.go` frames.
- **Allow precedence**: `AllowInTransaction` (call site) → `Allowlist`
  (central) → `Baseline` (wrapper reporter, filters last by construction).
  All three carry rot prevention: `StaleAllowReporter` fires when an allowed
  check runs outside any transaction; `UnusedEntries()` on Allowlist and
  Baseline is meant to fail CI when stale.

## Driver-wrapper correctness notes

Ported from txnproof; these prevent double counting — keep them intact when
touching `driver.go`:

- If the underlying conn lacks `ExecerContext`/`Execer`, return
  `driver.ErrSkip` **without observing** — database/sql falls back to the
  prepared-statement path, which is also wrapped and will observe.
- Do not observe on `driver.ErrBadConn` — database/sql retries on a fresh
  conn and the retry observes.
- `wrappedConn.txID`/`txScope` need no locking (database/sql guarantees
  single goroutine per driver.Conn); the scope's `openTxs` counter is the
  shared/atomic one.
- **Closing is idempotent per conn** (`txID == 0` guard): a textual `COMMIT`
  inside a driver-level tx, double closes, etc. decrement the scope counter
  at most once. `Commit`/`Rollback` errors still close — a counter stuck
  high poisons every later checkpoint in the scope (a permanent false
  positive is worse than a missed borderline case).
- Textual `BEGIN`/`COMMIT`/`ROLLBACK` executed as plain statements update
  the conn tx state (best effort); `ROLLBACK TO SAVEPOINT` / `SAVEPOINT` /
  `RELEASE SAVEPOINT` must **not** end the tx (regression-tested).
- A driver-level begin while a textual tx is open closes the old tx first so
  the counter cannot get stuck high.
- **MySQL implicit commits are deliberately not modeled** (DDL inside a tx):
  the wrapper is database-agnostic; a side effect after such DDL can be a
  false positive. Documented, same stance as txnproof.

## Classification (`classify.go`)

Tx-lifecycle plus a single **write/not-write bit** (for §4.11
cross-connection-write detection) — no finer taxonomy (that is txnproof's
job). Leading keyword →
`KindBegin | KindCommit | KindRollback | KindWrite | KindOther`, with
case/whitespace/comment tolerance, the savepoint exceptions, and the WITH-CTE
write scan. `writeTarget` extracts a best-effort table name for the Op
identity. Escape hatch: `WithClassifier`.

## Reporters

- **When adding a new optional reporter interface** (the pattern behind
  `UnscopedTxReporter` / `NestedScopeReporter` / `LeakedTxReporter` /
  `StaleAllowReporter`): the wrapper reporters (`ThrottlingReporter`,
  `BaselineReporter`) forward only the interfaces they themselves implement,
  so every new interface MUST also be implemented/forwarded there — the
  forwarding matrix is regression-tested in
  `TestWrapperReportersForwardAllOptionalInterfaces`.
- `ThrottlingReporter`: per-identity first-report-then-suppress with
  cumulative suppressed-count snapshots (not callbacks), injectable `now`
  (in-package test), and a capped **fail-open** key map for unscoped txs
  (past the cap: forwarded unthrottled — never lose reports, only dedup).
- Baseline file discipline: deterministic sorted JSON keyed on
  `(scope, kind, name)`, explicit `Save`, `LoadBaseline` errors on a missing
  file. No counts, stacks, or timestamps in the file.

## Child modules (own go.mod, `replace` root, keep root zero-dep)

- `grpc/` — unary + stream client interceptors (`UnaryClientInterceptor` /
  `StreamClientInterceptor`), Op `{Kind:"grpc", Name:"/pkg.Service/Method"}`.
  Depends on google.golang.org/grpc; tested with a stub invoker/streamer (no
  real server). `isInternalFrame` skips child-module frames too, so stacks
  point at the caller, not the interceptor.
- `examples/nethttp/`, `examples/enqueue/` — runnable, self-terminating demos;
  CI builds, lints, and **smoke-runs** them (`go run .` must print the
  violation message `side effect inside an open transaction`). `nethttp`
  derives scope names via `mux.Handler(r)`, NOT `r.Pattern` (empty in an outer
  middleware — a txnproof lesson).
- Lint a child module with the root tools modfile via an absolute path:
  `go tool -modfile=<repo>/internal/tools/go.mod golangci-lint run`
  (the relative depth differs per module).

## e2e (`e2e/` module)

Self-verifies txnpure through a **real pgx v5 driver** against a throwaway
PostgreSQL — no server-log cross-check (unlike txnproof; the property is
client-side timing). Scenarios: real driver-level tx / rollback, textual
BEGIN/COMMIT, ROLLBACK TO SAVEPOINT keeps the tx open, pool isolation,
cross-connection write (real INSERT → `{db, <table>}`), prepared-statement
cross-connection, RoundTripper-in-tx, and unscoped-tx. Tests skip unless
`TXNPURE_E2E_PG_DSN` is set; `e2e/run.sh` initdbs a socket-only cluster (no
Docker) and exports it. DDL setup runs with no scope so it never trips a
checkpoint. CI uses one PostgreSQL major (no version-specific behavior).

## Roadmap state

M0–M3 of DESIGN.md §6 are implemented (core, governance, grpc, examples,
e2e, doc.go). Remaining: version tags (`v0.1.0` / `v0.2.0`).
