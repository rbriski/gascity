package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/session"
)

// TestSessionClassifierInfoEquivalence is the byte-identical oracle for P2 of
// NONWORK-BEAD-FIELDDOOR-PLAN.md. Each converted classifier has a *Info sibling
// that reads typed session.Info fields instead of raw bead metadata. For every
// representative session-bead shape, the Info form (fed
// session.InfoFromPersistedBead(b)) must agree with the original bead form.
//
// This proves the Info projection plus the predicate mirror are semantically
// identical to the existing metadata reads, so later caller migration (P4) is
// safe. Any divergence here is a real fidelity bug in the codec or a mirror.
func TestSessionClassifierInfoEquivalence(t *testing.T) {
	pastRFC3339 := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	futureRFC3339 := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	clk := &clock.Fake{Time: time.Now()}

	beadsByShape := map[string]beads.Bead{
		"bare": {
			ID:     "ga-bare",
			Type:   session.BeadType,
			Title:  "bare",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template": "worker",
			},
		},
		"pool-managed-slot": {
			ID:     "ga-pool",
			Type:   session.BeadType,
			Title:  "worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "frontend/worker",
				"agent_name":   "frontend/worker-1",
				"pool_managed": "true",
				"pool_slot":    "1",
				"state":        "awake",
				"session_name": "worker-ga-pool",
			},
		},
		"pool-managed-flag-only": {
			ID:     "ga-poolflag",
			Type:   session.BeadType,
			Title:  "worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"pool_managed": "true",
				"state":        "active",
			},
		},
		"ephemeral-origin": {
			ID:     "ga-eph",
			Type:   session.BeadType,
			Title:  "eph",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":       "worker",
				"session_origin": "ephemeral",
			},
		},
		"ephemeral-via-pool-slot-name": {
			ID:     "ga-ephname",
			Type:   session.BeadType,
			Title:  "worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"session_name": "worker-3",
			},
		},
		"named": {
			ID:     "ga-named",
			Type:   session.BeadType,
			Title:  "mayor",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                  "mayor",
				"configured_named_session":  "true",
				"configured_named_identity": "mayor",
				"configured_named_mode":     "singleton",
				"common_name":               "mayor",
				"alias":                     "mayor",
				"session_name":              "mayor",
				"alias_history":             "mayor,boss",
			},
		},
		"manual": {
			ID:     "ga-manual",
			Type:   session.BeadType,
			Title:  "manual",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":       "worker",
				"manual_session": "true",
			},
		},
		"manual-padded-true": {
			// Edge: isManualSessionBead compares manual_session WITHOUT trimming,
			// so a padded "true" must read as NOT manual on both forms.
			ID:     "ga-manualpad",
			Type:   session.BeadType,
			Title:  "manual",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":       "worker",
				"manual_session": "  true  ",
			},
		},
		"manual-origin": {
			ID:     "ga-manualorigin",
			Type:   session.BeadType,
			Title:  "manual",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":       "worker",
				"session_origin": "manual",
			},
		},
		"drained-state": {
			ID:     "ga-drained",
			Type:   session.BeadType,
			Title:  "drained",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":  "worker",
				"state":     "drained",
				"pool_slot": "2",
			},
		},
		"drained-via-asleep": {
			ID:     "ga-drainasleep",
			Type:   session.BeadType,
			Title:  "drained",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"state":        "asleep",
				"sleep_reason": "drained",
				"pool_slot":    "2",
			},
		},
		"asleep-idle-freeable": {
			ID:     "ga-idle",
			Type:   session.BeadType,
			Title:  "idle",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"state":        "asleep",
				"sleep_reason": "idle",
				"pool_slot":    "2",
			},
		},
		"asleep-wait-hold-not-freeable": {
			ID:     "ga-wait",
			Type:   session.BeadType,
			Title:  "wait",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"state":        "asleep",
				"sleep_reason": "wait-hold",
				"pool_slot":    "2",
			},
		},
		"failed-create": {
			ID:     "ga-failed",
			Type:   session.BeadType,
			Title:  "failed",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"state":        string(session.StateFailedCreate),
				"pool_managed": "true",
			},
		},
		"pending-pool-create": {
			ID:     "ga-pending",
			Type:   session.BeadType,
			Title:  "pending",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":             "worker",
				"pool_managed":         "true",
				"pool_slot":            "1",
				"state":                string(session.StateStartPending),
				"pending_create_claim": "true",
			},
		},
		"stale-creating-old-marker": {
			ID:        "ga-stale",
			Type:      session.BeadType,
			Title:     "stale",
			Labels:    []string{session.LabelSession},
			CreatedAt: time.Now().Add(-90 * time.Minute),
			Metadata: map[string]string{
				"template":                  "worker",
				"state":                     string(session.StateStartPending),
				"pending_create_started_at": pastRFC3339,
			},
		},
		"fresh-creating": {
			ID:        "ga-fresh",
			Type:      session.BeadType,
			Title:     "fresh",
			Labels:    []string{session.LabelSession},
			CreatedAt: time.Now(),
			Metadata: map[string]string{
				"template":                  "worker",
				"state":                     string(session.StateStartPending),
				"pending_create_started_at": futureRFC3339,
			},
		},
		"quarantined-active": {
			ID:     "ga-quar",
			Type:   session.BeadType,
			Title:  "quar",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":          "worker",
				"state":             "quarantined",
				"quarantined_until": futureRFC3339,
				"wake_attempts":     "3",
			},
		},
		"quarantine-expired": {
			ID:     "ga-quarexp",
			Type:   session.BeadType,
			Title:  "quar",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":          "worker",
				"quarantined_until": pastRFC3339,
				"wake_attempts":     "1",
			},
		},
		"agent-label-fallback": {
			ID:     "ga-label",
			Type:   session.BeadType,
			Title:  "labeled",
			Labels: []string{session.LabelSession, "agent:scout"},
			Metadata: map[string]string{
				"template": "scout",
			},
		},
		"dependency-only": {
			ID:     "ga-dep",
			Type:   session.BeadType,
			Title:  "dep",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":        "worker",
				"dependency_only": "true",
			},
		},
		"unknown-state": {
			ID:     "ga-unknown",
			Type:   session.BeadType,
			Title:  "unknown",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template": "worker",
				"state":    "some-future-state",
			},
		},
		"closed": {
			ID:     "ga-closed",
			Type:   session.BeadType,
			Title:  "closed",
			Status: "closed",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"state":        string(session.StateFailedCreate),
				"pool_managed": "true",
				"pool_slot":    "1",
			},
		},
		"no-session-name-pool": {
			// Exercises the SessionNameMetadata-vs-SessionName divergence: the
			// raw session_name is empty, so beadOwnsPoolSessionName /
			// sessionBeadAssigneeIdentities must NOT see the sessionNameFor(ID)
			// fallback that Info.SessionName applies.
			ID:     "ga-noname",
			Type:   session.BeadType,
			Title:  "noname",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":  "worker",
				"pool_slot": "1",
			},
		},
		"owns-pool-session-name": {
			ID:     "ga-owns",
			Type:   session.BeadType,
			Title:  "owns",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"session_name": PoolSessionName("worker", "ga-owns"),
				"pool_slot":    "1",
			},
		},
		"acp-transport": {
			ID:     "ga-acptransport",
			Type:   session.BeadType,
			Title:  "acp",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":  "worker",
				"transport": "acp",
			},
		},
		"acp-provider": {
			ID:     "ga-acpprovider",
			Type:   session.BeadType,
			Title:  "acp",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template": "worker",
				"provider": "acp",
			},
		},
		"acp-mcp-identity": {
			ID:     "ga-acpmcpid",
			Type:   session.BeadType,
			Title:  "acp",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                     "worker",
				session.MCPIdentityMetadataKey: "mayor",
			},
		},
		"acp-mcp-snapshot": {
			ID:     "ga-acpmcpsnap",
			Type:   session.BeadType,
			Title:  "acp",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                            "worker",
				session.MCPServersSnapshotMetadataKey: "{}",
			},
		},
		"non-acp-transport": {
			ID:     "ga-nonacp",
			Type:   session.BeadType,
			Title:  "tmux",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":  "worker",
				"transport": "tmux",
			},
		},
	}

	const tmpl = "worker"

	boolChecks := map[string]struct {
		bead func(beads.Bead) bool
		info func(session.Info) bool
	}{
		"isPoolManagedSessionBead":  {isPoolManagedSessionBead, isPoolManagedSessionInfo},
		"isEphemeralSessionBead":    {isEphemeralSessionBead, isEphemeralSessionInfo},
		"isManualSessionBead":       {isManualSessionBead, isManualSessionInfo},
		"isNamedSessionBead":        {isNamedSessionBead, isNamedSessionInfo},
		"isDrainedSessionBead":      {isDrainedSessionBead, isDrainedSessionInfo},
		"isFailedCreateSessionBead": {isFailedCreateSessionBead, isFailedCreateSessionInfo},
		"isPendingPoolCreate":       {isPendingPoolCreate, isPendingPoolCreateInfo},
		"isStaleCreating":           {isStaleCreating, isStaleCreatingInfo},
		"isKnownState":              {isKnownState, isKnownStateInfo},
		"isPoolSessionSlotFreeable": {isPoolSessionSlotFreeable, isPoolSessionSlotFreeableInfo},
		"beadOwnsPoolSessionName":   {beadOwnsPoolSessionName, infoOwnsPoolSessionName},
	}

	stringChecks := map[string]struct {
		bead func(beads.Bead) string
		info func(session.Info) string
	}{
		"sessionOrigin":                {sessionOrigin, sessionOriginInfo},
		"sessionMetadataState":         {sessionMetadataState, sessionMetadataStateInfo},
		"sessionBeadStoredTemplate":    {sessionBeadStoredTemplate, sessionBeadStoredTemplateInfo},
		"sessionBeadAgentName":         {sessionBeadAgentName, sessionBeadAgentNameInfo},
		"stampedPoolQualifiedIdentity": {stampedPoolQualifiedIdentity, stampedPoolQualifiedIdentityInfo},
	}

	intChecks := map[string]struct {
		bead func(beads.Bead) int
		info func(session.Info) int
	}{
		"sessionWakeAttempts": {sessionWakeAttempts, sessionWakeAttemptsInfo},
	}

	sliceChecks := map[string]struct {
		bead func(beads.Bead) []string
		info func(session.Info) []string
	}{
		"sessionBeadAssigneeIdentities": {sessionBeadAssigneeIdentities, sessionBeadAssigneeIdentitiesInfo},
	}

	// classifiers that take cfg (nil here) and/or a template argument.
	cfgBoolChecks := map[string]struct {
		bead func(beads.Bead) bool
		info func(session.Info) bool
	}{
		"isCanonicalPoolManagedSessionBeadForTemplate": {
			func(b beads.Bead) bool { return isCanonicalPoolManagedSessionBeadForTemplate(b, tmpl) },
			func(i session.Info) bool { return isCanonicalPoolManagedSessionInfoForTemplate(i, tmpl) },
		},
		// nil cfg exercises the transport / provider=="acp" / MCP-key branches;
		// the cfg-dependent agent/provider resolution is out of the codec's scope.
		"beadUsesACPTransport": {
			func(b beads.Bead) bool { return beadUsesACPTransport(b, nil) },
			func(i session.Info) bool { return infoUsesACPTransport(i, nil) },
		},
	}

	cfgStringChecks := map[string]struct {
		bead func(beads.Bead) string
		info func(session.Info) string
	}{
		"resolvedSessionTemplate": {
			func(b beads.Bead) string { return resolvedSessionTemplate(b, nil) },
			func(i session.Info) string { return resolvedSessionTemplateInfo(i, nil) },
		},
		"normalizedSessionTemplate": {
			func(b beads.Bead) string { return normalizedSessionTemplate(b, nil) },
			func(i session.Info) string { return normalizedSessionTemplateInfo(i, nil) },
		},
	}

	clkBoolChecks := map[string]struct {
		bead func(beads.Bead) bool
		info func(session.Info) bool
	}{
		"sessionIsQuarantined": {
			func(b beads.Bead) bool { return sessionIsQuarantined(b, clk) },
			func(i session.Info) bool { return sessionIsQuarantinedInfo(i, clk) },
		},
	}

	for shape, b := range beadsByShape {
		b := b
		info := session.InfoFromPersistedBead(b)
		t.Run(shape, func(t *testing.T) {
			for name, c := range boolChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%v bead=%v", name, got, want)
				}
			}
			for name, c := range cfgBoolChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%v bead=%v", name, got, want)
				}
			}
			for name, c := range clkBoolChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%v bead=%v", name, got, want)
				}
			}
			for name, c := range stringChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%q bead=%q", name, got, want)
				}
			}
			for name, c := range cfgStringChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%q bead=%q", name, got, want)
				}
			}
			for name, c := range intChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%d bead=%d", name, got, want)
				}
			}
			for name, c := range sliceChecks {
				if got, want := c.info(info), c.bead(b); !reflect.DeepEqual(got, want) {
					t.Errorf("%s: info=%v bead=%v", name, got, want)
				}
			}
		})
	}
}
