package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// tbPreserveWorkerSessionBead is the open pool session bead a mid-do Lumen worker
// holds: session_name worker-a, backing template rig/claude — the shape the resume
// tier and stampRunSessionIdentity match the claimed "hello" fold row against.
func tbPreserveWorkerSessionBead() beads.Bead {
	return beads.Bead{
		ID:     "sess-1",
		Title:  "worker-a",
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:" + tbHookRoute},
		Metadata: map[string]string{
			"session_name":         "worker-a",
			"template":             tbHookRoute,
			poolManagedMetadataKey: boolMetadata(true),
		},
	}
}

// TestPreserveTierDoesNotDrainMidDoSession is the §3.2 pin (the DRAIN bug): a
// claimed Tier-B row, wired in through the S11 append, must survive the REAL
// production pool-demand filter (the step that drops it today) and then drive a
// resume-tier request that keeps the worker's session alive — never writing to the
// write-closed journal.
//
// The load-bearing assertion is the filter-survival pin: earlier this test fed the
// raw appended slice straight to ComputePoolDesiredStates, skipping
// filterAssignedWorkBeadsForPoolDemand — the very step that drops the row for a
// rig-scoped pool agent — so it passed green while production drained the session.
// It now drives that filter and asserts the row SURVIVES, so it fails before the
// assignedWorkIndexReachableFromAgent fix and passes after.
func TestPreserveTierDoesNotDrainMidDoSession(t *testing.T) {
	cityPath := tbHookGraphCity(t)
	tbSeedClaimedPoolRow(t, cityPath) // "hello" claimed by worker-a, routed to tbHookRoute

	var (
		stderr    bytes.Buffer
		workBeads []beads.Bead
		stores    []beads.Store
		refs      []string
	)
	appendTierBAssignedWork(cityPath, &workBeads, &stores, &refs, &stderr)

	// The append is read-only: it never writes a fold-owned row.
	if strings.Contains(stderr.String(), "fold-owned") || strings.Contains(stderr.String(), "write-closed") {
		t.Fatalf("append wrote to the write-closed journal: %q", stderr.String())
	}
	// The three index-aligned slices grew together with the claimed row.
	if len(workBeads) != 1 || len(stores) != 1 || len(refs) != 1 {
		t.Fatalf("appended slices misaligned: beads=%d stores=%d refs=%d", len(workBeads), len(stores), len(refs))
	}
	if workBeads[0].ID != "hello" || workBeads[0].Status != "in_progress" {
		t.Fatalf("appended bead = %+v, want the in_progress hello row", workBeads[0])
	}
	if refs[0] != tierBHookStoreName {
		t.Fatalf("store ref = %q, want %q (distinct journal ref)", refs[0], tierBHookStoreName)
	}

	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "rig", intPtr(2), 0)}}
	sessions := sessionInfosFromBeads([]beads.Bead{tbPreserveWorkerSessionBead()})

	// P1 REGRESSION PIN (mandatory): the claimed fold row must SURVIVE the real
	// production pool-demand filter. Its journal store ref (graph-journal) is not
	// the rig-scoped agent's configured rig, so before the reachability fix the
	// non-city branch of assignedWorkIndexReachableFromAgent drops it here — the
	// row never reaches poolWorkBeads / the resume tier / drain-suppression, and
	// the mid-do worker is treated as idle and DRAINED. After the fix it survives.
	filtered := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, sessions, workBeads, refs)
	if len(filtered) != 1 || filtered[0].ID != "hello" {
		t.Fatalf("pool-demand filter dropped the claimed Tier-B row (the DRAIN bug): filtered=%+v", filtered)
	}

	// The surviving row drives the resume tier: the mid-do session stays alive.
	result := ComputePoolDesiredStates(cfg, filtered, sessions, nil)
	if len(result) != 1 || len(result[0].Requests) == 0 {
		t.Fatalf("desired states = %+v, want a request for the claimed row", result)
	}
	if result[0].Requests[0].Tier != "resume" {
		t.Fatalf("preserve tier = %q, want resume (the mid-do session must not be drained)", result[0].Requests[0].Tier)
	}

	// The journal row is untouched by the preserve read.
	if st := tbHookNodeStatus(t, cityPath); st != "in_progress" {
		t.Fatalf("hello status after preserve append = %q, want in_progress (untouched)", st)
	}
}

// TestBuildDesiredStatePreservesMidDoLumenSessionWithoutFoldWrite drives the REAL
// buildDesiredState end-to-end against a graph-scoped city holding a claimed fold
// row plus the open worker session that claimed it. It pins the load-bearing S11
// insertion ORDER: appendTierBAssignedWork must run AFTER stampRunSessionIdentity /
// canonicalizeLegacyBoundAssignedWork (which WRITE the in-progress beads they see)
// and before the pool-demand consumer. If the append moved ahead of those writers,
// they would call SetMetadataBatch on the write-closed fold row every tick and log
// ErrFoldOwnedWriteClosed — the presence of the matching worker-a session is what
// gives this pin teeth (stampRunSessionIdentity only writes a bead with a resolved
// session). A unit test on the append alone cannot catch that reordering.
func TestBuildDesiredStatePreservesMidDoLumenSessionWithoutFoldWrite(t *testing.T) {
	cityPath := tbHookGraphCity(t)
	tbSeedClaimedPoolRow(t, cityPath)

	store := beads.NewMemStore()
	if _, err := store.Create(tbPreserveWorkerSessionBead()); err != nil {
		t.Fatalf("create worker session bead: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{poolAgent("claude", "rig", intPtr(2), 0)},
	}
	var stderr bytes.Buffer
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)

	// (a) No fold-owned write: the append lands after the write-closed-hostile
	// writers, so neither ever touches the journal row.
	if strings.Contains(stderr.String(), "fold-owned") {
		t.Fatalf("buildDesiredState wrote to a fold-owned row (S11 append ordering regressed): %q", stderr.String())
	}
	// (b) The claimed fold row reached the assigned-work snapshot in situ.
	found := false
	for _, wb := range dsResult.AssignedWorkBeads {
		if wb.ID == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("claimed Tier-B row absent from AssignedWorkBeads (append did not run in buildDesiredState): %+v", dsResult.AssignedWorkBeads)
	}
}
