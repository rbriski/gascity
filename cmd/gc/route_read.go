package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/api"
)

// fallbackAfterFetch is a sentinel an apiFetch closure may return to force a
// fallback to the local path AFTER a successful API round-trip — for a command
// whose response can indicate "the API can't serve this, use the richer local
// path" (e.g. convoy status on a graph/workflow convoy). routeRead renders it as
// route=fallback reason=<Reason>, bypassing error classification.
type fallbackAfterFetch struct{ Reason string }

func (f fallbackAfterFetch) Error() string { return "fallback-after-fetch: " + f.Reason }

// errorAfterFetch is a sentinel an apiFetch closure may return to force a hard
// api error (route=api reason=error, exit 1) AFTER a successful round-trip — for
// a response that is itself an error condition (e.g. mail count partial results).
// routeRead prints "gc <cmd>: <Detail>", bypassing fallback classification.
type errorAfterFetch struct{ Detail string }

func (e errorAfterFetch) Error() string { return e.Detail }

// routeRead runs the canonical read-path routing ladder shared by every routed
// read command, so the "try the API, classify the error, fall back to the local
// path" fork lives in ONE place instead of being copy-pasted per command. It is
// the collapse of the six-row matrix's routing logic onto a single helper.
//
//   - c == nil: the controller is down or the escape hatch is set — take the
//     local path, logging route=fallback reason=<nilReason>.
//   - c != nil: run apiFetch. On success, log route=api and render via apiRender.
//     On a non-fallbackable error (a remote city never falls back — gate G1),
//     log route=api reason=error, print the error, and exit 1. On a fallbackable
//     error, log route=fallback reason=<classified> and take the local path.
//
// apiFetch performs only the API round-trip(s) and stashes results in closure
// state; apiRender renders them. Keeping fetch and render separate preserves the
// exact stderr ordering — the single route= line precedes any render output.
func routeRead(c *api.Client, cmdName, nilReason string, stderr io.Writer, apiFetch func() error, apiRender func() int, localRender func() int) int {
	if c == nil {
		logRoute(stderr, cmdName, "fallback", nilReason)
		return localRender()
	}
	err := apiFetch()
	if err == nil {
		logRoute(stderr, cmdName, "api", "")
		return apiRender()
	}
	var faf fallbackAfterFetch
	if errors.As(err, &faf) {
		logRoute(stderr, cmdName, "fallback", faf.Reason)
		return localRender()
	}
	var eaf errorAfterFetch
	if errors.As(err, &eaf) || !api.ShouldFallbackForRead(c, err) {
		logRoute(stderr, cmdName, "api", "error")
		fmt.Fprintf(stderr, "gc %s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	logRoute(stderr, cmdName, "fallback", api.FallbackReason(c, err))
	return localRender()
}
