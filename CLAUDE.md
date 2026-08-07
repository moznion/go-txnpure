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
  free-form). Matchers run on the hot path — keep them cheap. Classifier and
  StatementChecker must be pure functions of the query: the prepared-statement
  path evaluates both once at prepare time and reuses the results per
  execution (`wrappedStmt.kind` / `wrappedStmt.stmtOps`).
- **No panicking reporter**: test-failure ergonomics are `Require*`
  assertions + stack traces in violations (`stack.go`, default depth 32,
  `WithStackDepth(0)` disables). Stack capture skips leading txnpure-internal
  frames but never `_test.go` frames.
- **Allow precedence**: `AllowInTransaction` (call site) →
  `AllowInTransactionHere` (ctx region, §4.14) → `Allowlist` (central) →
  `Baseline` (wrapper reporter, filters last by construction). All carry rot
  prevention: `StaleAllowReporter` fires when an allowed check runs outside
  any transaction; `UnusedEntries()` on Allowlist and Baseline is meant to
  fail CI when stale.
- **`AllowInTransactionHere(ctx, reason, exactCalls...)`** (§4.14) is a
  **lexical region**, not a scope/tx mark: it derives a ctx and covers
  exactly the events observed under it, every op kind — the only in-code
  hatch for the detection points with no call site (cross-conn writes,
  statement-checker matches, RoundTripper). It piggybacks on `scopeCtxKey`
  (`*scope` | `*allowedScope`, resolved by `scopeAndMarkFrom` in ONE
  ctx.Value walk — never add a second key). No scope → no-op; inner mark
  shadows outer; a nested `StartScope` does not inherit it. Do NOT turn it
  into a scope-wide mark: that would suppress every op in the scope and
  break `(Scope, Op)` identity (deliberate divergence from txnproof #14,
  DESIGN.md log #6).
- **Exact call counts** (§4.13, txnproof #12 analog): every allow form takes
  variadic `exactCalls` pinning the number of in-transaction calls covered
  per scope execution — NOT nesting depth (rejected; log #5). Option/
  Allowlist counts pin the per-identity total (`scope.calls`, lazy map,
  hazard path only — `Violation.Call` is the 1-based index);  a Here region
  pins its own event total (`allowMark.n`). Immediate-verdict split: calls
  ≤ max(declared) are suppressed as they arrive, the excess call violates
  immediately (`Violation.AllowedCalls` carries the declining declaration),
  and exactness is verified at `finishScope` — mismatch → `StaleAllow` with
  `Calls`/`AllowedCalls`, never a retroactive Violation. Shared rule:
  `coversCalls`; keep all three mechanisms deciding identically
  (`TestAllowFormsDecideIdentically` pins it).

## Driver-wrapper correctness notes

Ported from txnproof; these prevent double counting — keep them intact when
touching `driver.go`:

- **The tx state machine lives in `Session` (`session.go`), not in
  `wrappedConn`.** `wrappedConn` holds a `session Session` and delegates
  (`Observe`/`observeKind`/`BeginTx`/`EndTx`); native-driver integrations
  (pgx tracers, ...) drive the same exported surface directly. A behavior
  change in the driver path and in Session is the same change — keep them
  from diverging (`TestSessionAndDriverPathsAgree` pins the wiring,
  `FuzzSessionSequence` the counter invariants).
- If the underlying conn lacks `ExecerContext`/`Execer`, return
  `driver.ErrSkip` **without observing** — database/sql falls back to the
  prepared-statement path, which is also wrapped and will observe.
- Do not observe on `driver.ErrBadConn` — database/sql retries on a fresh
  conn and the retry observes.
- `Session.inTx`/`txScope` need no locking (database/sql guarantees single
  goroutine per driver.Conn, and Session documents the same serial-use
  contract for native callers); the scope's `openTxs` counter is the
  shared/atomic one.
- **Closing is idempotent per conn** (`inTx` guard): a textual `COMMIT`
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

## Performance guardrails

The always-on hot paths (classifier, driver observe, checkpoint fast path)
must stay allocation-free; StartScope has a fixed 3-alloc budget. **Only the
deterministic gate runs in CI** — when a change trips it, fix the regression
rather than loosening the gate (loosen only with justification in the PR):

- `TestHotPathAllocations` pins exact allocs/op via `testing.AllocsPerRun`.
  It self-skips under `-race` (the race runtime allocates), so CI runs it in
  a separate no-race step; `make test` runs both.
- **Timing is deliberately not gated in CI.** A `benchmark` job that benched
  head and base back to back existed and was removed: the suite takes >5 min
  per side, so it doubled PR latency for a ns/op signal that is noisy on
  shared runners anyway. Do not reintroduce it; extend
  `TestHotPathAllocations` instead when a new path needs a guardrail.
- `bench_test.go` is the suite (`make bench`), a local tool; cover any new hot
  path there, and mind `Classifier`/`StatementChecker` purity — prepared
  statements evaluate both once at prepare time. `internal/benchgate`
  (root-module package, stdlib only) compares two local runs: any allocs/op or
  B/op increase fails; median ns/op beyond +25% past a 5ns absolute floor
  fails; head-only benchmarks are informational.

## Fuzzing (crash safety)

txnpure runs on the always-on path of an application, so a panic in it takes
the host process down for a problem txnpure only *observes*: crash-freedom on
arbitrary input outranks every other property. `fuzz_test.go` (package
`txnpure`, so targets can read `scope.openTxs`) is the suite; `make fuzz`
drives every target one at a time (`FUZZTIME=10m` for a soak), and the `fuzz`
CI job runs a short generative pass per PR. Seed corpora replay in the normal
`go test ./...`, so committed inputs are permanent regression tests.

- Targets and the invariants they pin: `FuzzWriteTarget` (determinism, bounded
  op name), `FuzzDriverSequence` (an arbitrary op/statement/transaction stream:
  `openTxs` stays in `[0, conns]` at every step and drains to 0),
  `FuzzAdversarialClassifier` (same, with a `WithClassifier` returning
  arbitrary — including out-of-range — kinds; a user escape hatch must not be
  able to poison the counter), `FuzzPreparedStatementParity` (the
  prepare-time caching of classification + checker matches must report exactly
  what the direct path reports), `FuzzLoadBaseline` (hand-edited file: error,
  never panic; stable Save/Load round trip), `FuzzViolationPipeline` (arbitrary
  identity strings through allowlist → baseline → throttling → rendering, with
  the suppression contract — including exact-call counts and
  AllowInTransactionHere regions, modeled independently — intact), plus
  `FuzzDefaultClassifierMatchesReference` in `classify_internal_test.go`
  (differential against `referenceClassifier`, its readable specification).
- **A failing input is committed**, not just fixed: `go test -fuzz` writes it
  to `testdata/fuzz/<Target>/<hash>`; the CI job prints it on failure so the
  reproducer survives the runner.
- Helpers the targets install (`adversarialClassifier`,
  `fuzzStatementChecker`) must stay **pure functions of the query** like the
  real hooks, or the prepared-statement parity target compares two different
  things. Bound the work per input (the op stream is truncated) so fuzzing
  stays throughput-bound.

## Roadmap state

M0–M4 of DESIGN.md §6 are implemented (core, governance, grpc, examples,
e2e, doc.go, allow hatches §4.13–4.14). Remaining: version tags
(`v0.1.0` / `v0.2.0`).

## Releasing

Tagging and GitHub Releases are automated by [tagpr](https://github.com/Songmu/tagpr)
(`.tagpr`, `.github/workflows/tagpr.yml`). A push to `main` opens/updates a
release PR; merging it tags `vX.Y.Z` and cuts the release. `version.go`'s
`Version` constant is tagpr's version file — never edit it by hand. The
workflow needs a `TAGPR_TOKEN` secret (PAT or GitHub App token); with the
default `GITHUB_TOKEN` the release PR would not trigger the `check` workflow.
Child modules (`grpc/`, `e2e/`, `examples/*`) are not versioned by tagpr —
tag those manually if they ever need releasing.
