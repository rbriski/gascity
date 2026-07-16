package main

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
)

// swCreate creates a bead and returns it (MemStore assigns the id and forces
// Status="open", so callers thread the returned id and close explicitly).
func swCreate(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("Create(%s): %v", b.Title, err)
	}
	return created
}

func swClose(t *testing.T, store beads.Store, id string) {
	t.Helper()
	if err := store.Close(id); err != nil {
		t.Fatalf("Close(%s): %v", id, err)
	}
}

// newGraphWorkflow materializes a graph.v2 workflow the way mol-polecat-work
// does: a synthetic one-item input convoy tracking a source work bead, a
// workflow root linked via gc.input_convoy_id, and one open worker step routed
// to routedTo (the polecat pool). It returns the root and step ids and the
// source id. When sourceClosed is true the source work bead is closed (the
// refinery-merged state that strands the workflow).
func newGraphWorkflow(t *testing.T, store beads.Store, routedTo string, sourceClosed bool) (rootID, stepID string) {
	t.Helper()
	src := swCreate(t, store, beads.Bead{Title: "the work", Type: "task"})
	convoy := swCreate(t, store, beads.Bead{Title: "input convoy", Type: "convoy"})
	if err := convoycore.TrackItem(store, convoy.ID, src.ID); err != nil {
		t.Fatalf("TrackItem(%s -> %s): %v", convoy.ID, src.ID, err)
	}
	root := swCreate(t, store, beads.Bead{
		Title: "mol-polecat-work", Type: "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.InputConvoyIDMetadataKey:   convoy.ID,
			beadmeta.RoutedToMetadataKey:        routedTo,
		},
	})
	step := swCreate(t, store, beads.Bead{
		Title: "submit-and-exit", Type: "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			beadmeta.StepRefMetadataKey:    "mol-polecat-work.submit-and-exit",
			beadmeta.RoutedToMetadataKey:   routedTo,
		},
	})
	if sourceClosed {
		swClose(t, store, src.ID)
	}
	return root.ID, step.ID
}

func statusOf(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b.Status
}

// TestReapSourceTerminalWorkflowsClosesStaleRootWithOpenStep is the headline
// regression: a mol-polecat-work workflow whose source work merged and closed
// must self-finalize even though its submit-and-exit step is still open. The
// existing subtree-terminal reapers leave it open; the source-terminal reaper
// closes root + step so it stops generating pool demand.
func TestReapSourceTerminalWorkflowsClosesStaleRootWithOpenStep(t *testing.T) {
	store := beads.NewMemStore()
	rootID, stepID := newGraphWorkflow(t, store, "rig-A/executor", true /*sourceClosed*/)

	reaped := reapSourceTerminalWorkflows([]beads.Store{store}, io.Discard)
	if reaped < 2 {
		t.Fatalf("reaped = %d, want >= 2 (root + open step)", reaped)
	}
	if got := statusOf(t, store, rootID); got != "closed" {
		t.Errorf("root status = %q, want closed", got)
	}
	if got := statusOf(t, store, stepID); got != "closed" {
		t.Errorf("submit-and-exit step status = %q, want closed", got)
	}
}

// TestReapSourceTerminalWorkflowsLeavesLiveSourceOpen guards against
// false-positives: a workflow whose source work is still open is live and must
// never be force-finalized.
func TestReapSourceTerminalWorkflowsLeavesLiveSourceOpen(t *testing.T) {
	store := beads.NewMemStore()
	rootID, stepID := newGraphWorkflow(t, store, "rig-A/executor", false /*sourceClosed*/)

	reaped := reapSourceTerminalWorkflows([]beads.Store{store}, io.Discard)
	if reaped != 0 {
		t.Fatalf("reaped = %d, want 0 (live source must not be reaped)", reaped)
	}
	if got := statusOf(t, store, rootID); got != "open" {
		t.Errorf("root status = %q, want open", got)
	}
	if got := statusOf(t, store, stepID); got != "open" {
		t.Errorf("step status = %q, want open", got)
	}
}

