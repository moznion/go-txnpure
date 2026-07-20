package txnpuregrpc_test

import (
	"context"
	"testing"

	txnpure "github.com/moznion/go-txnpure"
	txnpuregrpc "github.com/moznion/go-txnpure/grpc"
	"google.golang.org/grpc"
)

const fullMethod = "/example.Service/DoThing"

func TestUnaryClientInterceptorViolatesInsideTx(t *testing.T) {
	rep := txnpure.NewCollectingReporter()
	det := txnpure.New(txnpure.WithReporter(rep))
	db := det.NewNullDB()
	defer func() { _ = db.Close() }()

	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	invoked := false
	invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		invoked = true
		return nil
	}
	interceptor := txnpuregrpc.UnaryClientInterceptor(det)
	if err := interceptor(ctx, fullMethod, nil, nil, nil, invoker); err != nil {
		t.Fatal(err)
	}
	if !invoked {
		t.Error("interceptor did not invoke the RPC — the call must never be blocked")
	}
	vs := rep.Violations()
	if len(vs) != 1 {
		t.Fatalf("got %d violations, want 1", len(vs))
	}
	if vs[0].Op != (txnpure.Op{Kind: "grpc", Name: fullMethod}) {
		t.Errorf("Op = %+v, want {grpc %s}", vs[0].Op, fullMethod)
	}
}

func TestUnaryClientInterceptorCleanOutsideTx(t *testing.T) {
	rep := txnpure.NewCollectingReporter()
	det := txnpure.New(txnpure.WithReporter(rep))

	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()
	invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error { return nil }
	if err := txnpuregrpc.UnaryClientInterceptor(det)(ctx, fullMethod, nil, nil, nil, invoker); err != nil {
		t.Fatal(err)
	}
	rep.RequireNoViolations(t)
}

func TestStreamClientInterceptorViolatesInsideTx(t *testing.T) {
	rep := txnpure.NewCollectingReporter()
	det := txnpure.New(txnpure.WithReporter(rep))
	db := det.NewNullDB()
	defer func() { _ = db.Close() }()

	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	streamer := func(context.Context, *grpc.StreamDesc, *grpc.ClientConn, string, ...grpc.CallOption) (grpc.ClientStream, error) {
		return nil, nil
	}
	interceptor := txnpuregrpc.StreamClientInterceptor(det)
	if _, err := interceptor(ctx, &grpc.StreamDesc{}, nil, fullMethod, streamer); err != nil {
		t.Fatal(err)
	}
	vs := rep.Violations()
	if len(vs) != 1 || vs[0].Op.Kind != "grpc" || vs[0].Op.Name != fullMethod {
		t.Fatalf("violations = %+v, want one grpc violation for %s", vs, fullMethod)
	}
}
