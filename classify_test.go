package txnpure_test

import (
	"testing"

	txnpure "github.com/moznion/go-txnpure"
)

func TestDefaultClassifier(t *testing.T) {
	cases := []struct {
		query string
		want  txnpure.StatementKind
	}{
		{"BEGIN", txnpure.KindBegin},
		{"begin", txnpure.KindBegin},
		{"  \t\nBEGIN", txnpure.KindBegin},
		{"BEGIN TRANSACTION", txnpure.KindBegin},
		{"START TRANSACTION", txnpure.KindBegin},
		{"start transaction read only", txnpure.KindBegin},
		{"-- comment\nBEGIN", txnpure.KindBegin},
		{"/* comment */ BEGIN", txnpure.KindBegin},
		{"/* multi\nline */COMMIT", txnpure.KindCommit},
		{"COMMIT", txnpure.KindCommit},
		{"commit work", txnpure.KindCommit},
		{"END", txnpure.KindCommit},
		{"ROLLBACK", txnpure.KindRollback},
		{"rollback work", txnpure.KindRollback},
		{"ABORT", txnpure.KindRollback},
		{"ROLLBACK TO SAVEPOINT sp1", txnpure.KindOther},
		{"ROLLBACK TO sp1", txnpure.KindOther},
		{"rollback  to  savepoint sp1", txnpure.KindOther},
		{"ROLLBACK /* c */ TO SAVEPOINT sp1", txnpure.KindOther},
		{"SAVEPOINT sp1", txnpure.KindOther},
		{"RELEASE SAVEPOINT sp1", txnpure.KindOther},
		{"SELECT 1", txnpure.KindOther},
		{"SHOW TABLES", txnpure.KindOther},
		{"SET autocommit = 0", txnpure.KindOther},
		{"INSERT INTO t VALUES (1)", txnpure.KindWrite},
		{"insert into t values (1)", txnpure.KindWrite},
		{"UPDATE t SET a = 1", txnpure.KindWrite},
		{"DELETE FROM t WHERE id = 1", txnpure.KindWrite},
		{"MERGE INTO t USING s ON (t.id = s.id)", txnpure.KindWrite},
		{"TRUNCATE TABLE t", txnpure.KindWrite},
		{"CREATE TABLE t (id int)", txnpure.KindWrite},
		{"DROP TABLE t", txnpure.KindWrite},
		{"WITH x AS (SELECT 1) SELECT * FROM x", txnpure.KindOther},
		{"WITH x AS (INSERT INTO t VALUES (1) RETURNING id) SELECT * FROM x", txnpure.KindWrite},
		{"", txnpure.KindOther},
		{"-- only a comment", txnpure.KindOther},
		{"/* unterminated", txnpure.KindOther},
		{"BEGINNING_COLUMN", txnpure.KindOther}, // identifier, not the BEGIN keyword
	}
	for _, c := range cases {
		if got := txnpure.DefaultClassifier(c.query); got != c.want {
			t.Errorf("DefaultClassifier(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}

func TestStatementKindString(t *testing.T) {
	cases := map[txnpure.StatementKind]string{
		txnpure.KindOther:    "other",
		txnpure.KindBegin:    "begin",
		txnpure.KindCommit:   "commit",
		txnpure.KindRollback: "rollback",
		txnpure.KindWrite:    "write",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", k, got, want)
		}
	}
}
