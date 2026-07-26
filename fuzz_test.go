package txnpure

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Fuzz suite: txnpure sits on the always-on path of an application (every
// statement, every outgoing request), so a panic here takes production down
// for a problem txnpure only observes. These targets therefore assert
// crash-freedom first, plus the invariants whose violation would poison
// detection: the scope's open-transaction counter must never go negative or
// get stuck high, and the prepared-statement fast path must observe exactly
// what the direct path observes.
//
// The classifier has its own differential target next to its reference
// implementation (FuzzDefaultClassifierMatchesReference in
// classify_internal_test.go).
//
// Run them with `make fuzz` (short budget per target); CI runs the same in a
// dedicated job. A failing input is written to testdata/fuzz/<Target>/ —
// commit that file: it becomes a permanent regression seed replayed by
// `go test ./...`.

// fuzzConns is how many connections the driver targets drive. Two is the
// smallest number that exercises cross-connection writes (§4.11).
const fuzzConns = 2

// fuzzDrainStmt ends a textual transaction during the drain phase. It is a
// sentinel: the adversarial classifier keeps classifying it as KindRollback so
// that draining stays deterministic however it mangles everything else.
const fuzzDrainStmt = "ROLLBACK /* txnpure fuzz drain */"

// fuzzStatementChecker declares any statement mentioning "notify" an external
// call. It is a pure function of the query, as WithStatementChecker requires
// (the prepared-statement path evaluates it once at prepare time).
func fuzzStatementChecker(query string) (Op, bool) {
	if strings.Contains(strings.ToLower(query), "notify") {
		return Op{Kind: "external", Name: "notify"}, true
	}
	return Op{}, false
}

// FuzzWriteTarget guards the table-name extraction behind cross-connection
// write violations: it runs on arbitrary user SQL, so it must not panic on
// truncated, quoted or non-UTF-8 statements, and it must stay a deterministic,
// bounded-length function of the query — the op name is a violation identity,
// not a place for unbounded data.
func FuzzWriteTarget(f *testing.F) {
	seeds := []string{
		"INSERT INTO users (id) VALUES (1)",
		`INSERT INTO "Mixed Case" VALUES (1)`,
		"INSERT INTO `t` VALUES (1)",
		`INSERT INTO "unterminated`,
		"INSERT INTO", "insert into ", "INSERT",
		"UPDATE", "update /* c */ t set a = 1", "UPDATE public.accounts SET a = 1",
		"DELETE FROM", "delete from public.sessions where 1",
		"TRUNCATE", "truncate table logs", "TRUNCATE logs",
		"MERGE INTO t USING s ON (t.id = s.id)",
		"COPY t FROM stdin", "UPSERT INTO t VALUES (1)",
		"-- only a comment", "/* unterminated", "", "   ", "\x00\xff",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, query string) {
		name := writeTarget(query)
		if again := writeTarget(query); again != name {
			t.Fatalf("writeTarget(%q) is not deterministic: %q then %q", query, name, again)
		}
		if len(name) > len(query) {
			t.Fatalf("writeTarget(%q) = %q, longer than the query it came from", query, name)
		}
	})
}

// FuzzDriverSequence drives a wrapped null DB through an arbitrary sequence of
// statements, transactions and checkpoints and asserts the two properties the
// whole detector rests on: the scope's open-transaction counter stays within
// [0, connections] at every step, and once every transaction is ended the
// counter is back to zero. A counter stuck high turns every later checkpoint
// in the scope into a false positive; a negative one silences real violations.
func FuzzDriverSequence(f *testing.F) {
	seeds := []struct {
		ops   []byte
		query string
	}{
		{[]byte{0x20, 0xc0, 0x40}, "SELECT 1"},                       // textual BEGIN, write, COMMIT
		{[]byte{0x03, 0x06, 0x04}, "SELECT 1"},                       // driver tx, check, commit
		{[]byte{0x03, 0xc8, 0x05}, "INSERT INTO t VALUES (1)"},       // cross-connection write
		{[]byte{0x22, 0xc2, 0x42}, "notify_external('x')"},           // prepared statements
		{[]byte{0x03, 0x07, 0x0f}, "BEGIN"},                          // unscoped statements
		{[]byte{0x20, 0x80, 0xa0, 0x60}, "SAVEPOINT sp1"},            // savepoints keep the tx open
		{[]byte{0x03, 0x0b, 0x04, 0x0c}, "COMMIT"},                   // begin/commit on both conns
		{[]byte{0x20, 0x20, 0x40, 0x40, 0x60}, "ROLLBACK"},           // repeated begin/commit
		{[]byte{0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa}, "\x00\xff"},     // non-UTF-8 query
		{[]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}, ""}, // every action once
	}
	for _, s := range seeds {
		f.Add(s.ops, s.query)
	}
	f.Fuzz(func(t *testing.T, ops []byte, query string) {
		det := New(
			WithReporter(NewCollectingReporter()),
			WithStatementChecker(fuzzStatementChecker),
			WithUnscopedTxDetection(),
			WithUnscopedCheckDetection(),
			WithLeakedTxDetection(),
			WithStackDepth(4),
		)
		if open := runFuzzDriverOps(t, det, ops, query); open != 0 {
			t.Fatalf("open transaction counter is %d after draining every transaction, want 0", open)
		}
	})
}

