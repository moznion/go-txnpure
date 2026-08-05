package txnpure_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	txnpure "github.com/moznion/go-txnpure"
)

var opEnqueue = txnpure.Op{Kind: "enqueue", Name: "sqs:SendMessage"}

// --- §4.13: exact in-transaction call counts ---

func TestAllowInTransactionExactCallsCovered(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	det.Check(ctx, opHTTP, txnpure.AllowInTransaction("TICKET-42", 2))
	det.Check(ctx, opHTTP, txnpure.AllowInTransaction("TICKET-42", 2))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finish()

	rep.RequireNoViolations(t)
	rep.RequireNoStaleAllows(t) // total 2 matches the declared 2 exactly
}

func TestAllowInTransactionExactCallsExcessViolates(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	det.Check(ctx, opHTTP, txnpure.AllowInTransaction("TICKET-42", 1))
	det.Check(ctx, opHTTP, txnpure.AllowInTransaction("TICKET-42", 1)) // the excess call
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finish()

	vs := rep.Violations()
	if len(vs) != 1 {
		t.Fatalf("got %d violations, want 1 (the second call is beyond the declared count)", len(vs))
	}
	v := vs[0]
	if v.Call != 2 {
		t.Errorf("Call = %d, want 2", v.Call)
	}
	if len(v.AllowedCalls) != 1 || v.AllowedCalls[0] != 1 {
		t.Errorf("AllowedCalls = %v, want [1]", v.AllowedCalls)
	}
	if !strings.Contains(v.String(), "allowed for exactly 1 in-transaction call(s)") {
		t.Errorf("String() = %q lacks the declined-allow explanation", v.String())
	}
	// The excess already surfaced as a Violation; no finish-time stale on top.
	rep.RequireNoStaleAllows(t)
}

func TestAllowInTransactionExactCallsShortfallStaleAtFinish(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	det.Check(ctx, opHTTP, txnpure.AllowInTransaction("TICKET-42", 2))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	rep.RequireNoViolations(t)
	rep.RequireNoStaleAllows(t) // shortfall is only decidable at finish
	finish()

	sas := rep.StaleAllows()
	if len(sas) != 1 {
		t.Fatalf("got %d stale allows after finish, want 1", len(sas))
	}
	sa := sas[0]
	if sa.Scope != "CreateUser" || sa.Op != opHTTP || sa.Reason != "TICKET-42" {
		t.Errorf("StaleAllow = %+v", sa)
	}
	if sa.Calls != 1 || len(sa.AllowedCalls) != 1 || sa.AllowedCalls[0] != 2 {
		t.Errorf("StaleAllow counts = calls %d allowed %v, want calls 1 allowed [2]", sa.Calls, sa.AllowedCalls)
	}
}

