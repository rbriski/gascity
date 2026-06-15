package main

import "testing"

// TestHookClaimCityPathPrefersGCCity proves the hook claim resolves the city path
// from GC_CITY in the agent env (so it can pick the controller API client),
// falling back to the working dir when GC_CITY is absent or empty.
func TestHookClaimCityPathPrefersGCCity(t *testing.T) {
	if got := hookClaimCityPath("/work", []string{"X=1", "GC_CITY=/city/path", "Y=2"}); got != "/city/path" {
		t.Fatalf("hookClaimCityPath(GC_CITY set) = %q, want /city/path", got)
	}
	if got := hookClaimCityPath("/work", []string{"X=1"}); got != "/work" {
		t.Fatalf("hookClaimCityPath(no GC_CITY) = %q, want /work fallback", got)
	}
	if got := hookClaimCityPath("/work", []string{"GC_CITY=  "}); got != "/work" {
		t.Fatalf("hookClaimCityPath(blank GC_CITY) = %q, want /work fallback", got)
	}
}
