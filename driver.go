package txnpure

import (
	"context"
	"database/sql/driver"
	"errors"
)

// Wrap wraps a database/sql driver so that every transaction begun through it
// is tracked by the Detector. Register the result with sql.Register:
//
//	sql.Register("pgx-txnpure", detector.Wrap(stdlib.GetDefaultDriver()))
//	db, err := sql.Open("pgx-txnpure", dsn)
func (d *Detector) Wrap(drv driver.Driver) driver.Driver {
	return &wrappedDriver{det: d, drv: drv}
}

// WrapConnector wraps a driver.Connector for use with sql.OpenDB.
func (d *Detector) WrapConnector(c driver.Connector) driver.Connector {
	return &wrappedConnector{det: d, connector: c}
}

type wrappedDriver struct {
	det *Detector
	drv driver.Driver
}

func (w *wrappedDriver) Open(name string) (driver.Conn, error) {
	conn, err := w.drv.Open(name)
	if err != nil {
		return nil, err
	}
	return &wrappedConn{det: w.det, conn: conn}, nil
}

type wrappedConnector struct {
	det       *Detector
	connector driver.Connector
}

func (w *wrappedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := w.connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &wrappedConn{det: w.det, conn: conn}, nil
}

func (w *wrappedConnector) Driver() driver.Driver {
	return &wrappedDriver{det: w.det, drv: w.connector.Driver()}
}

// wrappedConn tracks the transaction lifecycle of one connection and
// attributes each transaction to the scope carried by the ctx it was begun
// with. database/sql guarantees a driver.Conn is used by a single goroutine
// at a time, so txID/txScope need no locking; the scope counter is the
// shared/atomic one.
type wrappedConn struct {
	det     *Detector
	conn    driver.Conn
	txID    uint64 // non-zero while inside a transaction
	txScope *scope // scope the open transaction is attributed to (nil = unscoped)
}

var (
	_ driver.Conn               = (*wrappedConn)(nil)
	_ driver.ConnPrepareContext = (*wrappedConn)(nil)
	_ driver.ConnBeginTx        = (*wrappedConn)(nil)
	_ driver.ExecerContext      = (*wrappedConn)(nil)
	_ driver.QueryerContext     = (*wrappedConn)(nil)
	_ driver.Pinger             = (*wrappedConn)(nil)
	_ driver.SessionResetter    = (*wrappedConn)(nil)
	_ driver.Validator          = (*wrappedConn)(nil)
	_ driver.NamedValueChecker  = (*wrappedConn)(nil)
)

// openTx marks the connection as inside a transaction attributed to the ctx
// scope (incrementing its open-transaction counter), or reports an unscoped
// transaction when the ctx carries no scope.
func (c *wrappedConn) openTx(ctx context.Context) {
	c.txID = c.det.nextTxID()
	c.txScope = scopeFrom(ctx)
	if c.txScope != nil {
		c.txScope.openTxs.Add(1)
	} else {
		c.det.reportUnscopedTx(ctx)
	}
}

// closeTx ends the connection's transaction, decrementing the scope counter
// at most once (txID == 0 guard): a textual COMMIT inside a driver-level tx,
// double closes, etc. must not drive the counter negative.
func (c *wrappedConn) closeTx() {
	if c.txID == 0 {
		return
	}
	c.txID = 0
	if c.txScope != nil {
		c.txScope.openTxs.Add(-1)
		c.txScope = nil
	}
}

// observe updates textual transaction state (raw "BEGIN"/"COMMIT" executed as
// plain statements) as a best effort, and reports a Violation when a write is
// issued on this connection while a transaction opened on a *different*
// connection in the same scope is still open (§4.11 of DESIGN.md).
func (c *wrappedConn) observe(ctx context.Context, query string) {
	switch c.det.classify(query) {
	case KindBegin:
		if c.txID == 0 {
			c.openTx(ctx)
		}
	case KindCommit, KindRollback:
		c.closeTx()
	case KindWrite:
		c.reportIfCrossConn(ctx, query)
	case KindOther:
	}
	// User-declared external calls are checked against every open transaction
	// in the scope (including this connection's own), independently of the
	// tx-lifecycle/write classification above.
	c.det.runStatementCheckers(ctx, query)
}