// FuzzAdversarialClassifier repeats the driver sequence with a classifier that
// returns arbitrary kinds — including values outside the defined StatementKind
// range — for arbitrary queries. WithClassifier is a public escape hatch, so a
// user-supplied classifier must never be able to crash the wrapper or drive
// the counter out of range; the counter must still drain to zero, because
// draining only depends on the transactions the wrapper believes are open.
func FuzzAdversarialClassifier(f *testing.F) {
	f.Add([]byte{0x20, 0xc0, 0x40}, "SELECT 1", byte(0))
	f.Add([]byte{0x03, 0x06, 0x04}, "INSERT INTO t VALUES (1)", byte(7))
	f.Add([]byte{0x22, 0xc2, 0x42, 0x05}, "notify_external('x')", byte(200))
	f.Add([]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}, "BEGIN", byte(1))
	f.Fuzz(func(t *testing.T, ops []byte, query string, seed byte) {
		det := New(
			WithReporter(NewCollectingReporter()),
			WithClassifier(adversarialClassifier(seed)),
			WithStatementChecker(fuzzStatementChecker),
			WithLeakedTxDetection(),
			WithStackDepth(0),
		)
		if open := runFuzzDriverOps(t, det, ops, query); open != 0 {
			t.Fatalf("open transaction counter is %d after draining every transaction, want 0", open)
		}
	})
}

// adversarialClassifier maps a query to a pseudo-random StatementKind, biased
// to also produce values outside the defined range. It is a pure function of
// the query — Classifier must be, since the prepared-statement path evaluates
// it once at prepare time — and it leaves the drain sentinel alone.
func adversarialClassifier(seed byte) Classifier {
	return func(query string) StatementKind {
		if query == fuzzDrainStmt {
			return KindRollback
		}
		h := uint32(seed)
		for i := 0; i < len(query); i++ {
			h = h*31 + uint32(query[i])
		}
		return StatementKind(h % 7) // 5 and 6 are not valid kinds, on purpose
	}
}

// runFuzzDriverOps drives fuzzConns connections of det's null DB through the
// op stream, checking the counter invariant after every step, then ends every
// transaction it may have opened and returns the final counter.
//
// Each byte encodes one operation: bits 0-2 select the action, bit 3 the
// connection, bits 5-7 the statement.
func runFuzzDriverOps(t *testing.T, det *Detector, ops []byte, query string) int64 {
	t.Helper()

	// Index 0 is the fuzzer's query; the rest keep sequences meaningful
	// without the fuzzer having to discover SQL keywords by itself.
	stmts := [8]string{
		query,
		"BEGIN",
		"COMMIT",
		"ROLLBACK",
		"SAVEPOINT sp1",
		"ROLLBACK TO SAVEPOINT sp1",
		"INSERT INTO t VALUES (1)",
		"SELECT 1",
	}

	db := det.NewNullDB()
	defer func() { _ = db.Close() }()

	ctx, finish := det.StartScope(context.Background(), "FuzzScope")
	defer finish()
	s := scopeFrom(ctx)

	var conns [fuzzConns]*sql.Conn
	for i := range conns {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("open connection %d: %v", i, err)
		}
		conns[i] = c
	}
	var txs [fuzzConns]*sql.Tx

	// Bound the work per input so fuzzing stays throughput-bound rather than
	// spending its budget replaying one huge sequence.
	if len(ops) > 64 {
		ops = ops[:64]
	}
	for _, op := range ops {
		i := int(op>>3) % fuzzConns
		c, q := conns[i], stmts[op>>5]
		switch op & 0x7 {
		case 0:
			_, _ = c.ExecContext(ctx, q)
		case 1:
			if rows, err := c.QueryContext(ctx, q); err == nil {
				_ = rows.Close()
			}
		case 2:
			if st, err := c.PrepareContext(ctx, q); err == nil {
				_, _ = st.ExecContext(ctx)
				_ = st.Close()
			}
		case 3:
			if txs[i] == nil {
				if tx, err := c.BeginTx(ctx, nil); err == nil {
					txs[i] = tx
				}
			}
		case 4:
			if txs[i] != nil {
				_ = txs[i].Commit()
				txs[i] = nil
			}
		case 5:
			if txs[i] != nil {
				_ = txs[i].Rollback()
				txs[i] = nil
			}
		case 6:
			det.Check(ctx, Op{Kind: "http", Name: "GET api.example.com"})
		case 7:
			// The same statement with no scope on its context: pooled
			// connections must not leak transaction state across scopes.
			_, _ = c.ExecContext(context.Background(), q)
		}
		if open := s.openTxs.Load(); open < 0 || open > fuzzConns {
			t.Fatalf("open transaction counter out of range after op %#02x: %d", op, open)
		}
	}

	// Drain: end the driver-level transaction of every connection, then close
	// any textual transaction it may still hold. Nothing here depends on the
	// fuzzed input, so the counter must be back to zero afterwards.
	for i, c := range conns {
		if txs[i] != nil {
			_ = txs[i].Rollback()
		}
		_, _ = c.ExecContext(ctx, fuzzDrainStmt)
		_ = c.Close()
	}
	return s.openTxs.Load()
}

