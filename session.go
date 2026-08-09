package txnpure

import "context"

// Session is the observation surface for one database connection that does
// not go through the database/sql driver stack: a native driver (pgx, a
// ClickHouse native client, ...) or an ORM hook that exposes statement text.
// The driver middleware itself is built on it, so both paths share one
// transaction-attribution state machine: the same scope counting, the same
// cross-connection-write detection, the same statement checkers.
//
// Create exactly one Session per underlying connection and use it the way
// the connection itself must be used: serially. That mirrors the guarantee
// database/sql gives a driver.Conn; a Session has no locking of its own.
// Statements from different connections must go to different Sessions —
// connection identity is the transaction boundary (§4.11 of DESIGN.md), and
// mixing connections in one Session would merge unrelated transactions.
//
// Observe every statement the connection executes, at most once, with the
// context that carried it: the ctx of a "BEGIN" is what attributes the
// transaction to a scope, and the ctx of a write is what the cross-connection
// verdict consults. Transaction control that the driver executes as statement
// text ("BEGIN"/"COMMIT"/"ROLLBACK" — pgx's Begin()/Commit() do exactly that)
// is tracked from the text alone; BeginTx/EndTx exist for transaction
// transitions that never surface as text.
type Session struct {
	det *Detector
	// inTx / txScope are the same fields a wrappedConn tracked before the
	// state machine moved here: whether this connection is inside a
	// transaction, and the scope that transaction is attributed to (nil =
	// unscoped). Single-goroutine use is the caller's contract; the scope's
	// openTxs counter is the shared/atomic one.
	inTx    bool
	txScope *scope
}

// NewSession creates a Session bound to the Detector. One Session per
// connection; see the type comment for the contract.
func (d *Detector) NewSession() *Session { return &Session{det: d} }

// Observe records one executed statement: textual transaction control
// ("BEGIN"/"COMMIT"/"ROLLBACK") updates the connection's transaction state, a
// write is checked against other connections' open transactions in the ctx
// scope (§4.11), and registered statement checkers run — exactly what the
// driver middleware does per statement.
func (s *Session) Observe(ctx context.Context, query string) {
	s.observeKind(ctx, query, s.det.classify(query))
	// User-declared external calls are checked against every open transaction
	// in the scope (including this connection's own), independently of the
	// tx-lifecycle/write classification above. The len check is hoisted here
	// so the common no-checker case pays no function call per statement.
	if len(s.det.stmtCheckers) != 0 {
		s.det.runStatementCheckers(ctx, query)
	}
}

// observeKind is Observe minus the statement checkers, with the
// classification already known — the prepared-statement path classifies once
// at prepare time and reuses the result for every execution.
func (s *Session) observeKind(ctx context.Context, query string, kind StatementKind) {
	switch kind {
	case KindBegin:
		if !s.inTx {
			s.openTx(ctx)
		}
	case KindCommit, KindRollback:
		s.closeTx()
	case KindWrite:
		s.reportIfCrossConn(ctx, query)
	case KindOther:
	}
}

// BeginTx marks a transaction start that does not surface as statement text.
// Two callers need it: driver-API-level transactions (database/sql's
// ConnBeginTx, mirrored by the driver middleware), and protocol-level
// implicit transactions — a pgx batch is pipelined up to a single Sync, so
// PostgreSQL runs it as one implicit transaction even though no BEGIN is ever
// written. The ctx decides which scope the transaction is attributed to, so
// pass the one that carries the request's scope.
//
// If a textual BEGIN already opened a transaction on this connection, that
// transaction is closed first so the scope counter cannot get stuck high (a
// permanent false positive poisons every later checkpoint in the scope).
// That close-then-open is meant for driver-level begins, which genuinely
// replace the textual transaction; do not bracket a batch that runs inside an
// explicit transaction (it belongs to that transaction — check the
// connection's tx status first, as the README's pgx tracer does).
func (s *Session) BeginTx(ctx context.Context) {
	s.closeTx()
	s.openTx(ctx)
}

// EndTx marks the end of a transaction started by BeginTx — commit and
// rollback alike. It is idempotent, so integrations should also call it when
// the connection is torn down while a transaction may still be open (a
// dropped connection rolls back server-side; the counter must follow).
func (s *Session) EndTx() { s.closeTx() }

// openTx marks the connection as inside a transaction attributed to the ctx
// scope (incrementing its open-transaction counter), or reports an unscoped
// transaction when the ctx carries no scope.
func (s *Session) openTx(ctx context.Context) {
	s.inTx = true
	s.txScope = scopeFrom(ctx)
	if s.txScope != nil {
		s.txScope.openTxs.Add(1)
	} else {
		s.det.reportUnscopedTx(ctx)
	}
}

// closeTx ends the connection's transaction, decrementing the scope counter
// at most once (inTx guard): a textual COMMIT inside a driver-level tx,
// double closes, etc. must not drive the counter negative.
func (s *Session) closeTx() {
	if !s.inTx {
		return
	}
	s.inTx = false
	if s.txScope != nil {
		s.txScope.openTxs.Add(-1)
		s.txScope = nil
	}
}

// reportIfCrossConn reports a cross-connection-write Violation when a write on
// this connection runs alongside a transaction open on another connection in
// the ctx scope. Each connection is its own transaction boundary, so a write
// outside the connection that holds a transaction cannot be rolled back with
// it. The connection's own open transaction is excluded (a write inside its
// own transaction is normal); only *other* connections' transactions count.
func (s *Session) reportIfCrossConn(ctx context.Context, query string) {
	sc, mark := scopeAndMarkFrom(ctx)
	if sc == nil {
		return
	}
	total := sc.openTxs.Load()
	var self int64
	if s.inTx && s.txScope == sc {
		self = 1
	}
	foreign := total - self
	if foreign <= 0 {
		// A marked write with no transaction open at all is a stale allow
		// (§4.14 uniform rule). A write inside its own connection's
		// transaction stays silent — that execution is normal, not evidence
		// the mark is stale.
		if total == 0 && mark != nil {
			s.det.reportStaleAllow(ctx, StaleAllow{Scope: sc.name, Op: Op{Kind: "db", Name: crossConnOpName(query)}, Reason: mark.reason})
		}
		return
	}
	s.det.decideAndReport(ctx, sc, mark, Op{Kind: "db", Name: crossConnOpName(query)}, int(foreign), checkConfig{})
}