func TestAllowInTransactionMultipleCounts(t *testing.T) {
	// A total matching any declared count is clean...
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		det.Check(ctx, opHTTP, txnpure.AllowInTransaction("TICKET-42", 1, 3))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finish()
	rep.RequireNoViolations(t)
	rep.RequireNoStaleAllows(t)

	// ...but a total between the declared counts is a shortfall.
	det2, rep2, db2 := setup(t)
	ctx2, finish2 := det2.StartScope(context.Background(), "CreateUser")
	tx2, err := db2.BeginTx(ctx2, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		det2.Check(ctx2, opHTTP, txnpure.AllowInTransaction("TICKET-42", 1, 3))
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	finish2()
	rep2.RequireNoViolations(t) // 2 ≤ max(1,3): both calls were suppressed
	sas := rep2.StaleAllows()
	if len(sas) != 1 || sas[0].Calls != 2 {
		t.Fatalf("StaleAllows = %+v, want one entry with Calls=2 (2 is not among {1,3})", sas)
	}
}

func TestAllowlistExactCalls(t *testing.T) {
	al := txnpure.NewAllowlist().Add("CreateUser", opHTTP, "TICKET-42", 1)
	det, rep, db := setup(t, txnpure.WithAllowlist(al))
	ctx, finish := det.StartScope(context.Background(), "CreateUser")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	det.Check(ctx, opHTTP)
	det.Check(ctx, opHTTP) // beyond the declared count
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finish()

	vs := rep.Violations()
	if len(vs) != 1 {
		t.Fatalf("got %d violations, want 1", len(vs))
	}
	if len(vs[0].AllowedCalls) != 1 || vs[0].AllowedCalls[0] != 1 {
		t.Errorf("AllowedCalls = %v, want [1]", vs[0].AllowedCalls)
	}
	// The first call was suppressed, so the entry is used, not unused.
	if unused := al.UnusedEntries(); len(unused) != 0 {
		t.Errorf("UnusedEntries = %+v, want empty (the entry did suppress call #1)", unused)
	}
}

func TestAllowlistExactCallsShortfallStaleAtFinish(t *testing.T) {
	al := txnpure.NewAllowlist().Add("CreateUser", opHTTP, "TICKET-42", 2)
	det, rep, db := setup(t, txnpure.WithAllowlist(al))
	ctx, finish := det.StartScope(context.Background(), "CreateUser")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	det.Check(ctx, opHTTP)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finish()

	rep.RequireNoViolations(t)
	sas := rep.StaleAllows()
	if len(sas) != 1 || sas[0].Calls != 1 || sas[0].Op != opHTTP {
		t.Fatalf("StaleAllows = %+v, want one shortfall for %v", sas, opHTTP)
	}
}

// An in-code declaration whose counts decline falls through to the Allowlist,
// exactly as an unmarked call would.
func TestAllowExactCallsFallThroughToAllowlist(t *testing.T) {
	al := txnpure.NewAllowlist().Add("CreateUser", opHTTP, "central; TICKET-42")
	det, rep, db := setup(t, txnpure.WithAllowlist(al))
	ctx, finish := det.StartScope(context.Background(), "CreateUser")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	det.Check(ctx, opHTTP, txnpure.AllowInTransaction("call site", 1))
	det.Check(ctx, opHTTP, txnpure.AllowInTransaction("call site", 1)) // declined here, covered centrally
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finish()

	rep.RequireNoViolations(t)
	if unused := al.UnusedEntries(); len(unused) != 0 {
		t.Errorf("UnusedEntries = %+v, want empty (the entry covered the fall-through call)", unused)
	}
}

func TestAllowCountsCallerSliceCopied(t *testing.T) {
	counts := []int{1}
	opt := txnpure.AllowInTransaction("TICKET-42", counts...)
	al := txnpure.NewAllowlist().Add("CreateUser", opEnqueue, "TICKET-42", counts...)
	counts[0] = 99 // must not affect either declaration

	det, rep, db := setup(t, txnpure.WithAllowlist(al))
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		det.Check(ctx, opHTTP, opt)
		det.Check(ctx, opEnqueue)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finish()

	// With the original count 1, the second call of each identity violates;
	// with the mutated 99 both would have been suppressed.
	if got := len(rep.Violations()); got != 2 {
		t.Fatalf("got %d violations, want 2 (declared counts must not alias caller memory)", got)
	}
}

// Counts below 1 can never cover a call: the declaration is permanently stale
// rather than rejected.
func TestAllowCountBelowOneNeverCovers(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	det.Check(ctx, opHTTP, txnpure.AllowInTransaction("TICKET-42", 0))
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("got %d violations, want 1", got)
	}
}

// --- §4.14: AllowInTransactionHere ---

func TestAllowInTransactionHereSuppressesCheckpoint(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	actx := txnpure.AllowInTransactionHere(ctx, "TICKET-42")
	det.Check(actx, opHTTP)
	rep.RequireNoViolations(t)
	rep.RequireNoStaleAllows(t)

	// The original ctx is unmarked: the same check still violates.
	det.Check(ctx, opHTTP)
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("got %d violations, want 1 (the mark must not leak onto the original ctx)", got)
	}
}

func TestAllowInTransactionHereCrossConnWrite(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	txConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txConn.Close() }()
	tx, err := txConn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	otherConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = otherConn.Close() }()

	actx := txnpure.AllowInTransactionHere(ctx, "cache eviction is best-effort; TICKET-99")
	if _, err := otherConn.ExecContext(actx, "DELETE FROM order_cache WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)
	rep.RequireNoStaleAllows(t)

	// The same write without the mark still violates.
	if _, err := otherConn.ExecContext(ctx, "DELETE FROM order_cache WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("got %d violations, want 1", got)
	}
}

func TestAllowInTransactionHereStatementChecker(t *testing.T) {
	det, rep, db := setup(t, txnpure.WithStatementChecker(notifyChecker))
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	actx := txnpure.AllowInTransactionHere(ctx, "receiver is idempotent; TICKET-123")
	if _, err := tx.ExecContext(actx, "SELECT pg_notify('ch', 'msg')"); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)

	// Prepared-statement path decides identically.
	st, err := tx.PrepareContext(ctx, "SELECT pg_notify('ch', 'msg')")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.ExecContext(actx); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)

	// Unmarked execution still violates.
	if _, err := st.ExecContext(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("got %d violations, want 1", got)
	}
}

