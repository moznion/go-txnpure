package txnpure

import (
	"runtime"
	"strings"
)

// StackFrame is one resolved frame of the stack captured at Check time.
type StackFrame struct {
	Function string
	File     string
	Line     int
}

const modulePath = "github.com/moznion/go-txnpure"

// captureStack captures up to depth frames of the caller's stack, skipping
// the leading txnpure-internal frames (Check/Do/RoundTripper plumbing) so the
// first frame points at the instrumented call site. depth <= 0 disables
// capture.
func captureStack(depth int) []StackFrame {
	if depth <= 0 {
		return nil
	}
	// Room for the internal frames that get skipped before user code starts.
	pcs := make([]uintptr, depth+16)
	n := runtime.Callers(2, pcs)
	if n == 0 {
		return nil
	}
	frames := runtime.CallersFrames(pcs[:n])
	out := make([]StackFrame, 0, depth)
	skipping := true
	for {
		f, more := frames.Next()
		if skipping && isInternalFrame(f) {
			if !more {
				break
			}
			continue
		}
		skipping = false
		out = append(out, StackFrame{Function: f.Function, File: f.File, Line: f.Line})
		if len(out) >= depth || !more {
			break
		}
	}
	return out
}

// isInternalFrame reports whether the frame belongs to txnpure itself (or a
// txnpure child module such as the grpc adapter). Frames from _test.go files
// are never internal, so in-package tests still see themselves in stacks.
func isInternalFrame(f runtime.Frame) bool {
	if strings.HasSuffix(f.File, "_test.go") {
		return false
	}
	return strings.HasPrefix(f.Function, modulePath+".") || strings.HasPrefix(f.Function, modulePath+"/")
}
