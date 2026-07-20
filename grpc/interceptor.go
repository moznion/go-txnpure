// Package txnpuregrpc provides gRPC client interceptors that make every
// outgoing RPC a txnpure checkpoint: an RPC issued while a transaction is open
// in its scope is reported as a Violation. It is a separate module so the root
// txnpure module stays free of the google.golang.org/grpc dependency.
package txnpuregrpc

import (
	"context"

	txnpure "github.com/moznion/go-txnpure"
	"google.golang.org/grpc"
)

// opFor builds the Op for an RPC. The gRPC full method ("/pkg.Service/Method")
// is already a small, code-defined identifier, so it is a good Op.Name — it
// keys the Allowlist/Baseline like any other operation.
func opFor(method string) txnpure.Op {
	return txnpure.Op{Kind: "grpc", Name: method}
}

// UnaryClientInterceptor returns a grpc.UnaryClientInterceptor that checks
// every unary RPC against the detector before it is invoked. The RPC is never
// blocked — txnpure observes and reports.
//
//	conn, err := grpc.NewClient(target,
//		grpc.WithUnaryInterceptor(txnpuregrpc.UnaryClientInterceptor(det)))
func UnaryClientInterceptor(d *txnpure.Detector) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		d.Check(ctx, opFor(method))
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// StreamClientInterceptor returns a grpc.StreamClientInterceptor that checks
// stream creation against the detector. The check runs when the stream is
// opened (the point that commits to the RPC), not per message.
//
//	conn, err := grpc.NewClient(target,
//		grpc.WithStreamInterceptor(txnpuregrpc.StreamClientInterceptor(det)))
func StreamClientInterceptor(d *txnpure.Detector) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		d.Check(ctx, opFor(method))
		return streamer(ctx, desc, cc, method, opts...)
	}
}
