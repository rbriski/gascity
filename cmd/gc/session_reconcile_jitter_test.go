// Tests for the #3279 fix: de-correlated (full-jitter) rate-limit quarantine
// and THROTTLED-provider respawn pacing. The regression was a synchronized
// retry storm — sessions re-quarantined in lockstep and respawned together on
// the single tick a provider cleared. These tests pin the de-correlation at the
// level of the building blocks the reconciler wires together (the gate switch
// in session_reconciler.go composes recordProviderThrottlePace with the
// snapshot tri-state and the anonymous-pool guard, each covered here).

package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
)

func TestDeterministicFullJitter_DisabledAndBounds(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)

	if got := deterministicFullJitter("s1", now, 0); got != 0 {
		t.Fatalf("spread=0 should disable jitter, got %s", got)
	}
	if got := deterministicFullJitter("s1", now, -time.Minute); got != 0 {
		t.Fatalf("negative spread should disable jitter, got %s", got)
	}

	spread := 30 * time.Minute
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		got := deterministicFullJitter(id, now, spread)
		if got < 0 || got >= spread {
			t.Fatalf("jitter(%q) = %s, want in [0, %s)", id, got, spread)
		}
	}

	// Stable for the same (key, now): a given session/tick re-derives the same
	// offset (no hidden RNG state).
	if a, b := deterministicFullJitter("s1", now, spread), deterministicFullJitter("s1", now, spread); a != b {
		t.Fatalf("offset not stable for same key+now: %s vs %s", a, b)
	}
}

func TestRateLimitQuarantineUntil_DeCorrelatesHerd(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	floor := now.Add(defaultRateLimitQuarantineDuration)
	ceil := floor.Add(rateLimitQuarantineJitter)

	// Discriminating assertion: two sessions quarantined on the SAME tick must
	// get DISTINCT wake instants. This fails iff the durations are equal — the
	// old flat-30m lockstep behavior the regression describes.
	a := rateLimitQuarantineUntil("session-a", now)
	b := rateLimitQuarantineUntil("session-b", now)
	if a.Equal(b) {
		t.Fatalf("two sessions on the same tick got identical wake instant %s — herd not de-correlated", a)
	}
	for _, u := range []time.Time{a, b} {
		if u.Before(floor) || !u.Before(ceil) {
			t.Fatalf("wake instant %s outside [%s, %s)", u, floor, ceil)
		}
	}
}

// TestRecordProviderThrottlePace_StaggersWithinWindow is the respawn-gate's
// THROTTLED pacing assertion at the building-block level: N sessions paced on
// the same tick get wake instants spread across [now, now+spread) rather than
// all on this tick — the de-correlation the gate relies on.
func TestRecordProviderThrottlePace_StaggersWithinWindow(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	untils := make([]time.Time, 0, 3)
	for _, id := range []string{"p1", "p2", "p3"} {
		store := newTestStore()
		session := makeBead(id, map[string]string{"state": "active"})
		if err := recordProviderThrottlePace(&session, store, clk); err != nil {
			t.Fatalf("recordProviderThrottlePace(%s): %v", id, err)
		}
		if got := session.Metadata["sleep_reason"]; got != "rate_limit" {
			t.Fatalf("sleep_reason = %q, want rate_limit", got)
		}
		if got := session.Metadata["state"]; got != "asleep" {
			t.Fatalf("state = %q, want asleep", got)
		}
		u, err := time.Parse(time.RFC3339, session.Metadata["quarantined_until"])
		if err != nil {
			t.Fatalf("quarantined_until parse: %v", err)
		}
		if u.Before(now) || !u.Before(now.Add(providerThrottlePaceSpread)) {
			t.Fatalf("paced until %s outside [%s, %s)", u, now, now.Add(providerThrottlePaceSpread))
		}
		untils = append(untils, u)
	}
	// At least two of the three differ — the pace staggers the herd rather than
	// holding everyone to one instant.
	if untils[0].Equal(untils[1]) && untils[1].Equal(untils[2]) {
		t.Fatalf("throttle pace produced identical holds %v — not staggered", untils)
	}
}

// TestRecordProviderThrottlePace_PropagatesWriteError pins theme #1: the pace
// path must surface a metadata-write failure, never swallow it.
func TestRecordProviderThrottlePace_PropagatesWriteError(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	store.metadataBatchErr = errors.New("batch failed")
	session := makeBead("p1", map[string]string{"state": "active"})
	if err := recordProviderThrottlePace(&session, store, clk); err == nil {
		t.Fatal("expected the write error to propagate")
	}
}

// TestProviderThrottlePace_OnlyAnonymousPoolSessions documents the theme #4
// guard the gate applies: the reconciler paces only anonymous pool sessions
// (namedSessionIdentity()==""), never named / infrastructure ones.
func TestProviderThrottlePace_OnlyAnonymousPoolSessions(t *testing.T) {
	pool := makeBead("pool-1", map[string]string{
		"session_name": PoolSessionName("repo/worker", "pool-1"),
		"template":     "repo/worker",
	})
	if id := namedSessionIdentity(pool); id != "" {
		t.Fatalf("pool session identity = %q, want empty (anonymous → paceable)", id)
	}

	named := makeBead("ctrl-1", map[string]string{
		"session_name":               "trace-town-dispatcher",
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "dispatcher",
	})
	if id := namedSessionIdentity(named); id == "" {
		t.Fatal("named/infrastructure session identity is empty; it would be paced under throttle (theme #4 violation)")
	}
}