// TestReapSourceTerminalWorkflowsLifeOSMultipleRoots covers the LifeOS
// recurrence: several graph.v2 roots each over their own closed one-item input
// convoy, each still carrying open steps. Every stale root must self-finalize.
func TestReapSourceTerminalWorkflowsLifeOSMultipleRoots(t *testing.T) {
	store := beads.NewMemStore()
	root1, step1 := newGraphWorkflow(t, store, "rig-A/executor", true)
	root2, step2 := newGraphWorkflow(t, store, "rig-A/executor", true)
	// A third workflow whose source is still open must survive.
	liveRoot, liveStep := newGraphWorkflow(t, store, "rig-A/executor", false)

	reapSourceTerminalWorkflows([]beads.Store{store}, io.Discard)

	for _, id := range []string{root1, step1, root2, step2} {
		if got := statusOf(t, store, id); got != "closed" {
			t.Errorf("stale bead %s status = %q, want closed", id, got)
		}
	}
	for _, id := range []string{liveRoot, liveStep} {
		if got := statusOf(t, store, id); got != "open" {
			t.Errorf("live bead %s status = %q, want open", id, got)
		}
	}
}

// TestReapSourceTerminalWorkflowsSkipsStepBeads guards that the reaper only
// treats workflow roots as roots: a step bead (gc.root_bead_id, no
// input-convoy/source link) is never force-closed on its own even if it
// carries a stale look, so we never close a step out from under a live root.
func TestReapSourceTerminalWorkflowsSkipsStepBeads(t *testing.T) {
	store := beads.NewMemStore()
	// A lone step-shaped bead: has gc.root_bead_id but no root markers and no
	// source link.
	step := swCreate(t, store, beads.Bead{
		Title: "orphan step", Type: "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: "some-root",
			beadmeta.RoutedToMetadataKey:   "rig-A/executor",
		},
	})

	reaped := reapSourceTerminalWorkflows([]beads.Store{store}, io.Discard)
	if reaped != 0 {
		t.Fatalf("reaped = %d, want 0 (a bare step is not a reapable root)", reaped)
	}
	if got := statusOf(t, store, step.ID); got != "open" {
		t.Errorf("step status = %q, want open", got)
	}
}

// TestReapThenDemandDropsStaleWorkflowPoolDemand is the controller scale/claim
// regression (ga-tum acceptance): after the reaper runs in the controller tick,
// a stale graph.v2 workflow contributes ZERO pool demand while a separate valid
// workflow remains claimable. This is the exact "cannot recreate or occupy a
// polecat after an acknowledged drain, while a separate valid workflow remains
// claimable" invariant, proven through the real buildDesiredStateWithSessionBeads
// demand path. Two pools keep the assertion independent of how many beads each
// workflow routes.
func TestReapThenDemandDropsStaleWorkflowPoolDemand(t *testing.T) {
	cfg, cityStore, rigStores, poolStale := newNoScaleCheckRigPoolCity(t)
	rigStore := rigStores["rig-A"]

	// Add a second cold pool in the same rig for the live workflow.
	maxSess, minSess := 5, 0
	poolLive := "rig-A/reviewer"
	cfg.Agents = append(cfg.Agents, config.Agent{
		Name:              "reviewer",
		MaxActiveSessions: &maxSess,
		MinActiveSessions: &minSess,
		Dir:               "rig-A",
		Provider:          "mock",
	})

	// A stale workflow (source merged/closed) routed to poolStale, and a live
	// workflow (source still open) routed to poolLive, both in the rig store.
	newGraphWorkflow(t, rigStore, poolStale, true /*sourceClosed*/)
	newGraphWorkflow(t, rigStore, poolLive, false /*sourceClosed*/)

	// Before the reap the stale workflow occupies a poolStale slot — the bug.
	before := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
	)
	if got := before.ScaleCheckCounts[poolStale]; got < 1 {
		t.Fatalf("pre-reap poolStale demand = %d, want >= 1 (stale workflow occupies a slot before the fix)", got)
	}
	liveDemand := before.ScaleCheckCounts[poolLive]
	if liveDemand < 1 {
		t.Fatalf("pre-reap poolLive demand = %d, want >= 1 (live workflow is claimable)", liveDemand)
	}

	// The controller tick reaps source-terminal workflows before it computes
	// demand.
	reapSourceTerminalWorkflows([]beads.Store{cityStore, rigStore}, io.Discard)

	after := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
	)
	if got := after.ScaleCheckCounts[poolStale]; got != 0 {
		t.Fatalf("post-reap poolStale demand = %d, want 0 (stale workflow must not occupy a polecat)", got)
	}
	if got := after.ScaleCheckCounts[poolLive]; got != liveDemand {
		t.Fatalf("post-reap poolLive demand = %d, want %d (a separate valid workflow stays claimable)", got, liveDemand)
	}
}
