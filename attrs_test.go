package txnpure_test

import (
	"context"
	"log/slog"
	"testing"

	txnpure "github.com/moznion/go-txnpure"
)

// Merge order: detector-level (WithScopeAttrsFunc, evaluated once at scope
// start) → scope-level (WithScopeAttrs) → check-level (WithCheckAttrs);
// duplicates kept.
func TestAttrsMergeOrder(t *testing.T) {
	evaluations := 0
	det, rep, db := setup(t, txnpure.WithScopeAttrsFunc(func(ctx context.Context) []txnpure.ScopeAttr {
		evaluations++
		return []txnpure.ScopeAttr{txnpure.Attr("trace_id", "t-1")}
	}))

	ctx, finish := det.StartScope(context.Background(), "CreateUser",
		txnpure.WithScopeAttrs(txnpure.Attr("user_id", 42), txnpure.Attr("trace_id", "dup-kept")))
	defer finish()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	det.Check(ctx, opHTTP, txnpure.WithCheckAttrs(txnpure.Attr("attempt", 1)))
	det.Check(ctx, opHTTP)

	if evaluations != 1 {
		t.Errorf("WithScopeAttrsFunc evaluated %d times, want once per scope start", evaluations)
	}
	vs := rep.Violations()
	if len(vs) != 2 {
		t.Fatalf("got %d violations, want 2", len(vs))
	}
	want := []txnpure.ScopeAttr{
		txnpure.Attr("trace_id", "t-1"),
		txnpure.Attr("user_id", 42),
		txnpure.Attr("trace_id", "dup-kept"),
		txnpure.Attr("attempt", 1),
	}
	got := vs[0].Attrs
	if len(got) != len(want) {
		t.Fatalf("Attrs = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Attrs[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(vs[1].Attrs) != 3 {
		t.Errorf("second check Attrs = %+v, want only detector+scope attrs", vs[1].Attrs)
	}
}

func TestSlogAttrs(t *testing.T) {
	attrs := txnpure.SlogAttrs([]txnpure.ScopeAttr{txnpure.Attr("k", "v")})
	if len(attrs) != 1 {
		t.Fatalf("got %d attrs, want 1", len(attrs))
	}
	if !attrs[0].Equal(slog.Any("k", "v")) {
		t.Errorf("attr = %v, want k=v", attrs[0])
	}
}
