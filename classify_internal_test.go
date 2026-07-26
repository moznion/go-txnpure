package txnpure

import (
	"strings"
	"testing"
)

func TestWriteTarget(t *testing.T) {
	cases := map[string]string{
		"INSERT INTO users (id) VALUES (1)":     "users",
		"insert into Users values (1)":          "users",
		"INSERT INTO public.users VALUES (1)":   "public.users",
		`INSERT INTO "MixedCase" VALUES (1)`:    "MixedCase",
		"REPLACE INTO t VALUES (1)":             "t",
		"UPDATE accounts SET balance = 0":       "accounts",
		"DELETE FROM sessions WHERE expired":    "sessions",
		"TRUNCATE TABLE logs":                   "logs",
		"TRUNCATE logs":                         "logs",
		"MERGE INTO t USING s ON (t.id = s.id)": "t",
		"-- c\nUPDATE t SET a = 1":              "t",
		"CREATE TABLE t (id int)":               "", // DDL: no simple target
		"CALL do_stuff()":                       "", // opaque
	}
	for q, want := range cases {
		if got := writeTarget(q); got != want {
			t.Errorf("writeTarget(%q) = %q, want %q", q, got, want)
		}
	}
}

// referenceClassifier is the straightforward strings.ToUpper-based
// specification of DefaultClassifier, kept as a fuzz oracle for the
// allocation-free first-byte-dispatch implementation.
func referenceClassifier(query string) StatementKind {
	q := stripLeading(query)
	tok := strings.ToUpper(q[:identRunLen(q)])
	switch tok {
	case "BEGIN", "START":
		return KindBegin
	case "COMMIT", "END":
		return KindCommit
	case "ROLLBACK", "ABORT":
		rest := stripLeading(q[len(tok):])
		if strings.ToUpper(rest[:identRunLen(rest)]) == "TO" {
			return KindOther
		}
		return KindRollback
	case "INSERT", "UPDATE", "DELETE", "MERGE", "TRUNCATE", "REPLACE", "UPSERT", "COPY", "IMPORT",
		"CREATE", "ALTER", "DROP", "GRANT", "REVOKE", "COMMENT", "REFRESH",
		"CALL", "DO":
		return KindWrite
	case "WITH":
		rest := q[len(tok):]
		start := -1
		for i := 0; i <= len(rest); i++ {
			if i < len(rest) && isIdentChar(rest[i]) {
				if start < 0 {
					start = i
				}
				continue
			}
			if start >= 0 {
				switch strings.ToUpper(rest[start:i]) {
				case "INSERT", "UPDATE", "DELETE", "MERGE", "TRUNCATE", "REPLACE":
					return KindWrite
				}
				start = -1
			}
		}
		return KindOther
	default:
		return KindOther
	}
}

func FuzzDefaultClassifierMatchesReference(f *testing.F) {
	seeds := []string{
		"", "BEGIN", "begin transaction", "COMMIT", "end", "ROLLBACK", "abort",
		"ROLLBACK TO SAVEPOINT sp1", "rollback  to  sp1", "ROLLBACK TOO",
		"SELECT 1", "insert into t values (1)", "UPDATE t SET a = 1",
		"-- c\nBEGIN", "/* c */ COMMIT", "/* unterminated", "-- only",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"with x as (delete from q returning *) insert into a select * from x",
		"wItH x aS (uPdAtE t SET a=1) SELECT 1",
		"SAVEPOINT sp1", "RELEASE SAVEPOINT sp1", "BEGINNING_COLUMN",
		"\t\r\n  StArT tRaNsAcTiOn", "9begin", "_commit", "ROLLBACK/*c*/TO x",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		if got, want := DefaultClassifier(q), referenceClassifier(q); got != want {
			t.Errorf("DefaultClassifier(%q) = %v, reference = %v", q, got, want)
		}
	})
}
