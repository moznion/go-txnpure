// Command nethttp is a runnable txnpure example: an HTTP server whose
// middleware opens a scope per route pattern, a wrapped transport that makes
// every outbound request a checkpoint, and a handler that (buggily) calls an
// external service while a database transaction is open. Running it prints one
// txnpure violation and exits.
//
//	go run .
package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"

	txnpure "github.com/moznion/go-txnpure"
)

func main() {
	detector := txnpure.New(
		txnpure.WithReporter(txnpure.NewSlogReporter(
			slog.New(slog.NewTextHandler(os.Stdout, nil)),
		)),
	)

	// Unit-test-style no-op DB; swap for a wrapped real driver in production.
	db := detector.NewNullDB()
	defer func() { _ = db.Close() }()

	// The external service the handler calls (a payment gateway, say).
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer external.Close()

	// A client whose transport makes every request a txnpure checkpoint.
	client := &http.Client{Transport: detector.WrapRoundTripper(nil)}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /checkout", func(w http.ResponseWriter, r *http.Request) {
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = tx.Rollback() }()

		// BUG: this call happens while the transaction is open. If the tx
		// later rolls back, the charge cannot be undone. txnpure reports it.
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, external.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		_ = resp.Body.Close()

		if err := tx.Commit(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	server := httptest.NewServer(scopeMiddleware(detector, mux))
	defer server.Close()

	resp, err := http.Post(server.URL+"/checkout", "application/json", nil)
	if err != nil {
		slog.Error("request failed", "err", err)
		os.Exit(1)
	}
	_ = resp.Body.Close()
}

// scopeMiddleware opens a txnpure scope per request, named by the matched
// route pattern. The pattern is read via mux.Handler(r) — http.Request.Pattern
// is only populated after routing, so an outer middleware always sees it empty
// (a go-txnproof lesson). Route patterns keep scope names a small,
// code-defined set.
func scopeMiddleware(detector *txnpure.Detector, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		if pattern == "" {
			pattern = "unmatched"
		}
		ctx, finish := detector.StartScope(r.Context(), pattern)
		defer finish()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}
