package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestClassFederatedSessionStore_OwnerRoutesByID reproduces the split-store session seam:
// a session-lifecycle bead physically lives on the GRAPH store (a wisp-marked pool-agent
// session routed there by class) while the session handle's primary is the WORK store.
// Every by-id op the reconciler performs (Get + preWakeCommit's SetMetadataBatch +
// session_key/instance_token SetMetadata + Update + Close) must owner-route to the graph
// store, not fail "bead not found" on the work store.
func TestClassFederatedSessionStore_OwnerRoutesByID(t *testing.T) {
	work := beads.NewMemStore()
	graph, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() {
		if c, ok := graph.(interface{ CloseStore() error }); ok {
			_ = c.CloseStore()
		}
	})
	// sessions stay on the work store; graph is relocated — the reported topology.
	store := newClassFederatedSessionStore(work /*session==work*/, graph, work)
	if _, ok := store.(classFederatedSessionStore); !ok {
		t.Fatalf("expected the owner-routing wrapper for a split topology, got %T", store)
	}

	// The pool-agent session bead lives on the graph store (class routing put it there).
	sb, err := graph.Create(beads.Bead{Type: "session", Title: "pool session", Labels: []string{"gc:session"}})
	if err != nil {
		t.Fatalf("create session bead on graph store: %v", err)
	}

	got, err := store.Get(sb.ID)
	if err != nil || got.ID != sb.ID {
		t.Fatalf("Get(%s) = (%v, %v), want it resolved via the graph store", sb.ID, got.ID, err)
	}
	if err := store.SetMetadataBatch(sb.ID, map[string]string{"generation": "1", "instance_token": "tok"}); err != nil {
		t.Fatalf("SetMetadataBatch must owner-route to the graph store: %v", err)
	}
	if err := store.SetMetadata(sb.ID, "session_key", "k"); err != nil {
		t.Fatalf("SetMetadata must owner-route to the graph store: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(sb.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("Update must owner-route to the graph store: %v", err)
	}
	// The writes landed on the graph store, readable back through the federated Get.
	back, err := store.Get(sb.ID)
	if err != nil {
		t.Fatalf("Get after writes: %v", err)
	}
	if back.Metadata["session_key"] != "k" || back.Metadata["instance_token"] != "tok" {
		t.Fatalf("owner-routed writes not visible: metadata=%v", back.Metadata)
	}
	if err := store.Close(sb.ID); err != nil {
		t.Fatalf("Close must owner-route to the graph store: %v", err)
	}

	// A brand-new bead (Create) goes to the primary (work) store and still resolves.
	nb, err := store.Create(beads.Bead{Type: "session", Title: "named session"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := work.Get(nb.ID); err != nil {
		t.Fatalf("Create must land on the primary (work) store; work.Get: %v", err)
	}
	if _, err := store.Get(nb.ID); err != nil {
		t.Fatalf("federated Get of the new primary-store bead: %v", err)
	}
}

// TestClassFederatedSessionStore_ByteIdenticalSingleStore proves the wrapper is bypassed
// when every candidate store is the same physical store (default bd backend).
func TestClassFederatedSessionStore_ByteIdenticalSingleStore(t *testing.T) {
	work := beads.NewMemStore()
	store := newClassFederatedSessionStore(work, work, work)
	if _, wrapped := store.(classFederatedSessionStore); wrapped {
		t.Fatal("single-store federation must return the bare store, not the wrapper")
	}
	if store != beads.Store(work) {
		t.Fatalf("single-store federation must return the exact primary handle; got %T", store)
	}
}
