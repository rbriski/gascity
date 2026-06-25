package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestNudgeBeadBornUnroutedAndReadyExcluded pins the two independent guards that
// keep a nudge-queue shadow (type=chore + gc:nudge) from ever being claimed as
// Ready work — both must hold for the nudge class to relocate off bd safely:
//
//  1. born-unrouted — the constructor never stamps any gc.routed_to* key, so the
//     bd-CLI claim oracle (which selects on gc.routed_to) never returns it; and
//  2. Ready-excluded — beads.IsReadyExcludedBead excludes it by the gc:nudge
//     label, so the Go-side Ready scan the relocated nudge SQLite store will use
//     also drops it (the latent leak this closes).
func TestNudgeBeadBornUnroutedAndReadyExcluded(t *testing.T) {
	store := beads.NewMemStore()
	item := queuedNudge{
		ID:        "nudge-unrouted",
		Agent:     "wendy.wendy",
		SessionID: "mc-ayq6xi",
		Source:    "session",
		Message:   "follow up",
		CreatedAt: time.Now().Add(-time.Minute).UTC(),
	}
	createdID, created, err := ensureQueuedNudgeBead(store, item)
	if err != nil {
		t.Fatalf("ensureQueuedNudgeBead: %v", err)
	}
	if !created {
		t.Fatal("expected ensureQueuedNudgeBead to create a backing nudge bead")
	}
	bead, err := store.Get(createdID)
	if err != nil {
		t.Fatalf("Get(%q): %v", createdID, err)
	}

	// Shape sanity: this is the nudge shadow (type=chore + gc:nudge).
	if bead.Type != nudgeBeadType {
		t.Fatalf("nudge bead type = %q, want %q", bead.Type, nudgeBeadType)
	}
	if !slicesContain(bead.Labels, nudgeBeadLabel) {
		t.Fatalf("nudge bead labels = %v, want to contain %q", bead.Labels, nudgeBeadLabel)
	}

	// (1) Born unrouted: none of the routing keys the claim oracle selects on.
	for _, key := range []string{
		beadmeta.RoutedToMetadataKey,
		beadmeta.ExecutionRoutedToMetadataKey,
		beadmeta.DeferredRoutedToMetadataKey,
		beadmeta.DeferredExecutionRoutedToMetadataKey,
	} {
		if v, ok := bead.Metadata[key]; ok {
			t.Fatalf("nudge bead carries routing metadata %q=%q; it must be born unrouted", key, v)
		}
	}
	for _, label := range bead.Labels {
		if strings.HasPrefix(label, "routed_to:") || strings.HasPrefix(label, beadmeta.RoutedToMetadataKey) {
			t.Fatalf("nudge bead carries routing label %q; it must be born unrouted", label)
		}
	}

	// (2) Ready-excluded by the gc:nudge label across every Go Ready scan.
	if !beads.IsReadyExcludedBead(bead) {
		t.Fatalf("nudge bead is not Ready-excluded: %+v", bead)
	}
	if beads.IsReadyCandidate(bead, time.Now()) {
		t.Fatalf("nudge bead is a Ready candidate; the gc:nudge exclusion did not apply: %+v", bead)
	}
}
