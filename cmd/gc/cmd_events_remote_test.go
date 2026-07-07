package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/clientcontext"
)

// --api and a remote flag (--city-url/--context) both select a remote city and
// share the flag tier, so combining them is a loud conflict (gate G3).
func TestResolveEventsScope_ApiPlusRemoteFlagConflict(t *testing.T) {
	prev := contextFlag
	contextFlag = "prod"
	t.Cleanup(func() { contextFlag = prev })

	if _, err := resolveEventsScope("https://remote:9443"); err == nil ||
		!strings.Contains(err.Error(), "cannot combine --api") {
		t.Fatalf("want --api + --context conflict, got %v", err)
	}
}

// The core G3 property: a remote events scope (an explicit --api that is not the
// local supervisor) must never read the local .gc/events.jsonl on a 404 — that
// would be the local-disk fallback the design forbids.
func TestShouldUseLocalCityEventsFallback_RemoteScopeNeverReadsJsonl(t *testing.T) {
	scope := eventsAPIScope{cityPath: "/some/local/city", explicitAPI: true, localSupervisorAPI: false}
	notFound := &eventsAPIError{statusCode: http.StatusNotFound, detail: "city \"mc\" not found"}
	if shouldUseLocalCityEventsFallback(scope, notFound) {
		t.Fatal("a remote events scope must NOT fall back to .gc/events.jsonl on 404")
	}
}

// gc events under a remote context (no --api) is refused by the capability gate
// (via resolveDashboardContext -> resolveCity), never silently resolved local.
func TestResolveEventsScope_RemoteContextGated(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := doContextAdd(clientcontext.Context{Name: "prod", URL: "https://box:9443", City: "mc"}, &out, &errb); code != 0 {
		t.Fatalf("seed context: %q", errb.String())
	}
	prev := contextFlag
	contextFlag = "prod"
	t.Cleanup(func() { contextFlag = prev })

	if _, err := resolveEventsScope(""); err == nil ||
		!strings.Contains(err.Error(), "does not support a remote city") {
		t.Fatalf("gc events under a remote context must be gated, got %v", err)
	}
}
