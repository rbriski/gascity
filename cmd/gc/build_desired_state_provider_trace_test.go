package main

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// findProviderSkipDecision scans records for a TraceRecordDecision whose
// site code (either the normalised TraceSiteCode or the raw_site_code field)
// equals "build_desired_state.unresolvable_provider".
// Returns the first matching record and true, or zero value and false.
func findProviderSkipDecision(records []SessionReconcilerTraceRecord) (SessionReconcilerTraceRecord, bool) {
	for _, rec := range records {
		if rec.RecordType != TraceRecordDecision {
			continue
		}
		site := string(rec.SiteCode)
		if raw, ok := rec.Fields["raw_site_code"].(string); ok && raw != "" {
			site = raw
		}
		if site == "build_desired_state.unresolvable_provider" {
			return rec, true
		}
	}
	return SessionReconcilerTraceRecord{}, false
}

// assertProviderSkipDecision fails t if no trace decision matches
// "build_desired_state.unresolvable_provider" with reason "provider_not_found",
// outcome "skipped", and Fields["provider"] matching wantProvider.
func assertProviderSkipDecision(t *testing.T, records []SessionReconcilerTraceRecord, wantProvider string) {
	t.Helper()
	rec, ok := findProviderSkipDecision(records)
	if !ok {
		t.Fatalf("want trace decision with site_code %q, none found in %d records",
			"build_desired_state.unresolvable_provider", len(records))
	}

	// reason: "provider_not_found" (may be normalised to TraceReasonUnknown if constant not yet registered)
	reason := string(rec.ReasonCode)
	if raw, ok := rec.Fields["raw_reason_code"].(string); ok && raw != "" {
		reason = raw
	}
	if reason != "provider_not_found" {
		t.Errorf("trace decision reason = %q, want %q", reason, "provider_not_found")
	}

	// outcome: "skipped" is a registered constant (TraceOutcomeSkipped)
	if string(rec.OutcomeCode) != "skipped" {
		t.Errorf("trace decision outcome = %q, want %q", rec.OutcomeCode, "skipped")
	}

	// provider field must be populated
	if got, _ := rec.Fields["provider"].(string); got != wantProvider {
		t.Errorf("trace decision Fields[%q] = %q, want %q", "provider", got, wantProvider)
	}
}

// TestBuildDesiredState_UnknownProviderPoolNoStoreRecordsTraceDecision covers
// Site 1: the no-store pool path at ~line 527 of build_desired_state.go where
// resolveTemplatePrepared returns config.ErrProviderNotFound.
//
// Tests fail before builder adds errors.Is+trace.recordDecision at that site.
func TestBuildDesiredState_UnknownProviderPoolNoStoreRecordsTraceDecision(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		// Intentionally empty Providers map — "no-such-provider" is unknown.
		Agents: []config.Agent{{
			Name:              "worker",
			Provider:          "no-such-provider",
			MaxActiveSessions: intPtr(2),
			ScaleCheck:        "printf 1",
		}},
	}

	tracer := newSessionReconcilerTracer(cityPath, "test-city", io.Discard)
	defer tracer.Close() //nolint:errcheck
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), cfg)
	if cycle == nil {
		t.Fatal("BeginCycle returned nil — trace store could not be initialized")
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(),
		cfg, runtime.NewFake(), nil, nil, nil,
		cycle, io.Discard,
	)

	// Session must be absent from desired state.
	if len(result.State) != 0 {
		t.Fatalf("desired state size = %d, want 0: %#v", len(result.State), result.State)
	}

	assertProviderSkipDecision(t, cycle.records, "no-such-provider")
}

// TestBuildDesiredState_UnknownProviderNamedSessionRecordsTraceDecision covers
// Site 2: the named-session path at ~line 597 of build_desired_state.go.
//
// Tests fail before builder adds errors.Is+trace.recordDecision at that site.
func TestBuildDesiredState_UnknownProviderNamedSessionRecordsTraceDecision(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			Provider:          "no-such-provider",
			MaxActiveSessions: intPtr(1),
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}

	tracer := newSessionReconcilerTracer(cityPath, "test-city", io.Discard)
	defer tracer.Close() //nolint:errcheck
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), cfg)
	if cycle == nil {
		t.Fatal("BeginCycle returned nil — trace store could not be initialized")
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(),
		cfg, runtime.NewFake(), nil, nil, nil,
		cycle, io.Discard,
	)

	if len(result.State) != 0 {
		t.Fatalf("desired state size = %d, want 0: %#v", len(result.State), result.State)
	}

	assertProviderSkipDecision(t, cycle.records, "no-such-provider")
}