// reportIfCrossConn reports a cross-connection-write Violation when a write on
// this connection runs alongside a transaction open on another connection in
// the ctx scope. Each connection is its own transaction boundary, so a write
// outside the connection that holds a transaction cannot be rolled back with
// it. The connection's own open transaction is excluded (a write inside its
// own transaction is normal); only *other* connections' transactions count.
func (c *wrappedConn) reportIfCrossConn(ctx context.Context, query string) {
	s := scopeFrom(ctx)
	if s == nil {
		return
	}
	var self int64
	if c.txID != 0 && c.txScope == s {
		self = 1
	}
	foreign := s.openTxs.Load() - self
	if foreign <= 0 {
		return
	}
	name := writeTarget(query)
	if name == "" {
		name = "write"
	}
	c.det.emitViolation(ctx, s, Op{Kind: "db", Name: name}, int(foreign), nil)
}

func (c *wrappedConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &wrappedStmt{conn: c, stmt: stmt, query: query}, nil
}

func (c *wrappedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if pc, ok := c.conn.(driver.ConnPrepareContext); ok {
		stmt, err := pc.PrepareContext(ctx, query)
		if err != nil {
			return nil, err
		}
		return &wrappedStmt{conn: c, stmt: stmt, query: query}, nil
	}
	return c.Prepare(query)
}

func (c *wrappedConn) Close() error { return c.conn.Close() }

// Begin is the legacy no-context begin: there is no ctx to read a scope from,
// so the transaction is necessarily unscoped.
func (c *wrappedConn) Begin() (driver.Tx, error) {
	tx, err := c.conn.Begin() //nolint:staticcheck // legacy interface must be supported
	if err != nil {
		return nil, err
	}
	c.beginDriverTx(context.Background())
	return &wrappedTx{conn: c, tx: tx}, nil
}

func (c *wrappedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	var tx driver.Tx
	var err error
	if bt, ok := c.conn.(driver.ConnBeginTx); ok {
		tx, err = bt.BeginTx(ctx, opts)
	} else {
		if opts != (driver.TxOptions{}) {
			return nil, errors.New("txnpure: underlying driver does not support non-default transaction options")
		}
		tx, err = c.conn.Begin() //nolint:staticcheck // legacy interface fallback
	}
	if err != nil {
		return nil, err
	}
	c.beginDriverTx(ctx)
	return &wrappedTx{conn: c, tx: tx}, nil
}

// beginDriverTx opens the driver-level transaction. If a textual BEGIN
// already opened one on this conn, close it first so the scope counter does
// not get stuck high (a permanent false positive is worse than a missed
// borderline case).
func (c *wrappedConn) beginDriverTx(ctx context.Context) {
	c.closeTx()
	c.openTx(ctx)
}

func (c *wrappedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	res, err := c.execUnderlying(ctx, query, args)
	if errors.Is(err, driver.ErrSkip) {
		// database/sql falls back to the prepared-statement path, which is
		// also wrapped; observing here would double-count.
		return nil, driver.ErrSkip
	}
	if errors.Is(err, driver.ErrBadConn) {
		// database/sql retries on a fresh connection; the retry will be
		// observed, so observing here would double-count.
		return nil, err
	}
	c.observe(ctx, query)
	return res, err
}

func (c *wrappedConn) execUnderlying(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if ec, ok := c.conn.(driver.ExecerContext); ok {
		return ec.ExecContext(ctx, query, args)
	}
	if e, ok := c.conn.(driver.Execer); ok { //nolint:staticcheck // legacy interface fallback
		vals, err := namedValuesToValues(args)
		if err != nil {
			return nil, err
		}
		return e.Exec(query, vals)
	}
	return nil, driver.ErrSkip
}

func (c *wrappedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.queryUnderlying(ctx, query, args)
	if errors.Is(err, driver.ErrSkip) {
		return nil, driver.ErrSkip
	}
	if errors.Is(err, driver.ErrBadConn) {
		return nil, err
	}
	c.observe(ctx, query)
	return rows, err
}

func (c *wrappedConn) queryUnderlying(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if qc, ok := c.conn.(driver.QueryerContext); ok {
		return qc.QueryContext(ctx, query, args)
	}
	if q, ok := c.conn.(driver.Queryer); ok { //nolint:staticcheck // legacy interface fallback
		vals, err := namedValuesToValues(args)
		if err != nil {
			return nil, err
		}
		return q.Query(query, vals)
	}
	return nil, driver.ErrSkip
}