// FuzzPreparedStatementParity is the differential test behind the
// prepared-statement optimization: the classification and the statement-checker
// matches are computed once at prepare time and reused per execution, so a
// prepared statement must report exactly what the same statement executed
// directly reports — same ops, same order, same open-transaction counts.
func FuzzPreparedStatementParity(f *testing.F) {
	seeds := []string{
		"BEGIN;INSERT INTO t VALUES (1);COMMIT",
		"insert into users values (1)",
		"select notify_external('x')",
		"begin;select notify('x');rollback",
		"WITH moved AS (DELETE FROM queue RETURNING *) INSERT INTO archive SELECT * FROM moved",
		"SAVEPOINT sp1;ROLLBACK TO SAVEPOINT sp1;COMMIT",
		"-- c\nUPDATE accounts SET balance = 0",
		"", ";;", "\x00\xff",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, script string) {
		stmts := strings.Split(script, ";")
		if len(stmts) > 16 {
			stmts = stmts[:16]
		}
		direct := runFuzzScript(t, stmts, false)
		prepared := runFuzzScript(t, stmts, true)
		if len(direct) != len(prepared) {
			t.Fatalf("direct execution reported %d violations, prepared %d\ndirect:   %v\nprepared: %v",
				len(direct), len(prepared), direct, prepared)
		}
		for i := range direct {
			if direct[i] != prepared[i] {
				t.Fatalf("violation %d differs: direct %q, prepared %q", i, direct[i], prepared[i])
			}
		}
	})
}

// runFuzzScript executes stmts on one connection while another connection
// holds an open transaction (so that writes are cross-connection violations
// and statement-checker matches see an open transaction), and returns the
// reported violation identities in order. With prepare, every statement goes
// through the prepared-statement path instead of the direct one.
func runFuzzScript(t *testing.T, stmts []string, prepare bool) []string {
	t.Helper()

	rep := NewCollectingReporter()
	det := New(
		WithReporter(rep),
		WithStatementChecker(fuzzStatementChecker),
		WithStackDepth(0),
	)
	db := det.NewNullDB()
	defer func() { _ = db.Close() }()

	ctx, finish := det.StartScope(context.Background(), "FuzzScope")
	defer finish()

	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("open holder connection: %v", err)
	}
	defer func() { _ = holder.Close() }()
	tx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin holder transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("open connection: %v", err)
	}
	defer func() { _ = c.Close() }()

	for _, q := range stmts {
		if prepare {
			st, err := c.PrepareContext(ctx, q)
			if err != nil {
				t.Fatalf("prepare %q: %v", q, err)
			}
			_, _ = st.ExecContext(ctx)
			_ = st.Close()
			continue
		}
		_, _ = c.ExecContext(ctx, q)
	}

	vs := rep.Violations()
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Op.Kind + "|" + v.Op.Name + "|" + strconv.Itoa(v.OpenTxs)
	}
	return out
}

