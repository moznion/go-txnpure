package txnpure

import (
	"context"
	"log/slog"
)

// ScopeAttr is one string-keyed contextual value attached to a scope (or a
// single check) and carried into every Violation. Use it to tie a report back
// to the execution that produced it (trace ID, request ID, user ID).
type ScopeAttr struct {
	Key   string
	Value any
}

// Attr constructs a ScopeAttr.
func Attr(key string, value any) ScopeAttr { return ScopeAttr{Key: key, Value: value} }

// SlogAttrs converts scope attrs to log/slog attrs, for reporters built on
// slog. SlogReporter already applies it to the attrs it receives.
func SlogAttrs(attrs []ScopeAttr) []slog.Attr {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = slog.Any(a.Key, a.Value)
	}
	return out
}

// WithScopeAttrs attaches static attrs to a single scope at StartScope /
// InScope — for values the caller already has at hand:
//
//	ctx, finish := detector.StartScope(ctx, "CreateUser",
//		txnpure.WithScopeAttrs(txnpure.Attr("user_id", userID)))
//
// They are appended after any attrs produced by WithScopeAttrsFunc.
// Duplicate keys are kept in order, never deduplicated.
func WithScopeAttrs(attrs ...ScopeAttr) ScopeOption {
	return func(s *scope) { s.attrs = append(s.attrs, attrs...) }
}

// WithCheckAttrs attaches attrs to a single Check / Do call. They come after
// the scope's attrs in Violation.Attrs.
func WithCheckAttrs(attrs ...ScopeAttr) CheckOption {
	return func(c *checkConfig) { c.attrs = append(c.attrs, attrs...) }
}

// WithScopeAttrsFunc installs a detector-level extractor that derives attrs
// from the context — the middleware-friendly way to stamp every scope with
// trace/request IDs: set it up once and every Violation carries them for
// free.
//
// f is evaluated once per scope at StartScope (never per check), with the
// context StartScope received; its attrs come first, followed by any
// per-scope WithScopeAttrs and per-check WithCheckAttrs.
func WithScopeAttrsFunc(f func(ctx context.Context) []ScopeAttr) Option {
	return func(d *Detector) { d.attrsFunc = f }
}