type stubRoundTripper struct{}

func (stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK}, nil
}

func TestAllowInTransactionHereRoundTripper(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	rt := det.WrapRoundTripper(stubRoundTripper{})
	newReq := func(ctx context.Context) *http.Request {
		return (&http.Request{
			Method: http.MethodPost,
			URL:    &url.URL{Scheme: "https", Host: "lock.example.com", Path: "/heartbeat"},
		}).WithContext(ctx)
	}

	actx := txnpure.AllowInTransactionHere(ctx, "heartbeat holds the row lock; TICKET-42")
	if _, err := rt.RoundTrip(newReq(actx)); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)

	if _, err := rt.RoundTrip(newReq(ctx)); err != nil {
		t.Fatal(err)
	}
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("got %d violations, want 1 (unmarked request)", got)
	}
}

// An inner mark replaces the outer one for contexts descending from it: the
// inner declaration alone decides.
func TestAllowInTransactionHereInnerShadowsOuter(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	outer := txnpure.AllowInTransactionHere(ctx, "outer suppresses everything")
	inner := txnpure.AllowInTransactionHere(outer, "inner covers nothing", 0)
	det.Check(inner, opHTTP)
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("got %d violations, want 1 (the inner mark must shadow the outer one)", got)
	}
	det.Check(outer, opHTTP)
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("got %d violations, want still 1 (the outer region is intact)", got)
	}
}

// A nested StartScope does not inherit the mark: the exemption was reviewed
// against the scope it was written in.
func TestAllowInTransactionHereNotInheritedByNestedScope(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "Outer")
	defer finish()

	actx := txnpure.AllowInTransactionHere(ctx, "outer-scope exemption")
	inner, innerFinish := det.StartScope(actx, "Inner")
	defer innerFinish()

	tx, err := db.BeginTx(inner, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	det.Check(inner, opHTTP)
	if got := len(rep.Violations()); got != 1 {
		t.Fatalf("got %d violations, want 1 (a nested scope must not inherit the mark)", got)
	}
}

func TestAllowInTransactionHereNoScopeIsNoop(t *testing.T) {
	ctx := context.Background()
	if actx := txnpure.AllowInTransactionHere(ctx, "no scope yet"); actx != ctx {
		t.Fatal("AllowInTransactionHere without a scope must return ctx unchanged")
	}
}

func TestAllowInTransactionHereStaleOutsideTxCheckpoint(t *testing.T) {
	det, rep, _ := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	actx := txnpure.AllowInTransactionHere(ctx, "TICKET-42")
	det.Check(actx, opHTTP)
	rep.RequireNoViolations(t)
	sas := rep.StaleAllows()
	if len(sas) != 1 || sas[0].Reason != "TICKET-42" || sas[0].Op != opHTTP {
		t.Fatalf("StaleAllows = %+v, want one per-event stale for the marked checkpoint", sas)
	}
}