func (c *wrappedConn) Ping(ctx context.Context) error {
	if p, ok := c.conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *wrappedConn) ResetSession(ctx context.Context) error {
	if sr, ok := c.conn.(driver.SessionResetter); ok {
		return sr.ResetSession(ctx)
	}
	return nil
}

func (c *wrappedConn) IsValid() bool {
	if v, ok := c.conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *wrappedConn) CheckNamedValue(nv *driver.NamedValue) error {
	if nvc, ok := c.conn.(driver.NamedValueChecker); ok {
		return nvc.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

type wrappedTx struct {
	conn *wrappedConn
	tx   driver.Tx
}

// Commit closes the transaction even when the underlying Commit errors:
// database/sql discards the connection either way, and leaving the counter
// stuck high would poison every later checkpoint in the scope.
func (t *wrappedTx) Commit() error {
	err := t.tx.Commit()
	t.conn.closeTx()
	return err
}

func (t *wrappedTx) Rollback() error {
	err := t.tx.Rollback()
	t.conn.closeTx()
	return err
}

type wrappedStmt struct {
	conn  *wrappedConn
	stmt  driver.Stmt
	query string
}

var (
	_ driver.Stmt              = (*wrappedStmt)(nil)
	_ driver.StmtExecContext   = (*wrappedStmt)(nil)
	_ driver.StmtQueryContext  = (*wrappedStmt)(nil)
	_ driver.NamedValueChecker = (*wrappedStmt)(nil)
)

func (s *wrappedStmt) Close() error  { return s.stmt.Close() }
func (s *wrappedStmt) NumInput() int { return s.stmt.NumInput() }

func (s *wrappedStmt) Exec(args []driver.Value) (driver.Result, error) {
	res, err := s.stmt.Exec(args) //nolint:staticcheck // legacy interface must be supported
	if errors.Is(err, driver.ErrBadConn) {
		return nil, err
	}
	s.conn.observe(context.Background(), s.query)
	return res, err
}

func (s *wrappedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	res, err := s.execUnderlying(ctx, args)
	if errors.Is(err, driver.ErrBadConn) {
		return nil, err
	}
	s.conn.observe(ctx, s.query)
	return res, err
}

func (s *wrappedStmt) execUnderlying(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if sec, ok := s.stmt.(driver.StmtExecContext); ok {
		return sec.ExecContext(ctx, args)
	}
	vals, err := namedValuesToValues(args)
	if err != nil {
		return nil, err
	}
	return s.stmt.Exec(vals) //nolint:staticcheck // legacy interface fallback
}

func (s *wrappedStmt) Query(args []driver.Value) (driver.Rows, error) {
	rows, err := s.stmt.Query(args) //nolint:staticcheck // legacy interface must be supported
	if errors.Is(err, driver.ErrBadConn) {
		return nil, err
	}
	s.conn.observe(context.Background(), s.query)
	return rows, err
}

func (s *wrappedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := s.queryUnderlying(ctx, args)
	if errors.Is(err, driver.ErrBadConn) {
		return nil, err
	}
	s.conn.observe(ctx, s.query)
	return rows, err
}

func (s *wrappedStmt) queryUnderlying(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if sqc, ok := s.stmt.(driver.StmtQueryContext); ok {
		return sqc.QueryContext(ctx, args)
	}
	vals, err := namedValuesToValues(args)
	if err != nil {
		return nil, err
	}
	return s.stmt.Query(vals) //nolint:staticcheck // legacy interface fallback
}

func (s *wrappedStmt) CheckNamedValue(nv *driver.NamedValue) error {
	if nvc, ok := s.stmt.(driver.NamedValueChecker); ok {
		return nvc.CheckNamedValue(nv)
	}
	if nvc, ok := s.conn.conn.(driver.NamedValueChecker); ok {
		return nvc.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

func namedValuesToValues(named []driver.NamedValue) ([]driver.Value, error) {
	vals := make([]driver.Value, len(named))
	for i, nv := range named {
		if nv.Name != "" {
			return nil, errors.New("txnpure: named parameters are not supported by the underlying driver")
		}
		vals[i] = nv.Value
	}
	return vals, nil
}
