// Command enqueue is a runnable txnpure example for non-HTTP side effects: a
// job enqueue and a mail send, each wrapped with detector.Do so they become
// checkpoints without a dedicated adapter. Both run inside a transaction here
// (the bug), so running it prints two txnpure violations and exits.
//
// The plain functions below stand in for a real job queue (river/asynq/SQS)
// and a mail client — the generic Check/Do API is how you instrument any
// client that has no built-in adapter.
//
//	go run .
package main

import (
	"context"
	"log/slog"
	"os"

	txnpure "github.com/moznion/go-txnpure"
)

func main() {
	detector := txnpure.New(
		txnpure.WithReporter(txnpure.NewSlogReporter(
			slog.New(slog.NewTextHandler(os.Stdout, nil)),
		)),
	)
	db := detector.NewNullDB()
	defer func() { _ = db.Close() }()

	err := detector.InScope(context.Background(), "CreateOrder", func(ctx context.Context) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()

		// BUG: enqueuing while the transaction is open. The worker may pick up
		// the job before this tx commits (or after it rolls back).
		if err := detector.Do(ctx, txnpure.Op{Kind: "enqueue", Name: "orders:process"},
			func(ctx context.Context) error { return enqueueJob(ctx, "orders:process") }); err != nil {
			return err
		}

		// BUG: sending mail inside the transaction — a rollback cannot unsend it.
		if err := detector.Do(ctx, txnpure.Op{Kind: "mail", Name: "order_confirmation"},
			func(ctx context.Context) error { return sendMail(ctx, "order_confirmation") }); err != nil {
			return err
		}

		return tx.Commit()
	})
	if err != nil {
		slog.Error("use case failed", "err", err)
		os.Exit(1)
	}
}

// enqueueJob stands in for a real job queue client (river/asynq/SQS/...).
func enqueueJob(_ context.Context, _ string) error { return nil }

// sendMail stands in for a real mail client.
func sendMail(_ context.Context, _ string) error { return nil }
