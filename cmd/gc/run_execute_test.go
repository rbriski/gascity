package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func newOpenRoot(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	root, err := store.Create(beads.Bead{ID: "root-1", Title: "workflow root", Status: "open", Type: "workflow"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	return root
}

func TestWatchWorkflowRootReturnsPassOnClose(t *testing.T) {
	store := beads.NewMemStore()
	root := newOpenRoot(t, store)
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = store.SetMetadata(root.ID, beadmeta.OutcomeMetadataKey, beadmeta.OutcomePass)
		_ = store.Close(root.ID)
	}()
	out, err := watchWorkflowRoot(context.Background(), store, root.ID, 5*time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Terminal || !out.Passed() {
		t.Errorf("want terminal+pass, got %+v", out)
	}
}

func TestWatchWorkflowRootReturnsFail(t *testing.T) {
	store := beads.NewMemStore()
	root := newOpenRoot(t, store)
	if err := store.SetMetadata(root.ID, beadmeta.OutcomeMetadataKey, beadmeta.OutcomeFail); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(root.ID); err != nil {
		t.Fatal(err)
	}
	out, err := watchWorkflowRoot(context.Background(), store, root.ID, 5*time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Terminal || out.Passed() {
		t.Errorf("want terminal + not-passed, got %+v", out)
	}
}

func TestWatchWorkflowRootDeadlineIsNonDestructive(t *testing.T) {
	store := beads.NewMemStore()
	root := newOpenRoot(t, store) // never closes
	out, err := watchWorkflowRoot(context.Background(), store, root.ID, 15*time.Millisecond, 60*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if out.Terminal {
		t.Errorf("deadline should yield a non-terminal result, got %+v", out)
	}
}

func TestWatchWorkflowRootContextCancel(t *testing.T) {
	store := beads.NewMemStore()
	root := newOpenRoot(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := watchWorkflowRoot(ctx, store, root.ID, 5*time.Millisecond, time.Second); err == nil {
		t.Error("want context-cancel error")
	}
}
