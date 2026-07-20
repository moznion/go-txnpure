package txnpure

import "testing"

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