func TestAllowInTransactionHereStaleOutsideTxWrite(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	actx := txnpure.AllowInTransactionHere(ctx, "TICKET-99")
	if _, err := db.ExecContext(actx, "DELETE FROM order_cache WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)
	sas := rep.StaleAllows()
	if len(sas) != 1 || sas[0].Op.Kind != "db" || sas[0].Op.Name != "order_cache" {
		t.Fatalf("StaleAllows = %+v, want one {db order_cache} stale (marked write with no open tx)", sas)
	}
}

// A marked write inside its own connection's transaction is silent: that
// execution is normal, not evidence the mark is stale.
func TestAllowInTransactionHereSelfTxWriteSilent(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	actx := txnpure.AllowInTransactionHere(ctx, "TICKET-99")
	if _, err := tx.ExecContext(actx, "INSERT INTO orders (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)
	rep.RequireNoStaleAllows(t)
}

// A counted region pins the region's own event total, across op kinds.
func TestAllowInTransactionHereExactCallsRegion(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	actx := txnpure.AllowInTransactionHere(ctx, "exactly one side effect; TICKET-7", 1)
	det.Check(actx, opEnqueue)
	det.Check(actx, opHTTP) // second event in the region, whatever its op
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finish()

	vs := rep.Violations()
	if len(vs) != 1 {
		t.Fatalf("got %d violations, want 1 (the region declared exactly one call)", len(vs))
	}
	if vs[0].Op != opHTTP {
		t.Errorf("Op = %+v, want the second event %+v", vs[0].Op, opHTTP)
	}
	if len(vs[0].AllowedCalls) != 1 || vs[0].AllowedCalls[0] != 1 {
		t.Errorf("AllowedCalls = %v, want [1]", vs[0].AllowedCalls)
	}
	rep.RequireNoStaleAllows(t) // total 2 > declared max: the excess already violated
}

func TestAllowInTransactionHereRegionShortfallStaleAtFinish(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	actx := txnpure.AllowInTransactionHere(ctx, "declares two; TICKET-7", 2)
	det.Check(actx, opHTTP)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finish()

	rep.RequireNoViolations(t)
	sas := rep.StaleAllows()
	if len(sas) != 1 {
		t.Fatalf("got %d stale allows, want 1", len(sas))
	}
	sa := sas[0]
	if sa.Op != (txnpure.Op{}) {
		t.Errorf("Op = %+v, want zero (a region covers any op)", sa.Op)
	}
	if sa.Calls != 1 || len(sa.AllowedCalls) != 1 || sa.AllowedCalls[0] != 2 {
		t.Errorf("StaleAllow = %+v, want Calls=1 AllowedCalls=[2]", sa)
	}
}

// A counted region that saw no event at all is also a shortfall: it was
// created for this scope execution and suppressed nothing.
func TestAllowInTransactionHereUnusedCountedRegionStaleAtFinish(t *testing.T) {
	det, rep, _ := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	_ = txnpure.AllowInTransactionHere(ctx, "dead exemption; TICKET-7", 1)
	finish()

	sas := rep.StaleAllows()
	if len(sas) != 1 || sas[0].Calls != 0 {
		t.Fatalf("StaleAllows = %+v, want one with Calls=0", sas)
	}
}

// The three mechanisms share one covers rule: same counts, same events, same
// verdicts — choosing one is about where the exemption lives, never about
// what it can express.
func TestAllowFormsDecideIdentically(t *testing.T) {
	cases := []struct {
		name           string
		counts         []int
		events         int
		wantViolations int
		wantStale      bool
	}{
		{"uncounted", nil, 3, 0, false},
		{"exact_matches", []int{3}, 3, 0, false},
		{"excess_violates", []int{1}, 3, 2, false},
		{"shortfall_stale", []int{2}, 1, 0, true},
		{"hole_between_counts_stale", []int{1, 3}, 2, 0, true},
		{"below_one_never_covers", []int{0}, 2, 2, false},
	}
	mechanisms := []string{"option", "here", "allowlist"}

	for _, tc := range cases {
		for _, mech := range mechanisms {
			t.Run(tc.name+"/"+mech, func(t *testing.T) {
				var opts []txnpure.Option
				var al *txnpure.Allowlist
				if mech == "allowlist" {
					al = txnpure.NewAllowlist().Add("CreateUser", opHTTP, "parity", tc.counts...)
					opts = append(opts, txnpure.WithAllowlist(al))
				}
				det, rep, db := setup(t, opts...)
				ctx, finish := det.StartScope(context.Background(), "CreateUser")

				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				cctx := ctx
				if mech == "here" {
					cctx = txnpure.AllowInTransactionHere(ctx, "parity", tc.counts...)
				}
				for i := 0; i < tc.events; i++ {
					switch mech {
					case "option":
						det.Check(ctx, opHTTP, txnpure.AllowInTransaction("parity", tc.counts...))
					default:
						det.Check(cctx, opHTTP)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				finish()

				if got := len(rep.Violations()); got != tc.wantViolations {
					t.Errorf("violations = %d, want %d", got, tc.wantViolations)
				}
				gotStale := len(rep.StaleAllows()) > 0
				if gotStale != tc.wantStale {
					t.Errorf("stale = %v (%+v), want %v", gotStale, rep.StaleAllows(), tc.wantStale)
				}
			})
		}
	}
}

// Concurrent marked checkpoints must be race-free: the mark's counter is the
// only mutable state and it is atomic.
func TestAllowInTransactionHereConcurrentUse(t *testing.T) {
	det, rep, db := setup(t)
	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	actx := txnpure.AllowInTransactionHere(ctx, "TICKET-42")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				det.Check(actx, opHTTP)
			}
		}()
	}
	wg.Wait()
	rep.RequireNoViolations(t)
}
