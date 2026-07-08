package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/session"
)

// TestRouteMissingClassification pins the new-CLI/old-server hazard model: a 404
// with no problem+json body is a route-missing signal (old server's SPA / bare-
// mux catch-all), while a 404 that carries a problem+json body is a domain 404.
func TestRouteMissingClassification(t *testing.T) {
	pd := &genclient.ErrorModel{}
	cases := []struct {
		name   string
		status int
		pd     *genclient.ErrorModel
		wantRM bool
	}{
		{"404-no-body-route-missing", http.StatusNotFound, nil, true},
		{"404-with-problem-body-domain", http.StatusNotFound, pd, false},
		{"200-ok", http.StatusOK, nil, false},
		{"500-not-route-missing", http.StatusInternalServerError, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := routeMissingFromResponse(tc.status, tc.pd, "/waits")
			if got := err != nil; got != tc.wantRM {
				t.Fatalf("routeMissingFromResponse -> err=%v, want route-missing=%v", err, tc.wantRM)
			}
			if tc.wantRM {
				if !IsRouteMissing(err) {
					t.Errorf("IsRouteMissing=false for %v", err)
				}
				if !ShouldFallbackForRead(err) {
					t.Errorf("ShouldFallbackForRead=false for route-missing")
				}
				if r := FallbackReason(err); r != "route-missing" {
					t.Errorf("FallbackReason=%q, want route-missing", r)
				}
			}
		})
	}
}

// TestWaitInfoWireRoundTrip proves WaitInfo -> WaitView -> (JSON) ->
// genclient.WaitView -> WaitInfo preserves the fields the CLI renders, including
// the nil-vs-empty DepIDs and zero-CreatedAt distinctions.
func TestWaitInfoWireRoundTrip(t *testing.T) {
	cases := map[string]session.WaitInfo{
		"full": {
			ID:              "gc-wait-1",
			SessionID:       "gc-sess-1",
			SessionName:     "worker",
			Kind:            "deps",
			State:           "ready",
			DepIDs:          []string{"gc-1", "gc-2"},
			DepMode:         "all",
			RegisteredEpoch: "3",
			DeliveryAttempt: "2",
			NudgeID:         "wait-gc-wait-1-3-2",
			ExpiresAt:       "2026-05-16T09:30:00Z",
			Note:            "Continue.",
			Status:          "open",
			CreatedAt:       time.Date(2026, 3, 2, 4, 5, 6, 0, time.UTC),
			Labels:          []string{session.WaitBeadLabel, "session:gc-sess-1"},
		},
		"nil-deps-zero-created": {
			ID:        "gc-wait-2",
			SessionID: "gc-sess-2",
			Kind:      "deps",
			State:     "pending",
			Status:    "open",
			// DepIDs nil, CreatedAt zero
		},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			view := waitViewFromInfo(in)
			raw, err := json.Marshal(view)
			if err != nil {
				t.Fatalf("marshal WaitView: %v", err)
			}
			var g genclient.WaitView
			if err := json.Unmarshal(raw, &g); err != nil {
				t.Fatalf("unmarshal genclient.WaitView: %v", err)
			}
			out := waitInfoFromGen(g)
			if !reflect.DeepEqual(out, in) {
				t.Fatalf("round-trip mismatch:\n got  %#v\n want %#v", out, in)
			}
		})
	}
}

// TestNotAWaitErrorMessage locks the CLI-facing text the inspect ladder renders.
func TestNotAWaitErrorMessage(t *testing.T) {
	e := &NotAWaitError{ID: "gc-9"}
	if e.Error() != "gc-9 is not a wait" {
		t.Fatalf("NotAWaitError = %q", e.Error())
	}
}
