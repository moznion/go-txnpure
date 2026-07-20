// Package txnpure detects side effects executed while a database transaction
// is open: HTTP calls, message/job enqueues, mail sending, cross-database
// writes — anything that cannot be rolled back when the surrounding
// transaction aborts, and anything that can observe (or be observed by) state
// the transaction has not committed yet.
//
// It is the sibling of go-txnproof and covers the inverse failure mode:
// txnproof detects writes that are not wrapped in one transaction (atomicity),
// txnpure detects side effects that run inside one (purity). The idea is
// borrowed from the Ruby gem palkan/isolator, adapted to Go's
// dependency-injection idioms — detection points are explicit instrumentation,
// not global monkey-patching.
//
// # Mechanism
//
// Three cooperating parts, glued by a mutable holder carried on a
// context.Context:
//
//   - Scope: StartScope (or InScope) installs a *scope holder on the context,
//     marking a logical execution (a request, a use case, a job). Install it
//     in middleware so every code path is covered. The scope name is half of
//     the violation identity.
//   - Driver wrapper: Wrap / WrapConnector intercept database/sql, incrementing
//     the scope's open-transaction counter on BeginTx (and textual BEGIN) and
//     decrementing it on Commit/Rollback. This works with anything on top of
//     database/sql — sqlx, GORM, ent, pgx via stdlib — with no per-ORM adapter.
//   - Checkpoints: instrumented side-effect clients consult the scope. If a
//     transaction is open, a Violation is reported immediately, with a stack
//     trace. The side effect itself is never blocked — txnpure observes and
//     reports; tests assert.
//
// # Detection points
//
//   - Explicit checkpoints: Check and Do, with any Op.
//   - HTTP: WrapRoundTripper makes every outbound request a checkpoint.
//   - Cross-connection writes (on by default): a write on one connection while
//     another connection's transaction is open in the same scope — the
//     multi-database case, detected at the driver with no extra instrumentation.
//   - User-declared statements: WithStatementChecker treats matching SQL as an
//     external call.
//
// The gRPC client interceptors live in the github.com/moznion/go-txnpure/grpc
// submodule so the root module stays free of the grpc dependency.
//
// # Three modes
//
// One detector serves pure unit tests (via NewNullDB, a no-op driver),
// tests against a real database (wrap the real driver), and continuous
// production monitoring (pluggable Reporters; the per-checkpoint overhead is a
// context lookup and an atomic load). Governance — AllowInTransaction, an
// Allowlist, and a Baseline ratchet, all keyed on the (Scope, Op) identity —
// keeps known and intentional cases from failing CI.
//
// See the package README and DESIGN.md for the full semantics, blind spots,
// and design rationale.
package txnpure
