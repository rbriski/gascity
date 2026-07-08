package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/api"
)

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
	if !api.ShouldFallbackForRead(c, err) {
		logRoute(stderr, cmdName, "api", "error")
		fmt.Fprintf(stderr, "gc %s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	logRoute(stderr, cmdName, "fallback", api.FallbackReason(c, err))
	return localRender()
}
