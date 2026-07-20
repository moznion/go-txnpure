package txnpure_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	txnpure "github.com/moznion/go-txnpure"
)

func TestRoundTripperViolatesInsideTx(t *testing.T) {
	det, rep, db := setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	client := &http.Client{Transport: det.WrapRoundTripper(nil)}

	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	vs := rep.Violations()
	if len(vs) != 1 {
		t.Fatalf("got %d violations, want 1", len(vs))
	}
	v := vs[0]
	if v.Op.Kind != "http" {
		t.Errorf("Kind = %q, want http", v.Op.Kind)
	}
	wantName := "GET " + req.URL.Host
	if v.Op.Name != wantName {
		t.Errorf("Name = %q, want %q", v.Op.Name, wantName)
	}
	if v.Scope != "CreateUser" {
		t.Errorf("Scope = %q, want CreateUser", v.Scope)
	}
	if len(v.Stack) == 0 {
		t.Error("stack is empty")
	}
}

func TestRoundTripperCleanOutsideTx(t *testing.T) {
	det, rep, db := setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	client := &http.Client{Transport: det.WrapRoundTripper(nil)}

	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	rep.RequireNoViolations(t)
}

func TestRoundTripperWithRequestName(t *testing.T) {
	det, rep, db := setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	client := &http.Client{Transport: det.WrapRoundTripper(nil, txnpure.WithRequestName(func(r *http.Request) string {
		return r.Method + " payment-gateway"
	}))}

	ctx, finish := det.StartScope(context.Background(), "CreateUser")
	defer finish()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Commit() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	vs := rep.Violations()
	if len(vs) != 1 || vs[0].Op.Name != "GET payment-gateway" {
		t.Fatalf("violations = %+v, want one named %q", vs, "GET payment-gateway")
	}
}
