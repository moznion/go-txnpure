package txnpure

import "net/http"

// RoundTripperOption configures WrapRoundTripper.
type RoundTripperOption func(*wrappedRoundTripper)

// WithRequestName replaces the default op-name derivation (Method + " " +
// Host). Keep the result a small, code-defined set — never raw URLs with IDs
// — because the op name is half of the violation identity.
func WithRequestName(f func(*http.Request) string) RoundTripperOption {
	return func(w *wrappedRoundTripper) { w.name = f }
}

// WrapRoundTripper wraps an http.RoundTripper so that every outgoing request
// runs a Check with kind "http" before it is sent. A nil rt means
// http.DefaultTransport. Install it on the http.Client your application
// uses:
//
//	client := &http.Client{Transport: detector.WrapRoundTripper(nil)}
//
// The request is never blocked or delayed; a violating request is reported
// and then sent as usual.
func (d *Detector) WrapRoundTripper(rt http.RoundTripper, opts ...RoundTripperOption) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	w := &wrappedRoundTripper{det: d, rt: rt, name: defaultRequestName}
	for _, o := range opts {
		o(w)
	}
	return w
}

type wrappedRoundTripper struct {
	det  *Detector
	rt   http.RoundTripper
	name func(*http.Request) string
}

func (w *wrappedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	w.det.Check(req.Context(), Op{Kind: "http", Name: w.name(req)})
	return w.rt.RoundTrip(req)
}

func defaultRequestName(req *http.Request) string {
	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	return req.Method + " " + host
}