// FuzzLoadBaseline feeds arbitrary bytes to the baseline file parser: the file
// is committed to a repository and edited by hand, so a malformed one must
// produce an error, never a panic. Whatever does load must survive a
// Save/Load round trip byte for byte, otherwise the ratchet file would churn
// in diffs, and every loaded entry must actually suppress its own identity.
func FuzzLoadBaseline(f *testing.F) {
	saved := NewBaseline().
		Add("CreateUser", Op{Kind: "http", Name: "GET api.example.com"}).
		Add(AnyScope, Op{Kind: "db", Name: "users"})
	path := filepath.Join(f.TempDir(), "baseline.json")
	if err := saved.Save(path); err != nil {
		f.Fatalf("save seed baseline: %v", err)
	}
	seed, err := os.ReadFile(path)
	if err != nil {
		f.Fatalf("read seed baseline: %v", err)
	}
	f.Add(seed)
	f.Add([]byte(`{"entries":[{"scope":"s","kind":"k","name":"n"},{"scope":"s","kind":"k","name":"n"}]}`))
	f.Add([]byte(`{"entries":null}`))
	f.Add([]byte(`{"entries":[{}]}`))
	f.Add([]byte("{"))
	f.Add([]byte("null"))
	f.Add([]byte(""))
	f.Add([]byte("\xff\xfe"))

	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		in := filepath.Join(dir, "in.json")
		if err := os.WriteFile(in, data, 0o600); err != nil {
			t.Fatalf("write baseline: %v", err)
		}
		b, err := LoadBaseline(in)
		if err != nil {
			return // rejecting a malformed file is the expected outcome
		}

		out := filepath.Join(dir, "out.json")
		if err := b.Save(out); err != nil {
			t.Fatalf("save loaded baseline: %v", err)
		}
		again, err := LoadBaseline(out)
		if err != nil {
			t.Fatalf("reload of a saved baseline failed: %v", err)
		}
		out2 := filepath.Join(dir, "out2.json")
		if err := again.Save(out2); err != nil {
			t.Fatalf("save reloaded baseline: %v", err)
		}
		first, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("read saved baseline: %v", err)
		}
		second, err := os.ReadFile(out2)
		if err != nil {
			t.Fatalf("read re-saved baseline: %v", err)
		}
		if string(first) != string(second) {
			t.Fatalf("baseline round trip is not stable:\nfirst:\n%s\nsecond:\n%s", first, second)
		}
		for _, k := range b.Entries() {
			if !again.covers(k) {
				t.Fatalf("entry %+v was lost by the round trip", k)
			}
		}
	})
}

// FuzzViolationPipeline pushes arbitrary scope and op strings — non-UTF-8,
// embedded NULs, empty, the AnyScope wildcard — through the full reporting
// pipeline (allowlist → baseline → throttling → rendering). Identities come
// from user code, so no string may crash a reporter, and the suppression
// contract must hold whatever the strings look like.
func FuzzViolationPipeline(f *testing.F) {
	f.Add("CreateUser", "http", "GET api.example.com", false, false)
	f.Add("CreateUser", "http", "GET api.example.com", true, false)
	f.Add("CreateUser", "db", "users", false, true)
	f.Add("*", "", "", true, true)
	f.Add("", "\x00", "\x00", false, false)
	f.Add("s\x00k", "", "n", false, false)
	f.Add("\xff", "\xff", "\xff", false, false)

	f.Fuzz(func(t *testing.T, scopeName, kind, name string, allowed, baselined bool) {
		op := Op{Kind: kind, Name: name}
		collected := NewCollectingReporter()

		var next Reporter = collected
		if baselined {
			next = NewBaselineReporter(NewBaseline().Add(scopeName, op), collected)
		}
		allowlist := NewAllowlist()
		if allowed {
			allowlist.Add(scopeName, op, "fuzz")
		}
		det := New(
			WithReporter(NewThrottlingReporter(next, time.Minute)),
			WithReporter(NewSlogReporter(slog.New(slog.NewJSONHandler(io.Discard, nil)))),
			WithAllowlist(allowlist),
			WithStackDepth(2),
		)

		ctx, finish := det.StartScope(context.Background(), scopeName, WithScopeAttrs(Attr(name, kind)))
		defer finish()
		scopeFrom(ctx).openTxs.Add(1)
		det.Check(ctx, op)

		want := 1
		if allowed || baselined {
			want = 0
		}
		vs := collected.Violations()
		if len(vs) != want {
			t.Fatalf("got %d violations (scope %q, op %+v, allowed=%v, baselined=%v), want %d",
				len(vs), scopeName, op, allowed, baselined, want)
		}
		for _, v := range vs {
			if v.String() == "" {
				t.Fatal("Violation.String() is empty")
			}
		}
	})
}