// TestBuildDesiredState_UnknownProviderSessionBeadRecordsTraceDecision covers
// Site 3: the bead/template path inside discoverSessionBeadsWithRoots at
// ~line 1598 of build_desired_state.go.  This path requires trace to be
// threaded through applySessionBeadDesiredOverlay and
// discoverSessionBeadsWithRoots (the ga-2jnm5r.2 signature change).
//
// Tests fail before builder:
//
//	(a) adds trace parameter to discoverSessionBeadsWithRoots / applySessionBeadDesiredOverlay
//	(b) adds errors.Is+trace.recordDecision at that call site
func TestBuildDesiredState_UnknownProviderSessionBeadRecordsTraceDecision(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			Provider:          "no-such-provider",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 0", // zero demand so pool pass never adds session
		}},
	}

	// A manual session bead for the "worker" template.  session_origin="manual"
	// ensures isManualSessionBead returns true, bypassing pool-skip guards.
	b, err := store.Create(beads.Bead{
		Title:  "worker manual session",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":   "worker-manual-001",
			"template":       "worker",
			"agent_name":     "worker",
			"state":          "asleep",
			"session_origin": "manual",
		},
	})
	if err != nil {
		t.Fatalf("create manual session bead: %v", err)
	}

	tracer := newSessionReconcilerTracer(cityPath, "test-city", io.Discard)
	defer tracer.Close() //nolint:errcheck
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), cfg)
	if cycle == nil {
		t.Fatal("BeginCycle returned nil — trace store could not be initialized")
	}

	sessionBeads, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(),
		cfg, runtime.NewFake(), store, nil, sessionBeads,
		cycle, io.Discard,
	)

	// Bead session must be absent from desired state.
	if _, ok := result.State["worker-manual-001"]; ok {
		t.Fatal("want session absent from desired state, but it was included")
	}
	if len(result.State) != 0 {
		t.Fatalf("desired state size = %d, want 0: %#v", len(result.State), result.State)
	}

	rec, ok := findProviderSkipDecision(cycle.records)
	if !ok {
		t.Fatalf("want trace decision with site_code %q, none found in %d records",
			"build_desired_state.unresolvable_provider", len(cycle.records))
	}

	// reason / outcome / provider
	reason := string(rec.ReasonCode)
	if raw, ok := rec.Fields["raw_reason_code"].(string); ok && raw != "" {
		reason = raw
	}
	if reason != "provider_not_found" {
		t.Errorf("trace decision reason = %q, want %q", reason, "provider_not_found")
	}
	if string(rec.OutcomeCode) != "skipped" {
		t.Errorf("trace decision outcome = %q, want %q", rec.OutcomeCode, "skipped")
	}
	if got, _ := rec.Fields["provider"].(string); got != "no-such-provider" {
		t.Errorf("trace decision Fields[%q] = %q, want %q", "provider", got, "no-such-provider")
	}

	// bead_id must be populated for Site 3 (differentiates it from Sites 1/2)
	if got, _ := rec.Fields["bead_id"].(string); got != b.ID {
		t.Errorf("trace decision Fields[%q] = %q, want bead ID %q", "bead_id", got, b.ID)
	}
}

// TestBuildDesiredState_UnknownProviderNilTraceSafe verifies that the pool
// no-store skip path does not panic when trace is nil, and that the existing
// stderr skip message is preserved.
func TestBuildDesiredState_UnknownProviderNilTraceSafe(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			Provider:          "no-such-provider",
			MaxActiveSessions: intPtr(2),
			ScaleCheck:        "printf 1",
		}},
	}

	var stderr strings.Builder
	// trace = nil — must not panic.
	result := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(),
		cfg, runtime.NewFake(), nil, nil, nil,
		nil, &stderr,
	)

	if len(result.State) != 0 {
		t.Fatalf("desired state size = %d, want 0: %#v", len(result.State), result.State)
	}

	// Existing skip message must be preserved so operators notice the skip.
	if got := stderr.String(); !strings.Contains(got, "buildDesiredState:") {
		t.Errorf("stderr = %q, want a buildDesiredState skip message", got)
	}
}
